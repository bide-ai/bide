package audit

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/bide-ai/bide/agent"
)

// This file is the bring-your-own port for DURABLE event-trail persistence. The journal is
// the resume substrate and may be garbage-collected after a run; a compliance trail often has
// to outlive it (keep 7 years, on WORM storage, in a different trust domain). An EventStore
// lets you mirror the run's committed event trail into a backend YOU run, on ITS own
// retention lifecycle, and rebuild an EventLog from it later for Root / STH / proofs — without
// the journal still being around.
//
// The trail is fed from the DURABLE journal projection (agent.ReplayEvents), not the live
// stream, on purpose: the projection is a deterministic function of the persisted records, so
// re-mirroring after a crash appends the SAME leaves at the SAME positions (idempotent), never
// forking. A live stream re-emits replayed events with different flags across a crash and would
// fork the trail — so PersistJournal is the supported path.

// EventStore is the bring-your-own port: append canonical event leaves to your append-only
// backend (a Postgres table with UNIQUE(run_id, seq) and insert-only grants, object storage
// with object-lock/WORM, or a log) and read them back. Bide ships MemEventStore as the
// in-memory default; you implement Append/Load against infrastructure you already run. A leaf
// holds its event's salt (see EventInclusion), so a store must keep the bytes verbatim: a proof
// built from the reloaded trail discloses the salt from there.
type EventStore interface {
	// Append durably records leaf at position seq (0-based, contiguous) for runID. It MUST be
	// append-only and idempotent on (runID, seq): re-appending an already-stored seq with the
	// SAME bytes is a no-op (so a retried mirror is safe), while a DIFFERENT leaf at an
	// existing seq must error (a fork / tamper attempt). A seq beyond the next position is a
	// gap and must error.
	Append(ctx context.Context, runID string, seq int, leaf []byte) error
	// Load returns every leaf for runID in seq order (0..n-1), or nil if the run is unknown.
	Load(ctx context.Context, runID string) ([][]byte, error)
}

// PersistJournal mirrors runID's durable event trail into evStore by projecting the journal
// (agent.ReplayEvents) and appending each canonical event leaf at its position. It is
// IDEMPOTENT: call it after each turn, once at run end, or on a schedule — re-runs append only
// what is new and never fork the trail, because the projection is deterministic and
// resume-stable. After it returns, LoadEventLog(evStore, runID) reconstructs the committed
// trail for anchoring/proofs even if the journal is later deleted.
//
// Each leaf commits to its event's salt, derived from the salt of the journal record the event
// projects (see EventLogFromJournal), so a re-run computes the same leaf bytes and a stored leaf
// carries the salt its proof discloses. It errors if a source record has no agent.SaltSize salt.
func PersistJournal(ctx context.Context, evStore EventStore, jStore agent.Durable, runID string) error {
	evs, salts, err := projectJournal(ctx, jStore, runID)
	if err != nil {
		return err
	}
	for seq, e := range evs {
		leaf, err := canonicalEvent(e, salts[seq])
		if err != nil {
			return err
		}
		if err := evStore.Append(ctx, runID, seq, leaf); err != nil {
			return fmt.Errorf("audit: persist event %d of run %s: %w", seq, runID, err)
		}
	}
	return nil
}

// PersistEvent appends one event's canonical leaf to evStore at position seq, for callers
// streaming events into a store directly; prefer PersistJournal for the crash-safe path. The
// leaf commits to a fresh random salt (agent.SaltSize bytes from crypto/rand), which is stored
// in the leaf. If seq is already stored, the event is salted with the stored leaf's salt, so a
// retry of the same event is a no-op and a different event is still refused as a fork. Do not
// mix it with PersistJournal on one run: the two salt the same event differently, so the second
// reports a fork.
func PersistEvent(ctx context.Context, evStore EventStore, runID string, seq int, e agent.AgentEvent) error {
	stored, err := evStore.Load(ctx, runID)
	if err != nil {
		return fmt.Errorf("audit: load event trail %s: %w", runID, err)
	}
	var salt []byte
	if seq >= 0 && seq < len(stored) {
		if salt, err = eventLeafSalt(stored[seq]); err != nil {
			return fmt.Errorf("audit: event %d of run %s: %w", seq, runID, err)
		}
	} else if salt, err = newEventSalt(); err != nil {
		return err
	}
	leaf, err := canonicalEvent(e, salt)
	if err != nil {
		return err
	}
	return evStore.Append(ctx, runID, seq, leaf)
}

// LoadEventLog rebuilds an EventLog from evStore's persisted leaves for runID, ready for
// Root / Head / TreeHead / Prove / ProveConsistency — from the store alone, no journal needed.
// The leaves were canonicalized, salts included, when stored, so inclusion proofs verify against
// the original events exactly as if the log had been built live. It refuses a leaf that is not a
// salted bide.audit.event-leaf.v2 leaf: its proof would have no salt to disclose.
func LoadEventLog(ctx context.Context, evStore EventStore, runID string) (*EventLog, error) {
	leaves, err := evStore.Load(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("audit: load event trail %s: %w", runID, err)
	}
	salts := make([][]byte, len(leaves))
	for i, leaf := range leaves {
		if salts[i], err = eventLeafSalt(leaf); err != nil {
			return nil, fmt.Errorf("audit: event %d of run %s: %w", i, runID, err)
		}
	}
	return &EventLog{leaves: leaves, salts: salts}, nil
}

// MemEventStore is the in-memory reference EventStore for tests and local dev. A real backend
// (Postgres, object storage) enforces the same append-only/idempotent contract with a unique
// constraint and insert-only permissions.
type MemEventStore struct {
	mu   sync.Mutex
	runs map[string][][]byte
}

// NewMemEventStore returns an empty in-memory EventStore.
func NewMemEventStore() *MemEventStore { return &MemEventStore{runs: map[string][][]byte{}} }

var _ EventStore = (*MemEventStore)(nil) // port/adapter contract

func (s *MemEventStore) Append(_ context.Context, runID string, seq int, leaf []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.runs[runID]
	switch {
	case seq == len(cur): // the next position — append
		s.runs[runID] = append(cur, append([]byte(nil), leaf...))
		return nil
	case seq < len(cur): // already stored — must be byte-identical (idempotent replay)
		if !bytes.Equal(cur[seq], leaf) {
			return fmt.Errorf("audit: event %d of run %s already stored with different bytes (fork/tamper)", seq, runID)
		}
		return nil
	default: // seq > len(cur) — a gap; the trail must stay contiguous
		return fmt.Errorf("audit: event %d of run %s is non-contiguous (have %d)", seq, runID, len(cur))
	}
}

func (s *MemEventStore) Load(_ context.Context, runID string) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.runs[runID]
	if cur == nil {
		return nil, nil
	}
	out := make([][]byte, len(cur))
	for i, l := range cur {
		out[i] = append([]byte(nil), l...)
	}
	return out, nil
}
