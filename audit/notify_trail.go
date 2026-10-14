package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// NotifyTrail adapts an EventStore to agent.NotifyTrail: the notification trail of a run is an
// event trail of its own (named agent.NotifyTrailName), each record a salted, canonical event leaf
// committed like any other, so EventLog, TreeHead, Prove and ProveConsistency work on it and bide
// audit can verify it. It is deliberately a different trail from the one PersistJournal mirrors
// for the run, whose positions that projection owns.
//
// Append is insert-if-absent at a position, on the store's contract: a different leaf at a stored
// position is an error. The adapter turns that error into agent.ErrTrailConflict, and recognises
// the caller's own earlier write (an Append whose reply was lost) so a retry is a no-op. It needs
// a durable EventStore with UNIQUE(run_id, seq) semantics to give two processes mutual exclusion.
type NotifyTrail struct {
	store EventStore
}

var (
	_ agent.NotifyTrail         = (*NotifyTrail)(nil)
	_ agent.NotifyTrailAnchorer = (*NotifyTrail)(nil)
)

// NewNotifyTrail returns a NotifyTrail over store.
func NewNotifyTrail(store EventStore) *NotifyTrail { return &NotifyTrail{store: store} }

// Load implements agent.NotifyTrail. It refuses a leaf that is not a notify record (a trail that
// is not a notification trail, or a record of a kind this version does not know is not skipped).
func (t *NotifyTrail) Load(ctx context.Context, trail string) ([]agent.NotifyRecord, error) {
	leaves, err := t.store.Load(ctx, trail)
	if err != nil {
		return nil, fmt.Errorf("audit: load notify trail %s: %w", trail, err)
	}
	recs := make([]agent.NotifyRecord, len(leaves))
	for i, leaf := range leaves {
		if recs[i], err = decodeNotifyLeaf(leaf); err != nil {
			return nil, fmt.Errorf("audit: notify trail %s record %d: %w", trail, i, err)
		}
	}
	return recs, nil
}

// Append implements agent.NotifyTrail.
func (t *NotifyTrail) Append(ctx context.Context, trail string, seq int, rec agent.NotifyRecord) error {
	rec = rec.Normalized()
	salt, err := newEventSalt()
	if err != nil {
		return err
	}
	leaf, err := canonicalEvent(rec, salt)
	if err != nil {
		return err
	}
	appendErr := t.store.Append(ctx, trail, seq, leaf)
	if appendErr == nil {
		return nil
	}
	// The store refused or the reply was lost. Reload to see which: our own record already at seq
	// (the write landed), a different record there (we lost the race), or neither (a real failure).
	leaves, err := t.store.Load(ctx, trail)
	if err != nil {
		return fmt.Errorf("audit: notify trail %s append at %d: %w (reload: %w)", trail, seq, appendErr, err)
	}
	if seq < len(leaves) {
		got, derr := decodeNotifyLeaf(leaves[seq])
		if derr != nil {
			return fmt.Errorf("audit: notify trail %s position %d: %w", trail, seq, derr)
		}
		if agent.SameNotifyRecord(got, rec) {
			return nil
		}
		return fmt.Errorf("%w: %s position %d (%w)", agent.ErrTrailConflict, trail, seq, appendErr)
	}
	return fmt.Errorf("audit: notify trail %s append at %d: %w", trail, seq, appendErr)
}

// AnchorTrails implements agent.NotifyTrailAnchorer: it asks the AuditedEventStore beneath the
// trail's store (found through Unwrap, under any decorator) to publish a head for each named trail
// that has grown past its last one, and for some that an earlier failure left unanchored. A trail
// over a store that anchors nothing has nothing to publish, and returns nil.
func (t *NotifyTrail) AnchorTrails(ctx context.Context, trails []string) error {
	var s EventStore = t.store
	for range 64 {
		if an, ok := s.(interface {
			AnchorTrails(context.Context, []string) error
		}); ok {
			return an.AnchorTrails(ctx, trails)
		}
		u, ok := s.(interface{ Unwrap() EventStore })
		if !ok {
			return nil
		}
		s = u.Unwrap()
	}
	return nil
}

// decodeNotifyLeaf reads a notify record from a stored event leaf. The leaf's kind must be the
// notify kind of the record it holds, so a leaf cannot say one thing and hold another.
func decodeNotifyLeaf(leaf []byte) (agent.NotifyRecord, error) {
	body, ok := bytes.CutPrefix(leaf, []byte(eventLeafTag))
	if !ok {
		return agent.NotifyRecord{}, errors.New("not an event leaf")
	}
	var el eventLeaf
	if err := json.Unmarshal(body, &el); err != nil {
		return agent.NotifyRecord{}, err
	}
	kind, isNotify := strings.CutPrefix(el.Kind, "notify_")
	if !isNotify {
		return agent.NotifyRecord{}, fmt.Errorf("leaf kind %q is not a notify record", el.Kind)
	}
	var rec agent.NotifyRecord
	if err := json.Unmarshal(el.Event, &rec); err != nil {
		return agent.NotifyRecord{}, err
	}
	if string(rec.Kind) != kind {
		return agent.NotifyRecord{}, fmt.Errorf("leaf kind %q holds a %q record", el.Kind, rec.Kind)
	}
	return rec, nil
}
