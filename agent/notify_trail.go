package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
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
