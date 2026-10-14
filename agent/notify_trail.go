package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// ErrTrailConflict is returned by NotifyTrail.Append when the position is already taken by a
// different record: another process won the append. The loser reloads, folds, and decides again.
var ErrTrailConflict = errors.New("agent: notify trail position taken by a different record")

// NotifyTrail is the port for the notification trail: a named, append-only sequence of
// NotifyRecords with insert-if-absent at a position. audit.NewNotifyTrail implements it over an
// audit.EventStore, so the trail is salted, Merkle-committed and verifiable by bide audit; package
// agent cannot import audit. MemNotifyTrail is the in-memory implementation for tests and local
// development.
//
// The position is what makes a claim atomic: a process loads the trail, decides from the fold,
// and appends at len(loaded). If any other process appended meanwhile, the position is taken, the
// append returns ErrTrailConflict, and the decision is made again from a fresh load.
type NotifyTrail interface {
	// Load returns trail's records in append order, nil if it has none.
	Load(ctx context.Context, trail string) ([]NotifyRecord, error)
	// Append stores rec at position seq (from 0) of trail. seq equal to the trail's length appends.
	// A seq below it holding an equal record is a no-op (nil): the caller's own earlier append
	// landed although it saw an error. A seq below it holding a different record is
	// ErrTrailConflict. A seq above the length is an error, not a conflict.
	Append(ctx context.Context, trail string, seq int, rec NotifyRecord) error
}

// NotifyTrailAnchorer is an optional capability of a NotifyTrail: one whose records are signed and
// anchored as they are written (audit.NotifyTrail over an audit.AuditedEventStore) can also be asked
// to catch up. The notifier calls AnchorTrails with the trails of the runs a NotifyPending sweep or
// a recovery pass looked at, so a trail that grew while the anchor was unreachable, or whose writer
// died before it anchored, is signed again without waiting for a new event. It is best effort: an
// error is reported to the handler of WithNotifyErrors and never affects a delivery, and it must be
// bounded (it is called inline by the sweep and the recovery pass). A NotifyTrail that does not
// implement it is simply not asked.
type NotifyTrailAnchorer interface {
	AnchorTrails(ctx context.Context, trails []string) error
}

// anchorTrails asks the engine's trail, if it can, to bring the heads of the named trails up to
// date. A failure is reported, never returned: anchoring is a side channel of delivery.
func (e *notifyEngine) anchorTrails(ctx context.Context, trails ...string) {
	a, ok := e.trail.(NotifyTrailAnchorer)
	if !ok || ctx.Err() != nil {
		return
	}
	trails = slices.Compact(slices.Sorted(slices.Values(trails)))
	if err := a.AnchorTrails(ctx, trails); err != nil {
		e.report(fmt.Errorf("notify: anchor trails: %w", err))
	}
}

// recentSegments is how many of the open trail's newest segments a catch-up signs again besides
// today's: a sealed segment is final, so its last head is the one that matters, and a few cover a
// process that was down for a while.
const recentSegments = 3

// globalEvery is how often a recovery pass signs the global trails again: a pass visits every
// unfinished run, and they need it once per interval, not once per run.
const globalEvery = time.Minute

// globalDue reports whether the global trails are due to be signed again by a recovery pass, and
// if so books the next time.
func (e *notifyEngine) globalDue() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.cfg.now()
	if !e.globalAt.IsZero() && now.Sub(e.globalAt) < globalEvery {
		return false
	}
	e.globalAt = now
	return true
}

// globalTrails are the trails that belong to no run and that a catch-up signs again: the digest's,
// the open trail's index for this year and the last, today's open segment and the newest few
// before it. Reading the indexes is best effort: an error is reported and the names found so far
// are used.
func (e *notifyEngine) globalTrails(ctx context.Context) []string {
	now := e.cfg.now()
	names := []string{NotifyDigestTrail, OpenSegment(now)}
	var segs []string
	for _, yr := range []int{now.UTC().Year() - 1, now.UTC().Year()} {
		idx := OpenIndex(yr)
		recs, err := e.trail.Load(ctx, idx)
		if err != nil {
			e.report(fmt.Errorf("notify: read %s to anchor the open trail: %w", idx, err))
			continue
		}
		if len(recs) > 0 {
			names = append(names, idx)
		}
		for _, r := range recs {
			if r.Kind == NotifySegment && !slices.Contains(segs, r.Next) {
				segs = append(segs, r.Next)
			}
		}
	}
	return append(names, segs[max(0, len(segs)-recentSegments):]...)
}

// MemNotifyTrail is an in-memory NotifyTrail, safe for concurrent use.
type MemNotifyTrail struct {
	mu     sync.Mutex
	trails map[string][]NotifyRecord
}

// NewMemNotifyTrail returns an empty in-memory trail store.
func NewMemNotifyTrail() *MemNotifyTrail { return &MemNotifyTrail{trails: map[string][]NotifyRecord{}} }

var _ NotifyTrail = (*MemNotifyTrail)(nil)

// Load implements NotifyTrail.
func (m *MemNotifyTrail) Load(_ context.Context, trail string) ([]NotifyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]NotifyRecord(nil), m.trails[trail]...), nil
}

// Append implements NotifyTrail.
func (m *MemNotifyTrail) Append(_ context.Context, trail string, seq int, rec NotifyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.trails[trail]
	switch {
	case seq == len(cur):
		m.trails[trail] = append(cur, rec.Normalized())
		return nil
	case seq < len(cur):
		if SameNotifyRecord(cur[seq], rec) {
			return nil
		}
		return fmt.Errorf("%w: %s position %d", ErrTrailConflict, trail, seq)
	default:
		return fmt.Errorf("agent: notify trail %s: position %d is past the end (%d records)", trail, seq, len(cur))
	}
}

// SameNotifyRecord reports whether a and b are the same record: the same canonical JSON, so times
// compare by instant and not by location or monotonic clock reading.
func SameNotifyRecord(a, b NotifyRecord) bool {
	ab, err1 := json.Marshal(a.Normalized())
	bb, err2 := json.Marshal(b.Normalized())
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}
