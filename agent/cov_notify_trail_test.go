package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// covTrail is a MemNotifyTrail whose Load or Append can be made to fail, or to lose a race once.
type covTrail struct {
	*MemNotifyTrail
	mu          sync.Mutex
	loadErr     error
	appendErr   error
	conflicts   int // Appends that report ErrTrailConflict before one succeeds
	appendCalls int
}

func (t *covTrail) Load(ctx context.Context, name string) ([]NotifyRecord, error) {
	t.mu.Lock()
	err := t.loadErr
	t.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return t.MemNotifyTrail.Load(ctx, name)
}

func (t *covTrail) Append(ctx context.Context, name string, seq int, rec NotifyRecord) error {
	t.mu.Lock()
	t.appendCalls++
	err, conflict := t.appendErr, t.conflicts > 0
	if conflict {
		t.conflicts--
	}
	t.mu.Unlock()
	switch {
	case err != nil:
		return err
	case conflict:
		return ErrTrailConflict
	}
	return t.MemNotifyTrail.Append(ctx, name, seq, rec)
}

// errCollector is a WithNotifyErrors callback that keeps what it is given.
type errCollector struct {
	mu   sync.Mutex
	errs []error
}

func (c *errCollector) add(err error) { c.mu.Lock(); c.errs = append(c.errs, err); c.mu.Unlock() }
func (c *errCollector) n() int        { c.mu.Lock(); defer c.mu.Unlock(); return len(c.errs) }

// A recovery pass that cannot read or write the notification trail reports the fault to the error
// hook and goes on: it never fails the pass, and it never writes a half record.
func TestNotifier_RecoveryTrailFaultsAreReportedNotFatal(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		set  func(*covTrail)
		call func(*notifyEngine)
	}{
		"failed: load":  {func(t *covTrail) { t.loadErr = errCovBlip }, func(e *notifyEngine) { e.recoveryFailed(ctx, "r", errCovBlip) }},
		"failed: write": {func(t *covTrail) { t.appendErr = errCovBlip }, func(e *notifyEngine) { e.recoveryFailed(ctx, "r", errCovBlip) }},
		"ok: load":      {func(t *covTrail) { t.loadErr = errCovBlip }, func(e *notifyEngine) { e.recoveryOK(ctx, "r") }},
	} {
		t.Run(name, func(t *testing.T) {
			tr := &covTrail{MemNotifyTrail: NewMemNotifyTrail()}
			col := &errCollector{}
			e := testEngine(t, memJournal(), &fakeSink{}, tr, &notifyClock{t: time.Now()}, "p", WithNotifyErrors(col.add))
			tc.set(tr)
			tc.call(e)
			if col.n() != 1 || !errors.Is(col.errs[0], errCovBlip) {
				t.Fatalf("reported %v; want the one fault", col.errs)
			}
		})
	}
	t.Run("ok: write", func(t *testing.T) {
		tr := &covTrail{MemNotifyTrail: NewMemNotifyTrail()}
		col := &errCollector{}
		e := testEngine(t, memJournal(), &fakeSink{}, tr, &notifyClock{t: time.Now()}, "p", WithNotifyErrors(col.add))
		e.recoveryFailed(ctx, "r", errCovBlip) // opens the failure episode
		tr.mu.Lock()
		tr.appendErr = errCovBlip
		tr.mu.Unlock()
		e.recoveryOK(ctx, "r") // cannot record the recovery
		if col.n() != 1 || !errors.Is(col.errs[0], errCovBlip) {
			t.Fatalf("reported %v; want the one fault", col.errs)
		}
	})
}

// Losing the append race while opening or closing a failure episode retries on the fresh trail
// and records the episode once: two processes recovering the same run write one episode.
func TestNotifier_RecoveryEpisodeSurvivesALostAppendRace(t *testing.T) {
	ctx := context.Background()
	tr := &covTrail{MemNotifyTrail: NewMemNotifyTrail(), conflicts: 2}
	col := &errCollector{}
	e := testEngine(t, memJournal(), &fakeSink{}, tr, &notifyClock{t: time.Now()}, "p", WithNotifyErrors(col.add))
	e.recoveryFailed(ctx, "r", errCovBlip)
	recs, _ := tr.MemNotifyTrail.Load(ctx, "notify:r")
	if len(recs) != 1 || recs[0].Kind != NotifyFailingSince || tr.appendCalls != 3 {
		t.Fatalf("after two lost races: %v, %d appends; want one failing_since record after 3 tries", recKinds(recs), tr.appendCalls)
	}
	tr.mu.Lock()
	tr.conflicts, tr.appendCalls = 1, 0
	tr.mu.Unlock()
	e.recoveryOK(ctx, "r")
	recs, _ = tr.MemNotifyTrail.Load(ctx, "notify:r")
	if len(recs) != 2 || recs[1].Kind != NotifyRecovered || tr.appendCalls != 2 {
		t.Fatalf("after a lost race on recovery: %v, %d appends; want one recovered record after 2 tries", recKinds(recs), tr.appendCalls)
	}
	if col.n() != 0 {
		t.Fatalf("a lost race was reported as a fault: %v", col.errs)
	}
}

// What tells one halt of an operation from a later one is the halt's attempt time when it carries
// one, and otherwise the live attempt's generation (negated, less one): the first attempt of an
// untimed marker and each re-attempt after a not-started record are different events, for a tool
// call and for a Step alike. A marker that cannot be read is an error, not a guessed key.
func TestNotifier_HaltAttemptDistinguishesGenerations(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	j := memJournal()
	putMarker(t, j, "r", "c1", 0)
	if _, err := j.do(ctx, "r", stepAttemptStep("s1"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: "s1"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	// c2: the first attempt is recorded as not started, so the live one is the first re-attempt.
	rec := Record{Kind: StepAttempt, ToolUseID: "c2"}
	won, marker, key, err := j.claimNext(ctx, "r", toolAttemptStep("c2"), rec)
	if err != nil || !won {
		t.Fatal(won, err)
	}
	if err := j.notStarted(ctx, "r", key, marker); err != nil {
		t.Fatal(err)
	}
	if won, _, _, err := j.claimNext(ctx, "r", toolAttemptStep("c2"), rec); err != nil || !won {
		t.Fatal(won, err)
	}
	e := testEngine(t, j, &fakeSink{}, NewMemNotifyTrail(), &notifyClock{t: time.Now()}, "p")
	for name, c := range map[string]struct {
		halt *OutcomeUnknown
		want int64
	}{
		"time on the halt":        {&OutcomeUnknown{RunRef: RunRef{RunID: "r"}, Op: OpRef{Kind: OpTool, ID: "x"}, AttemptedAt: at}, at.UnixMilli()},
		"untimed tool marker":     {&OutcomeUnknown{RunRef: RunRef{RunID: "r"}, Op: OpRef{Kind: OpTool, ID: "c1"}}, -1},
		"untimed step marker":     {&OutcomeUnknown{RunRef: RunRef{RunID: "r"}, Op: OpRef{Kind: OpStep, ID: "s1"}}, -1},
		"after a not-started one": {&OutcomeUnknown{RunRef: RunRef{RunID: "r"}, Op: OpRef{Kind: OpTool, ID: "c2"}}, -2},
	} {
		got, err := e.haltAttempt(ctx, c.halt)
		if err != nil || got != c.want {
			t.Errorf("%s: haltAttempt = %d, %v; want %d", name, got, err, c.want)
		}
	}
	bad := testEngine(t, mustJournal(covGetFail{NewMemStore(), "attempt:"}), &fakeSink{}, NewMemNotifyTrail(), &notifyClock{t: time.Now()}, "p")
	if _, err := bad.haltAttempt(ctx, &OutcomeUnknown{RunRef: RunRef{RunID: "r"}, Op: OpRef{Kind: OpTool, ID: "c1"}}); err == nil {
		t.Error("haltAttempt over an unreadable marker succeeded")
	}
}

// A round that cannot read or write the trail, or cannot tell whether its event still stands,
// returns the fault with the time to try again, and sends nothing: an alert is never delivered
// on a state the engine could not record or read.
func TestNotifyRound_FaultsSendNothingAndSayWhenToRetry(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	for name, c := range map[string]struct {
		trail func(*covTrail)
		store func(*MemStore) Store
	}{
		"trail unreadable":       {func(t *covTrail) { t.loadErr = errCovBlip }, nil},
		"observation unwritable": {func(t *covTrail) { t.appendErr = errCovBlip }, nil},
		"run state unreadable":   {nil, func(s *MemStore) Store { return loadFails{s} }},
	} {
		t.Run(name, func(t *testing.T) {
			tr := &covTrail{MemNotifyTrail: NewMemNotifyTrail()}
			if c.trail != nil {
				c.trail(tr)
			}
			var st Store = NewMemStore()
			if c.store != nil {
				st = c.store(NewMemStore())
			}
			sink := &fakeSink{}
			e := testEngine(t, mustJournal(st), sink, tr, clk, "p1")
			res, err := e.round(ctx, haltEvent())
			if err == nil || res.done || !res.next.After(clk.now()) {
				t.Fatalf("round = %+v, %v; want a fault, not done, and a retry time in the future", res, err)
			}
			if sink.count() != 0 {
				t.Fatalf("an alert was sent despite the fault: %d", sink.count())
			}
		})
	}
}
