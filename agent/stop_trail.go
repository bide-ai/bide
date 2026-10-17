package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// StopTrailName is the name of the audit trail of a deployment's stops: an append-only sequence of the
// control run's signed records, committed, signed and anchored by the audit package (audit.StopTrail
// over an audit.AuditedEventStore). The control run is what enforces a stop; the trail is its evidence.
const StopTrailName = "stop"

// StopTrail is the port of the stop trail: a named, append-only sequence of StopRecords with
// insert-if-absent at a position, as NotifyTrail is for notifications. audit.NewStopTrail implements it
// over an audit.EventStore, so each record is a salted, Merkle-committed event leaf that bide audit stops
// can verify; MemStopTrail is the in-memory one for tests.
type StopTrail interface {
	// Load returns the trail's records in append order, nil if it has none.
	Load(ctx context.Context, trail string) ([]StopRecord, error)
	// Append stores rec at position seq (from 0) of trail. seq equal to the trail's length appends. A
	// seq below it holding an equal record is a no-op (nil); one holding a different record is
	// ErrTrailConflict; one above the length is an error, not a conflict.
	Append(ctx context.Context, trail string, seq int, rec StopRecord) error
}

// WithStopTrail gives an audited control the trail its records are also written to. The order is what
// keeps the evidence at least as strong as the effect: a stop is written to the control run first (so it
// is effective at once) and to the trail second, and every other record (a lift, a policy transition,
// the root, a mode change) is written to the trail first and to the control run second, so none is
// effective without its evidence. A failed trail write is a *StopTrailError that says which half
// landed; Reaudit completes a stop whose trail write failed. A basic control keeps no trail (ErrConfig).
func WithStopTrail(t StopTrail) StopOption {
	return stopOption(func(c *StopControl) error {
		if t == nil {
			return fmt.Errorf("WithStopTrail: nil trail: %w", ErrConfig)
		}
		c.trail = t
		return nil
	})
}

// StopTrailError is returned when a record could not be written to the stop trail. For a stop,
// StoreWritten is true: the stop is in force (the control run has it) and only its evidence is missing;
// Reaudit appends it. For any other record the control run was not written, so it has no effect.
type StopTrailError struct {
	Record       string // the record's name in the control run
	StoreWritten bool
	Err          error
}

func (e *StopTrailError) Error() string {
	if e.StoreWritten {
		return fmt.Sprintf("stop record %s is in force, but writing it to the stop trail failed (Reaudit completes it): %v", e.Record, e.Err)
	}
	return fmt.Sprintf("stop record %s was not written: the stop trail could not take it first: %v", e.Record, e.Err)
}

func (e *StopTrailError) Unwrap() error { return e.Err }

// agentEvent lets the audit package commit a StopRecord as an event leaf (as it does a NotifyRecord);
// a run's stream never emits one.
func (StopRecord) agentEvent() {}

// SameStopRecord reports whether a and b are the same record: the same canonical bytes, signature included.
func SameStopRecord(a, b StopRecord) bool { return StopDigest(a) == StopDigest(b) }

// maxStopTrailTries bounds how often an append retries after losing a race for its position.
const maxStopTrailTries = 8

// appendTrail appends rec to the trail unless it is already there, retrying at the next position when
// another writer took the one it tried.
func (c *StopControl) appendTrail(ctx context.Context, rec StopRecord) error {
	for range maxStopTrailTries {
		have, err := c.trail.Load(ctx, StopTrailName)
		if err != nil {
			return err
		}
		for _, h := range have {
			if SameStopRecord(h, rec) {
				return nil
			}
		}
		if err := c.trail.Append(ctx, StopTrailName, len(have), rec); err == nil {
			return nil
		} else if !errors.Is(err, ErrTrailConflict) {
			return err
		}
	}
	return fmt.Errorf("stop trail: no position after %d tries: %w", maxStopTrailTries, ErrTrailConflict)
}

// StopRecords returns the control run's decodable records, in store order: what the trail is
// compared with.
func (c *StopControl) StopRecords(ctx context.Context) ([]StopRecord, error) {
	recs, err := c.j.History(ctx, StopControlRun)
	if err != nil {
		return nil, err
	}
	var out []StopRecord
	for _, rec := range recs {
		if rec.Kind != StepValue || !strings.HasPrefix(rec.Name, "@stop/") {
			continue
		}
		if r, err := decodeStopRecord(rec.Result); err == nil && stopRecordStep(r) == rec.Name {
			out = append(out, r)
		}
	}
	return out, nil
}

// DiffStopRecords returns the records of store that are not in trail, and those of trail that are not in
// store, by content. A record in the store only is in force (or ignored) without evidence; one in the
// trail only was recorded but is not in the control run (deleted, or a lift that never took effect).
func DiffStopRecords(store, trail []StopRecord) (storeOnly, trailOnly []StopRecord) {
	has := func(set []StopRecord, r StopRecord) bool {
		for _, s := range set {
			if SameStopRecord(s, r) {
				return true
			}
		}
		return false
	}
	for _, r := range store {
		if !has(trail, r) {
			storeOnly = append(storeOnly, r)
		}
	}
	for _, r := range trail {
		if !has(store, r) {
			trailOnly = append(trailOnly, r)
		}
	}
	return storeOnly, trailOnly
}

// Reaudit appends to the trail every record of the control run that it lacks, in store order, and
// returns how many it appended: how a stop whose trail write failed (a *StopTrailError) is completed, and
// a trail that fell behind is caught up. It needs a control with a trail.
func (c *StopControl) Reaudit(ctx context.Context) (int, error) {
	if c.trail == nil {
		return 0, fmt.Errorf("Reaudit needs a stop trail (WithStopTrail): %w", ErrConfig)
	}
	recs, err := c.StopRecords(ctx)
	if err != nil {
		return 0, err
	}
	have, err := c.trail.Load(ctx, StopTrailName)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range recs {
		if present := func() bool {
			for _, h := range have {
				if SameStopRecord(h, r) {
					return true
				}
			}
			return false
		}(); present {
			continue
		}
		if err := c.appendTrail(ctx, r); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// VerifyStopRecords folds records (a control run's, or a stop trail's) under the policy the options give,
// with no store: what bide audit stops does to a trail. It returns the same StopState as
// StopControl.State, so the stops in force, the records rejected and the chain's condition are read the
// same way. recs are folded in the order given.
func VerifyStopRecords(recs []StopRecord, opts ...StopOption) (StopState, error) {
	c := &StopControl{now: time.Now}
	for _, o := range opts {
		if o == nil {
			return StopState{}, fmt.Errorf("VerifyStopRecords: nil option: %w", ErrConfig)
		}
		if err := o.applyStop(c); err != nil {
			return StopState{}, err
		}
	}
	if c.mode == "" {
		c.mode = StopBasic
		if c.policy != nil {
			c.mode = StopAudited
		}
	}
	if c.mode == StopAudited && c.policy == nil {
		return StopState{}, fmt.Errorf("VerifyStopRecords: audited mode needs WithStopPolicy: %w", ErrConfig)
	}
	var journal []Record
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			return StopState{}, fmt.Errorf("encode a stop record: %w", err)
		}
		journal = append(journal, Record{Kind: StepValue, Name: stopRecordStep(r), Result: b})
	}
	return c.stateOf(journal)
}

// MemStopTrail is an in-memory StopTrail, safe for concurrent use.
type MemStopTrail struct {
	mu     sync.Mutex
	trails map[string][]StopRecord
}

// NewMemStopTrail returns an empty in-memory stop trail.
func NewMemStopTrail() *MemStopTrail { return &MemStopTrail{trails: map[string][]StopRecord{}} }

var _ StopTrail = (*MemStopTrail)(nil)

// Load implements StopTrail.
func (m *MemStopTrail) Load(_ context.Context, trail string) ([]StopRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]StopRecord(nil), m.trails[trail]...), nil
}

// Append implements StopTrail.
func (m *MemStopTrail) Append(_ context.Context, trail string, seq int, rec StopRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.trails[trail]
	switch {
	case seq == len(cur):
		m.trails[trail] = append(cur, rec)
		return nil
	case seq < len(cur):
		if SameStopRecord(cur[seq], rec) {
			return nil
		}
		return fmt.Errorf("%w: %s position %d", ErrTrailConflict, trail, seq)
	default:
		return fmt.Errorf("agent: stop trail %s: position %d is past the end (%d records)", trail, seq, len(cur))
	}
}
