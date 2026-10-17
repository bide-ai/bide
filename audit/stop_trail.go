package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// StopTrail adapts an EventStore to agent.StopTrail: the stop trail of a deployment is an event trail of
// its own (named agent.StopTrailName), each signed stop record a salted, canonical event leaf committed
// like any other, so EventLog, TreeHead, Prove and ProveConsistency work on it, an AuditedEventStore
// signs and anchors its heads, and bide audit stops can verify it. Append is insert-if-absent at a
// position, as NotifyTrail's is: a different leaf at a stored position is agent.ErrTrailConflict, and a
// caller's own earlier write (a reply that was lost) is recognised, so a retry is a no-op.
type StopTrail struct {
	store EventStore
}

var (
	_ agent.StopTrail = (*StopTrail)(nil)
)

// NewStopTrail returns a StopTrail over store.
func NewStopTrail(store EventStore) *StopTrail { return &StopTrail{store: store} }

// Load implements agent.StopTrail. It refuses a leaf that is not a stop record.
func (t *StopTrail) Load(ctx context.Context, trail string) ([]agent.StopRecord, error) {
	leaves, err := t.store.Load(ctx, trail)
	if err != nil {
		return nil, fmt.Errorf("audit: load stop trail %s: %w", trail, err)
	}
	recs := make([]agent.StopRecord, len(leaves))
	for i, leaf := range leaves {
		if recs[i], err = decodeStopLeaf(leaf); err != nil {
			return nil, fmt.Errorf("audit: stop trail %s record %d: %w", trail, i, err)
		}
	}
	return recs, nil
}

// Append implements agent.StopTrail.
func (t *StopTrail) Append(ctx context.Context, trail string, seq int, rec agent.StopRecord) error {
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
	leaves, err := t.store.Load(ctx, trail)
	if err != nil {
		return fmt.Errorf("audit: stop trail %s append at %d: %w (reload: %w)", trail, seq, appendErr, err)
	}
	if seq < len(leaves) {
		got, derr := decodeStopLeaf(leaves[seq])
		if derr != nil {
			return fmt.Errorf("audit: stop trail %s position %d: %w", trail, seq, derr)
		}
		if agent.SameStopRecord(got, rec) {
			return nil
		}
		return fmt.Errorf("%w: %s position %d (%w)", agent.ErrTrailConflict, trail, seq, appendErr)
	}
	return fmt.Errorf("audit: stop trail %s append at %d: %w", trail, seq, appendErr)
}

// AnchorTrails asks the AuditedEventStore beneath the trail's store (found through Unwrap) to publish a
// head for each named trail that has grown past its last one. A trail over a store that anchors nothing
// has nothing to publish, and returns nil.
func (t *StopTrail) AnchorTrails(ctx context.Context, trails []string) error {
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

// decodeStopLeaf reads a stop record from a stored event leaf. The leaf's kind must be the stop kind of
// the record it holds, so a leaf cannot say one thing and hold another.
func decodeStopLeaf(leaf []byte) (agent.StopRecord, error) {
	body, ok := bytes.CutPrefix(leaf, []byte(eventLeafTag))
	if !ok {
		return agent.StopRecord{}, errors.New("not an event leaf")
	}
	var el eventLeaf
	if err := json.Unmarshal(body, &el); err != nil {
		return agent.StopRecord{}, err
	}
	kind, isStop := strings.CutPrefix(el.Kind, "stop_")
	if !isStop {
		return agent.StopRecord{}, fmt.Errorf("leaf kind %q is not a stop record", el.Kind)
	}
	var rec agent.StopRecord
	dec := json.NewDecoder(bytes.NewReader(el.Event))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return agent.StopRecord{}, err
	}
	if rec.Kind != kind {
		return agent.StopRecord{}, fmt.Errorf("leaf kind %q holds a %q record", el.Kind, rec.Kind)
	}
	return rec, nil
}

// StopTrailExportFormat is the format of a StopTrailExport.
const StopTrailExportFormat = "bide.audit.stop-trail-export.v1"

// A StopTrailExport is the stop trail as the leaves its EventStore holds, in position order: what
// VerifyStops and bide audit stops read when they hold no store. Each leaf is carried verbatim, so a
// signed head over the trail (SignStopTrail) commits to exactly the bytes exported.
type StopTrailExport struct {
	Format string   `json:"format"` // StopTrailExportFormat
	Trail  string   `json:"trail"`  // agent.StopTrailName
	Leaves [][]byte `json:"leaves"`
}

func (StopTrailExport) artifactFormat() (string, string) {
	return "stop trail export", StopTrailExportFormat
}

// ExportStopTrail returns the trail named trail in store as a StopTrailExport. A trail the store does not
// hold exports with no leaves.
func ExportStopTrail(ctx context.Context, store EventStore, trail string) (StopTrailExport, error) {
	leaves, err := store.Load(ctx, trail)
	if err != nil {
		return StopTrailExport{}, fmt.Errorf("audit: export stop trail %s: %w", trail, err)
	}
	if leaves == nil {
		leaves = [][]byte{}
	}
	return StopTrailExport{Format: StopTrailExportFormat, Trail: trail, Leaves: leaves}, nil
}

// SignStopTrail signs a tree head over the leaves store holds for trail, at now: the head
// AuditedEventStore publishes as records arrive, for a deployment that signs on its own schedule.
func SignStopTrail(ctx context.Context, store EventStore, trail string, s Signer, now time.Time) (SignedTreeHead, error) {
	log, err := LoadEventLog(ctx, store, trail)
	if err != nil {
		return SignedTreeHead{}, err
	}
	return SignTreeHead(log.TreeHead(trail, now.UnixNano()), s)
}

// StopVerifierFromAudit builds the verifier of a policy document's approver with this package's
// verifiers (agent.StopVerifierFunc): pass it to agent.WithStopVerifier, or to agent.PolicyFromDoc.
func StopVerifierFromAudit(alg agent.Alg, pub []byte) (agent.ApproverVerifier, error) {
	v, err := NewVerifier(alg, pub)
	if err != nil {
		return nil, err
	}
	av, ok := v.(agent.ApproverVerifier)
	if !ok {
		return nil, fmt.Errorf("audit: the %s verifier does not verify approvals: %w", alg, ErrMalformed)
	}
	return av, nil
}

// StopPolicyFromDoc builds the stop policy a document names, with this package's verifiers, and
// validates it.
func StopPolicyFromDoc(d agent.StopPolicyDoc) (agent.StopPolicy, error) {
	return agent.PolicyFromDoc(d, StopVerifierFromAudit)
}
