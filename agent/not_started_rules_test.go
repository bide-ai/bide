package agent

import (
	"context"
	"errors"
	"iter"
	"sync/atomic"
	"testing"
	"time"
)

// Only the driver that claimed an attempt can record that it did not start: a not-started record
// that carries another claim, or is not a StepNotStarted record, does not void the marker.
func TestNotStarted_OnlyTheClaimantVoidsAnAttempt(t *testing.T) {
	for name, forged := range map[string]Record{
		"another claim": {Kind: StepNotStarted, ToolUseID: "reserve", claim: "someone-else"},
		"another kind":  {Kind: StepValue, ToolUseID: "reserve"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := memJournal()
			won, marker, key, err := claimNextAttempt(ctx, store, "r1", stepAttemptStep("reserve"),
				Record{Kind: StepAttempt, ToolUseID: "reserve", AttemptedAt: time.Now().UnixMilli()})
			if err != nil || !won {
				t.Fatalf("setup claim = (%v, %v)", won, err)
			}
			rec := forged
			if name == "another kind" {
				rec.claim = marker.claim
			}
			if _, err := store.do(ctx, "r1", notStartedStep(key, marker.claim), func(context.Context) (Record, error) { return rec, nil }); err != nil {
				t.Fatal(err)
			}
			var entered, ran atomic.Int32
			_, err = store.Step(ctx, "r1", "reserve", reserveFn(&entered, &ran))
			var halt *OutcomeUnknown
			if !errors.As(err, &halt) || entered.Load() != 0 {
				t.Fatalf("Step = %v with fn called %d times, want a OutcomeUnknown: the attempt was never recorded as not started by its claimant", err, entered.Load())
			}
		})
	}
}

// ctxStore is a MemStore whose every method fails once its context is cancelled, before reading
// anything, as a SQL store's does, and that cancels the driver's context once an attempt marker is
// recorded.
type ctxStore struct {
	*MemStore
	cancel func()
}

func (s *ctxStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	e, inserted, err := s.MemStore.Insert(ctx, runID, name, data)
	if rec, derr := DecodeRecord(e.Data); err == nil && derr == nil && rec.Kind == StepAttempt {
		s.cancel()
	}
	return e, inserted, err
}

func (s *ctxStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	return s.MemStore.Get(ctx, runID, name)
}

func (s *ctxStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if err := ctx.Err(); err != nil {
		return func(yield func(Entry, error) bool) { yield(Entry{}, err) }
	}
	return s.MemStore.Load(ctx, runID, after)
}

// On a store that refuses a cancelled context, the step's own write fails before fn is called,
// and the not-started record is still written: it does not depend on the cancelled context.
func TestStep_NotStartedIsRecordedOnAStoreThatChecksContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := mustJournal(&ctxStore{MemStore: NewMemStore(), cancel: cancel})
	var entered, ran atomic.Int32
	reserve := reserveFn(&entered, &ran)
	if _, err := store.Step(ctx, "r1", "reserve", reserve); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled step err = %v, want context.Canceled", err)
	}
	got, err := store.Step(context.Background(), "r1", "reserve", reserve)
	if err != nil || got != "reserved" || ran.Load() != 1 {
		t.Fatalf("second attempt = (%q, %v) with fn run %d times, want (\"reserved\", nil) and exactly 1", got, err, ran.Load())
	}
}

// A saga's rollback compensates the calls that may have taken effect. A call cancelled by a
// sibling's failure after its claim and before it was called changed nothing, so the rollback
// neither halts on it nor compensates it.
func TestSaga_NotStartedCallIsNotCompensated(t *testing.T) {
	var charged, refunded atomic.Int32
	pay := MustCompensatedFunc("pay", "charge the card", func(context.Context, struct{}) (string, error) { charged.Add(1); return "paid", nil },
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil })
	book := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		return "", errors.New("no seats")
	})
	store := mustJournal(&holdClaimStore{MemStore: NewMemStore(), toolUseID: "p1"})
	m := &sagaTurns{turns: [][][3]string{{{"p1", "pay", `{}`}, {"b1", "book", `{}`}}}}
	_, err := mustNew(m, store, WithTools(pay, book)).Run(context.Background(), "r1", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	if aborted.CompensateErr != nil || charged.Load() != 0 || refunded.Load() != 0 {
		t.Fatalf("SagaAborted = %+v, charged %d, refunded %d; want a clean rollback of a charge that never started", aborted, charged.Load(), refunded.Load())
	}
}

// holdClaimStore is a MemStore that, once the attempt marker of the call toolUseID is recorded,
// holds that call until its context (the tool group's) is cancelled, so a sibling's failure lands
// after the claim and before the call.
type holdClaimStore struct {
	*MemStore
	toolUseID string
}

func (s *holdClaimStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	e, inserted, err := s.MemStore.Insert(ctx, runID, name, data)
	if rec, derr := DecodeRecord(e.Data); err == nil && derr == nil && rec.Kind == StepAttempt && rec.ToolUseID == s.toolUseID {
		<-ctx.Done()
	}
	return e, inserted, err
}

// ResolveHalt's minimum age runs from the attempt that may have fired the effect, not from an
// earlier attempt recorded as never started.
func TestResolveHalt_AgeRunsFromTheLiveAttempt(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	now := time.Now()
	base := toolAttemptStep("c1")
	won, first, key, err := claimNextAttempt(ctx, store, "r1", base,
		Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: now.Add(-2 * time.Hour).UnixMilli()})
	if err != nil || !won {
		t.Fatalf("first claim = (%v, %v)", won, err)
	}
	if err := recordNotStarted(ctx, store, "r1", key, first); err != nil {
		t.Fatal(err)
	}
	if won, _, _, err := claimNextAttempt(ctx, store, "r1", base,
		Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: now.UnixMilli()}); err != nil || !won {
		t.Fatalf("re-attempt claim = (%v, %v)", won, err)
	}
	err = ResolveHalt(ctx, store, HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "charged", IsError: false}, WithMinHaltAge(time.Hour), WithClock(func() time.Time { return now }))
	var young *HaltTooYoung
	if !errors.As(err, &young) {
		t.Fatalf("ResolveHalt = %v, want *HaltTooYoung: the live attempt is moments old", err)
	}
}

// A re-attempt's marker halts a resume like a first attempt's: a process that dies after claiming
// the re-attempt leaves its outcome unknown.
func TestTool_CrashInAReattemptHalts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	var calls atomic.Int32
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "charged", nil
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	_, _ = mustNew(m, j, WithTools(charge)).Run(ctx, "r1", UserText("pay")) // cancelled before the call: recorded as not started

	ctx2, cancel2 := context.WithCancel(context.Background())
	crashing := mustJournal(&markerHookStore{MemStore: store.MemStore, cancel: cancel2, crash: true})
	resume := &greedyModel{script: [][]Emit{textTurn("done")}}
	if _, err := mustNew(resume, crashing, WithTools(charge)).Run(ctx2, "r1", UserText("pay")); err == nil {
		t.Fatal("the re-attempt whose process died succeeded")
	}
	_, err := mustNew(
		&greedyModel{script: [][]Emit{textTurn("done")}},
		mustJournal(store.MemStore),
		WithTools(charge),
	).Run(context.Background(), "r1", UserText("pay"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "c1" || calls.Load() != 0 {
		t.Fatalf("resume after a crash in the re-attempt = %v with %d charges, want *OutcomeUnknown for c1 and no charge", err, calls.Load())
	}
}

// A call recorded as not started emits neither ToolStarted nor ToolCompleted: a stream consumer
// sees ToolStarted only for a call that actually starts, and each is paired with its
// ToolCompleted once the call is re-attempted.
func TestStream_NotStartedCallEmitsNoToolStarted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		return "charged", nil
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	evs, _, err := collect(mustNew(m, j, WithTools(charge)).Stream(ctx, "r1", UserText("pay")))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled stream err = %v, want context.Canceled", err)
	}
	for _, e := range evs {
		switch e.(type) {
		case ToolStarted, ToolCompleted:
			t.Fatalf("a call that never started emitted %T (events %v)", e, kinds(evs))
		}
	}

	evs, _, err = collect(mustNew(
		&greedyModel{script: [][]Emit{textTurn("done")}},
		mustJournal(store.MemStore),
		WithTools(charge),
	).Stream(context.Background(), "r1", UserText("pay")))
	if err != nil {
		t.Fatalf("resumed stream err = %v", err)
	}
	var started, completed int
	for _, e := range evs {
		switch e.(type) {
		case ToolStarted:
			started++
		case ToolCompleted:
			completed++
		}
	}
	if started != 1 || completed != 1 {
		t.Fatalf("the re-attempt emitted %d ToolStarted and %d ToolCompleted, want one of each (events %v)", started, completed, kinds(evs))
	}
}

// A saga call cancelled before it started records no accepted arguments (it accepted none); its
// re-attempt goes through tool middleware like any call, journals the arguments it accepted, and
// the rollback undoes those.
func TestSaga_NotStartedReattemptCompensatesTheAcceptedArguments(t *testing.T) {
	var charged, refunded atomic.Int32
	charge := MustCompensatedFunc("charge", "charge the card", func(_ context.Context, in chargeArgs) (string, error) {
		charged.Add(int32(in.Amount))
		return "ok", nil
	},
		func(_ context.Context, in chargeArgs, _ string) error { refunded.Add(int32(in.Amount)); return nil })
	fail := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		return "", errors.New("no seats")
	})
	model := func() Model {
		return NewScriptedModel(ToolTurn("c1", "charge", `{"amount":5}`), ToolTurn("b1", "book", `{}`), TextTurn("done"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	if _, err := mustNew(model(), j, WithTools(charge, fail), WithToolMiddleware(scaleCharge)).Run(ctx, "r", UserText("trip"), WithSaga()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled saga err = %v, want context.Canceled", err)
	}
	if charged.Load() != 0 {
		t.Fatalf("charged %d before the call started, want 0", charged.Load())
	}
	recs, _ := j.History(context.Background(), "r")
	for _, r := range recs {
		if r.Name == sagaArgsStep("c1") {
			t.Fatalf("a call that never started journaled accepted arguments: %+v", r)
		}
	}

	_, err := mustNew(
		model(),
		mustJournal(store.MemStore),
		WithTools(charge, fail),
		WithToolMiddleware(scaleCharge),
	).Run(context.Background(), "r", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil {
		t.Fatalf("resumed saga = %v, want a clean *SagaAborted", err)
	}
	if charged.Load() != 500 || refunded.Load() != 500 {
		t.Fatalf("charged %d, refunded %d; want the re-attempt's 500 charged and refunded", charged.Load(), refunded.Load())
	}
}
