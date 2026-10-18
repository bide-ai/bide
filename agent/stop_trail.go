package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
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

// Error implements error.
func (e *StopTrailError) Error() string {
	if e.StoreWritten {
		return fmt.Sprintf("stop record %s is in force, but writing it to the stop trail failed (Reaudit completes it): %v", e.Record, e.Err)
	}
	return fmt.Sprintf("stop record %s was not written: the stop trail could not take it first: %v", e.Record, e.Err)
}

// Unwrap returns the trail's error.
func (e *StopTrailError) Unwrap() error { return e.Err }

// agentEvent lets the audit package commit a StopRecord as an event leaf (as it does a NotifyRecord);
// a run's stream never emits one.
func (StopRecord) agentEvent() {}

// SameStopRecord reports whether a and b are the same record: the same canonical bytes, signature included.
func SameStopRecord(a, b StopRecord) bool { return StopDigest(a) == StopDigest(b) }

// stopTrailBudget bounds the trail writes of one call that has no deadline of its own (a lift with its catch-up
// records, a Reaudit): how long they keep retrying after losing races for a position, all together. A caller's
// earlier deadline wins. stopSetTrailBudget is the shorter one for a stop's own trail write: the stop is in force
// before it starts, so waiting only delays the answer, and a write that gives up is exit 5 of `bide stop`, which
// -reaudit repairs. Variables so a test can shorten them.
var (
	stopTrailBudget    = 30 * time.Second
	stopSetTrailBudget = 5 * time.Second
)

// stopTrailBackoff is the wait before the retry after the try-th lost race: 1 ms doubling to 64 ms, jittered
// by +-50%, so writers that collided do not collide again in step.
func stopTrailBackoff(try int) time.Duration {
	d := time.Millisecond << min(try, 6)
	return d/2 + time.Duration(rand.Int64N(int64(d)))
}

// appendTrail appends rec to the trail unless it is already there, retrying at the next position when
// another writer took the one it tried. Writers that lose a race wait a jittered, growing time and try
// again until the caller's deadline (or stopTrailBudget, when it has none): a fixed number of tries ran out
// with ten operators writing at once, and a trail write that gives up turns a stop into exit 5.
func (c *StopControl) appendTrail(ctx context.Context, rec StopRecord) error {
	if _, ok := ctx.Deadline(); !ok { // a caller (write, Reaudit) that has a budget of its own shares it
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, stopTrailBudget)
		defer cancel()
	}
	for try := 0; ; try++ {
		have, err := c.trail.Load(ctx, StopTrailName)
		if err != nil {
			return err
		}
		want := StopDigest(rec)
		for _, h := range have {
			if StopDigest(h) == want {
				return nil
			}
		}
		if err := c.trail.Append(ctx, StopTrailName, len(have), rec); err == nil {
			return nil
		} else if !errors.Is(err, ErrTrailConflict) {
			return err
		}
		select {
		case <-time.After(stopTrailBackoff(try)):
		case <-ctx.Done():
			// The context's own error is in the chain: a caller that cancelled (Ctrl-C, a shutdown) is told it
			// was cancelled, one that ran out of time that the deadline passed, and both that the position was
			// never won.
			return fmt.Errorf("stop trail: no position won in %d tries before the context ended: %w (%w)", try+1, ctx.Err(), ErrTrailConflict)
		}
	}
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
	digests := func(recs []StopRecord) ([]string, map[string]bool) {
		ds, set := make([]string, len(recs)), make(map[string]bool, len(recs))
		for i, r := range recs {
			ds[i] = StopDigest(r) // once per record: the comparison is by digest, not pairwise
			set[ds[i]] = true
		}
		return ds, set
	}
	sd, sset := digests(store)
	td, tset := digests(trail)
	for i, r := range store {
		if !tset[sd[i]] {
			storeOnly = append(storeOnly, r)
		}
	}
	for i, r := range trail {
		if !sset[td[i]] {
			trailOnly = append(trailOnly, r)
		}
	}
	return storeOnly, trailOnly
}

// protocol:stop begin Reaudit

// Reaudit appends to the trail every record of the control run that it lacks and that the control accepts
// (a record the fold rejects is a forgery, and copying it would turn a finding into evidence), in store order, and
// returns how many it appended: how a stop whose trail write failed (a *StopTrailError) is completed, and
// a trail that fell behind is caught up. It needs a control with a trail.
func (c *StopControl) Reaudit(ctx context.Context) (int, error) {
	if c.trail == nil {
		return 0, fmt.Errorf("Reaudit needs a stop trail (WithStopTrail): %w", ErrConfig)
	}
	if _, ok := ctx.Deadline(); !ok { // one budget for every record it appends
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, stopTrailBudget)
		defer cancel()
	}
	recs, err := c.StopRecords(ctx)
	if err != nil {
		return 0, err
	}
	st, err := c.State(ctx)
	if err != nil {
		return 0, err
	}
	have, err := c.trail.Load(ctx, StopTrailName)
	if err != nil {
		return 0, err
	}
	onTrail := make(map[string]bool, len(have))
	for _, h := range have {
		onTrail[StopDigest(h)] = true
	}
	n := 0
	for _, r := range recs {
		if onTrail[StopDigest(r)] || st.rejectedSet[stopRecordStep(r)] {
			continue // already evidenced, or a record the fold rejects (a forgery is not evidence: it is a finding)
		}
		if err := c.appendTrail(ctx, r); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// protocol:stop end

// PendingStop is a record on the trail that the control run lacks and that ApplyPending would consider: signed,
// not a stop, well formed. Age is how long ago its signer wrote it, by the signer's clock.
type PendingStop struct {
	Record StopRecord
	Age    time.Duration
	// Superseded is true when a record the control run holds follows it on the trail: ApplyPending leaves it.
	Superseded bool
}

// pending returns the trail's pending records, in trail order, with the store's records.
func (c *StopControl) pending(ctx context.Context) ([]PendingStop, []StopRecord, error) {
	have, err := c.trail.Load(ctx, StopTrailName)
	if err != nil {
		return nil, nil, err
	}
	store, err := c.StopRecords(ctx)
	if err != nil {
		return nil, nil, err
	}
	inStore := make(map[string]bool, len(store))
	for _, r := range store {
		inStore[StopDigest(r)] = true
	}
	last := -1 // the last record of the trail that the control run holds
	for i, r := range have {
		if inStore[StopDigest(r)] {
			last = i
		}
	}
	var out []PendingStop
	for i, r := range have {
		if inStore[StopDigest(r)] || r.Kind == "set" || r.Kind == "apply" || !r.Signed() || r.Validate() != nil {
			continue
		}
		p := PendingStop{Record: r, Superseded: i < last}
		if t, err := time.Parse(time.RFC3339Nano, r.At); err == nil {
			p.Age = c.now().Sub(t)
		}
		out = append(out, p)
	}
	return out, store, nil
}

// PendingRecords lists the records ApplyPending would consider, with each one's signer (Record.By) and age,
// so an operator sees what a run would make effective before making it so.
func (c *StopControl) PendingRecords(ctx context.Context) ([]PendingStop, error) {
	if c.trail == nil {
		return nil, fmt.Errorf("PendingRecords needs a stop trail (WithStopTrail): %w", ErrConfig)
	}
	p, _, err := c.pending(ctx)
	return p, err
}

// ApplyRequest names who applies pending records: an enrolled approver of the policy in force, with the key to sign
// the record that says so.
type ApplyRequest struct {
	By, Host, Reason string
	Signer           StopSigner
}

// ApplyPending writes to the control run the records of the trail that it lacks and that are not stops: what a
// crash between "trail first" and the control-run write leaves (a lift, a policy transition, the root or a mode
// change that was signed and evidenced and never took effect). It applies, in trail order, each one the control
// would accept (a record the fold would reject stays unapplied) and returns how many it applied.
//
// It is an operator action, and an accountable one. A signature cannot be revoked, so a signed lift that sat on the
// trail can be made effective by whoever runs this, however old it is: see PendingRecords, or `bide audit stops`,
// first. Before it applies anything it writes a signed "apply" record, by req.By, naming the digests it is about to
// apply and when, to the trail and the control run like any other record, so who applied what is evidence.
//
// Only the records at the end of the trail are applied: those after the last record the control run holds. A
// pending record that a stored record follows on the trail is superseded (the next write went on without it):
// writing it now would put it after records the trail has behind it and make the control run's order differ from
// the trail's, so it is left, and counted in superseded. A stop on the trail that the control run lacks is never
// applied (a stop is written to the control run first, so that is a row deleted from it: a finding).
func (c *StopControl) ApplyPending(ctx context.Context, req ApplyRequest) (applied, superseded int, err error) {
	if c.trail == nil {
		return 0, 0, fmt.Errorf("ApplyPending needs a stop trail (WithStopTrail): %w", ErrConfig)
	}
	if req.Signer == nil || req.By == "" {
		return 0, 0, fmt.Errorf("ApplyPending is signed: it needs the applier (By) and a Signer: %w", ErrConfig)
	}
	pend, store, err := c.pending(ctx)
	if err != nil {
		return 0, 0, err
	}
	var todo []StopRecord
	for _, p := range pend {
		if p.Superseded {
			superseded++
			continue
		}
		if ok, err := c.acceptable(p.Record, store); err != nil {
			return 0, superseded, err
		} else if ok {
			todo = append(todo, p.Record)
		}
	}
	if len(todo) == 0 {
		return 0, superseded, nil
	}
	st, err := c.State(ctx)
	if err != nil {
		return 0, superseded, err
	}
	digests := make([]string, len(todo))
	for i, r := range todo {
		digests[i] = StopDigest(r)
	}
	rec := StopRecord{Format: StopFormat, Kind: "apply", Mode: StopAudited, Applied: digests, Reason: req.Reason, By: req.By, Host: req.Host}
	// A dry run first: an applier the policy does not enrol is refused before anything is written.
	probe := rec
	if err := c.complete(st, &probe, req.Signer, false); err != nil {
		return 0, superseded, err
	}
	if ok, err := c.acceptable(probe, store); err != nil {
		return 0, superseded, err
	} else if !ok {
		return 0, superseded, fmt.Errorf("%q is not an approver the policy in force enrols, so the apply record would not be accepted: %w", req.By, ErrStopRejected)
	}
	if err := c.write(ctx, st, &rec, req.Signer, false); err != nil {
		return 0, superseded, err
	}
	if after, err := c.State(ctx); err != nil {
		return 0, superseded, err
	} else {
		for _, rj := range after.Rejected {
			if rj.Name == stopRecordStep(rec) {
				return 0, superseded, fmt.Errorf("the apply record was written but has no effect: %s: %w", rj.Reason, ErrStopRejected)
			}
		}
	}
	for _, r := range todo {
		if err := c.putRecord(ctx, r); err != nil {
			return applied, superseded, err
		}
		applied++
	}
	return applied, superseded, nil
}

// acceptable reports whether folding the control run's records with r accepts r.
func (c *StopControl) acceptable(r StopRecord, store []StopRecord) (bool, error) {
	var journal []Record
	for _, x := range append(slices.Clone(store), r) {
		b, err := json.Marshal(x)
		if err != nil {
			return false, err
		}
		journal = append(journal, Record{Kind: StepValue, Name: stopRecordStep(x), Result: b})
	}
	st, err := c.stateOf(journal)
	if err != nil {
		return false, err
	}
	return !st.Rejects(r), nil
}

// putRecord writes r, already on the trail, to the control run.
func (c *StopControl) putRecord(ctx context.Context, r StopRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, _, err = c.j.putNew(ctx, StopControlRun, stopRecordStep(r), Record{Kind: StepValue, Result: b})
	return err
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
