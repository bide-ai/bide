package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// failingWaker fails its first fail Schedule calls, then registers every wake it is given.
type failingWaker struct {
	mu    sync.Mutex
	fail  int
	calls int
	wakes []Wake
}

func (w *failingWaker) Schedule(_ context.Context, wk Wake) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls <= w.fail {
		return errors.New("scheduler unavailable")
	}
	w.wakes = append(w.wakes, wk)
	return nil
}

func (w *failingWaker) scheduled() []Wake {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.wakes)
}

// napTool sleeps for an hour under the name "nap", from a retry-safe tool.
func napTool() Tool {
	return MustFunc("nap", "sleep for an hour", func(ctx context.Context, _ struct{}) (string, error) {
		if err := Sleep(ctx, "nap", time.Hour); err != nil {
			return "", err
		}
		return "rested", nil
	}, WithSafety(Safety{ReadOnly: true}))
}

// A Waker that cannot schedule the wake must not let the run pause (nothing might ever wake it)
// nor record a failure (the model would read the scheduler's outage as the tool's answer, and a
// saga would roll back). The run fails with an error wrapping ErrStorage, records nothing for the
// call, and the next drive schedules again and pauses.
func TestWaker_ScheduleFailureFailsTheRunAndRecordsNothing(t *testing.T) {
	for _, saga := range []bool{false, true} {
		t.Run(fmt.Sprintf("saga=%v", saga), func(t *testing.T) {
			store := memJournal()
			w := &failingWaker{fail: 1}
			ctx := contextWithWaker(context.Background(), w)
			drive := func() error {
				a := mustNew(
					&greedyModel{script: [][]Emit{toolTurn("c1", "nap", `{}`), textTurn("done")}},
					store,
					WithTools(napTool()),
				)
				var err error
				if saga {
					_, err = a.Run(ctx, "r1", UserText("rest"), WithSaga())
				} else {
					_, err = a.Run(ctx, "r1", UserText("rest"))
				}
				return err
			}

			err := drive()
			if !errors.Is(err, ErrStorage) || IsPause(err) || errors.Is(err, ErrTool) {
				t.Fatalf("run with a failing Waker = %v; want an error wrapping ErrStorage (and not ErrTool), not a pause", err)
			}
			recs, herr := store.History(context.Background(), "r1")
			if herr != nil {
				t.Fatal(herr)
			}
			for _, r := range recs {
				if r.Kind == StepToolResult || r.Kind == StepSagaFail || r.Name == runCompleteStep || r.Name == runAbortedStep {
					t.Fatalf("a failed schedule recorded %s (kind %v); want nothing recorded for the call", r.Name, r.Kind)
				}
			}

			err = drive()
			tp, ok := errors.AsType[*TimerPending](err)
			if !ok {
				t.Fatalf("re-driven run = %v; want *TimerPending once the Waker schedules", err)
			}
			got := w.scheduled()
			want := Wake{RunID: "r1", RootRunID: "r1", Name: "nap", FireAt: tp.FireAt}
			if len(got) != 1 || !got[0].FireAt.Equal(want.FireAt) || got[0].RunID != want.RunID || got[0].RootRunID != want.RootRunID || got[0].Name != want.Name {
				t.Fatalf("scheduled wakes = %+v; want exactly %+v", got, want)
			}
		})
	}
}

// RecoverLoop retries a run whose wake could not be scheduled: the failure is reported (it wraps
// ErrStorage), and a later pass drives the run again, which schedules the wake and pauses.
func TestRecoverLoop_RetriesAFailedWakeSchedule(t *testing.T) {
	store := memJournal()
	newAgent := func() *Agent {
		return mustNew(
			&greedyModel{script: [][]Emit{toolTurn("c1", "nap", `{}`), textTurn("done")}},
			store,
			WithTools(napTool()),
		)
	}
	// The first drive has no Waker: it pauses at the timer with nothing scheduled, as a run does
	// whose process died before its wake was registered anywhere.
	if _, err := newAgent().Run(context.Background(), "r1", UserText("rest")); !IsPause(err) {
		t.Fatalf("first drive = %v; want a pause", err)
	}

	w := &failingWaker{fail: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var reported []error
	resume := func(ctx context.Context, runID string, _ RunStart) error {
		_, err := newAgent().Run(ctx, runID, UserText("rest"), WithWaker(w))
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- RecoverLoop(ctx, store, resume, WithRecoverInterval(5*time.Millisecond),
			WithRecoverErrors(func(err error) { mu.Lock(); reported = append(reported, err); mu.Unlock() }))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(w.scheduled()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if got := w.scheduled(); len(got) == 0 || got[0].RunID != "r1" || got[0].Name != "nap" {
		t.Fatalf("scheduled wakes = %+v; want the run's wake scheduled by a later pass", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 || !errors.Is(reported[0], ErrStorage) {
		t.Fatalf("reported = %v; want exactly the one schedule failure, wrapping ErrStorage", reported)
	}
}

// MemWaker decides whether to retry a resume with IsPause, so every kind of pause, wrapped or not,
// consumes the wake, and any other error puts it back.
func TestMemWaker_UsesIsPause(t *testing.T) {
	ref := RunRef{RunID: "r1", RootRunID: "r1"}
	pauses := map[string]error{
		"approval":  &ApprovalPending{RunRef: ref},
		"interrupt": &InterruptPending{RunRef: ref, Name: "q"},
		"signal":    &SignalPending{RunRef: ref, Name: "s"},
		"timer":     &TimerPending{RunRef: ref, Name: "next"},
		"halt":      &OutcomeUnknown{RunRef: ref, Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed},
	}
	for name, p := range pauses {
		for _, wrapped := range []bool{false, true} {
			err := p
			if wrapped {
				err = fmt.Errorf("resume r1: %w", p)
			}
			calls := 0
			w := NewMemWaker(func(context.Context, string) error { calls++; return err })
			due := time.Unix(1000, 0)
			if serr := w.Schedule(context.Background(), Wake{RunID: "r1", Name: "nap", FireAt: due}); serr != nil {
				t.Fatal(serr)
			}
			_, _ = w.Fire(context.Background(), due)
			_, _ = w.Fire(context.Background(), due.Add(time.Second))
			if calls != 1 {
				t.Errorf("%s (wrapped=%v): resume called %d times, want 1: a pause must consume the wake", name, wrapped, calls)
			}
		}
	}

	calls := 0
	w := NewMemWaker(func(context.Context, string) error { calls++; return fmt.Errorf("resume: %w", ErrStorage) })
	due := time.Unix(1000, 0)
	_ = w.Schedule(context.Background(), Wake{RunID: "r1", Name: "nap", FireAt: due})
	_, _ = w.Fire(context.Background(), due)
	_, _ = w.Fire(context.Background(), due.Add(time.Second))
	if calls != 2 {
		t.Fatalf("a failed resume was called %d times, want 2: it must be retried", calls)
	}
}

// Timers of the same name in two sub-runs of one root are two wakes: scheduling the second must
// not replace the first, and the root is what is resumed.
func TestMemWaker_SubRunTimersAreDistinct(t *testing.T) {
	var resumed []string
	w := NewMemWaker(func(_ context.Context, runID string) error { resumed = append(resumed, runID); return nil })
	t1, t2 := time.Unix(1000, 0), time.Unix(2000, 0)
	ctx := context.Background()
	for _, wk := range []Wake{
		{RunID: "root>a", RootRunID: "root", Name: "nap", FireAt: t1},
		{RunID: "root>b", RootRunID: "root", Name: "nap", FireAt: t2},
	} {
		if err := w.Schedule(ctx, wk); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := w.Fire(ctx, t1); n != 1 || err != nil {
		t.Fatalf("Fire at the first timer = %d, %v; want the root resumed once (the first sub-run's timer was overwritten)", n, err)
	}
	if n, err := w.Fire(ctx, t2); n != 1 || err != nil {
		t.Fatalf("Fire at the second timer = %d, %v; want the root resumed again", n, err)
	}
	if !slices.Equal(resumed, []string{"root", "root"}) {
		t.Fatalf("resumed %v; want the root twice", resumed)
	}
	if err := w.Schedule(ctx, Wake{Name: "nap", FireAt: t1}); !errors.Is(err, ErrConfig) {
		t.Fatalf("Schedule with no RunID = %v; want ErrConfig", err)
	}
}

// ResolveHaltRef clears a tool call's halt from the halt's own Ref, and records the evidence given
// in the Outcome: the resumed run proceeds past the call without running it again.
func TestResolveHaltRef_ToolHalt(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	var charged int
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		charged++
		return "", fmt.Errorf("gateway connection reset (%w)", ErrToolOutcomeUnknown)
	})
	if _, err := mustNew(
		&greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}},
		store,
		WithTools(charge),
	).Run(ctx, "r1", UserText("pay")); !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("first drive = %v; want ErrToolOutcomeUnknown", err)
	}
	_, err := mustNew(&greedyModel{script: [][]Emit{textTurn("done")}}, store, WithTools(charge)).Run(ctx, "r1", UserText("pay"))
	halt, ok := errors.AsType[*OutcomeUnknown](err)
	if !ok {
		t.Fatalf("resume = %v; want *OutcomeUnknown", err)
	}
	want := HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1", ToolName: "charge"}, Cause: HaltCrashed}
	if halt.Ref() != want {
		t.Fatalf("Ref = %+v; want %+v", halt.Ref(), want)
	}
	if err := ResolveHalt(ctx, store, halt.Ref(), Outcome{Result: "charged", Evidence: map[string]string{"charge": "ch_1"}}); err != nil {
		t.Fatal(err)
	}
	rec, ok := hasStep(t, store, "r1", ToolResultStep("c1"))
	if !ok || rec.Kind != StepToolResult || rec.IsError || string(rec.Result) != `"charged"` || !rec.Reconciled || string(rec.Evidence) != `{"charge":"ch_1"}` {
		t.Fatalf("resolved record = %+v; want the reconciled result with its evidence", rec)
	}
	res, err := mustNew(&greedyModel{script: [][]Emit{textTurn("done")}}, store, WithTools(charge)).Run(ctx, "r1", UserText("pay"))
	var msg Message
	if res != nil {
		msg = res.Message
	}
	if err != nil || msg.Text() != "done" {
		t.Fatalf("run after resolution = %q, %v; want it to finish", msg.Text(), err)
	}
	if charged != 1 {
		t.Fatalf("charged %d times, want 1", charged)
	}
}

// ResolveHaltRef clears a Step's halt too, as a value or as a failure, and refuses a ref whose
// kind is not the kind of operation that halted.
func TestResolveHaltRef_StepHalt(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	lost := func(context.Context) (string, error) { return "", errors.New("connection dropped") }
	halted := func(name string) *OutcomeUnknown {
		t.Helper()
		if _, err := store.Step(ctx, "r1", name, lost); err == nil {
			t.Fatal("the failing step succeeded")
		}
		_, err := store.Step(ctx, "r1", name, lost)
		halt, ok := errors.AsType[*OutcomeUnknown](err)
		if !ok {
			t.Fatalf("resumed step = %v; want *OutcomeUnknown", err)
		}
		if want := (HaltRef{RunID: "r1", Op: OpRef{Kind: OpStep, ID: name}, Cause: HaltCrashed}); halt.Ref() != want || halt.AttemptedAt.IsZero() {
			t.Fatalf("halt = %+v; want Ref %+v with its attempt time", halt, want)
		}
		return halt
	}

	ok := halted("reserve")
	asTool := ok.Ref()
	asTool.Op.Kind = OpTool
	if err := ResolveHalt(ctx, store, asTool, Outcome{Result: "x"}); !errors.Is(err, ErrConfig) {
		t.Fatalf("resolving a step's halt as a tool call = %v; want ErrConfig", err)
	}
	if err := ResolveHalt(ctx, store, ok.Ref(), Outcome{Result: "R-9"}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Step(ctx, "r1", "reserve", lost); err != nil || got != "R-9" {
		t.Fatalf("step after resolution = %q, %v; want the recorded value", got, err)
	}

	failed := halted("pay")
	if err := ResolveHalt(ctx, store, failed.Ref(), Outcome{Result: "declined", IsError: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Step(ctx, "r1", "pay", lost); !errors.Is(err, ErrTool) {
		t.Fatalf("step resolved as failed = %v; want ErrTool", err)
	}
}

// A ref that does not say why the run halted, or what kind of operation halted, is refused, as is
// evidence given twice; none of them records anything.
func TestResolveHaltRef_RefusesAnIncompleteRef(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	good := HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
	for name, c := range map[string]struct {
		ref  HaltRef
		out  Outcome
		opts []ResolveOption
	}{
		"no cause":       {ref: HaltRef{RunID: "r1", Op: good.Op}},
		"unknown cause":  {ref: HaltRef{RunID: "r1", Op: good.Op, Cause: "maybe"}},
		"no kind":        {ref: HaltRef{RunID: "r1", Op: OpRef{ID: "c1"}, Cause: HaltCrashed}},
		"no run":         {ref: HaltRef{Op: good.Op, Cause: HaltCrashed}},
		"no id":          {ref: HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool}, Cause: HaltCrashed}},
		"reserved step":  {ref: HaltRef{RunID: "r1", Op: OpRef{Kind: OpStep, ID: "run:complete"}, Cause: HaltCrashed}},
		"evidence twice": {ref: good, out: Outcome{Evidence: 1}, opts: []ResolveOption{WithEvidence(2)}},
	} {
		if err := ResolveHalt(ctx, store, c.ref, c.out, c.opts...); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: ResolveHaltRef = %v; want ErrConfig", name, err)
		}
	}
	if recs, _ := store.History(ctx, "r1"); len(recs) != 0 {
		t.Fatalf("refused resolutions recorded %d records", len(recs))
	}
}

// A contended halt may still be running in the driver that won the claim, so it is not resolved
// while it is young: without WithMinHaltAge it is refused outright, and with it the age is
// measured from the live attempt (an older attempt recorded as never started does not count). A
// crashed halt of the same age is resolved without a minimum, as before.
func TestResolveHaltRef_ContendedHaltIsNotResolvedWhileYoung(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	t0 := time.Unix(1_000_000, 0)
	// c1's first attempt, claimed an hour ago, never started; its live re-attempt is at t0.
	base := toolAttemptStep("c1")
	won, marker, key, err := claimNextAttempt(ctx, store, "r1", base, Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: t0.Add(-time.Hour).UnixMilli()})
	if err != nil || !won {
		t.Fatal(won, err)
	}
	if err := recordNotStarted(ctx, store, "r1", key, marker); err != nil {
		t.Fatal(err)
	}
	if won, _, _, err := claimNextAttempt(ctx, store, "r1", base, Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: t0.UnixMilli()}); err != nil || !won {
		t.Fatal(won, err)
	}
	contended := HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1", ToolName: "charge"}, Cause: HaltContended}
	out := Outcome{Result: "charged"}
	unresolved := func(msg string) {
		t.Helper()
		if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); ok {
			t.Fatalf("%s: a result was recorded: %+v", msg, rec)
		}
	}

	if err := ResolveHalt(ctx, store, contended, out); !errors.Is(err, ErrConfig) {
		t.Fatalf("contended halt without WithMinHaltAge = %v; want ErrConfig", err)
	}
	unresolved("without a minimum age")
	young := t0.Add(10 * time.Second)
	err = ResolveHalt(ctx, store, contended, out, WithMinHaltAge(time.Minute), WithClock(func() time.Time { return young }))
	if tooYoung, ok := errors.AsType[*HaltTooYoung](err); !ok || tooYoung.Age != 10*time.Second {
		t.Fatalf("young contended halt = %v; want *HaltTooYoung aged 10s (from the live attempt, not the voided one)", err)
	}
	unresolved("while young")

	// The same young marker, halted by a crash, is resolved without a minimum age.
	if won, _, err := ClaimAttempt(ctx, store, "r1", toolAttemptStep("c2"), Record{Kind: StepAttempt, ToolUseID: "c2", AttemptedAt: t0.UnixMilli()}); err != nil || !won {
		t.Fatal(won, err)
	}
	if err := ResolveHalt(ctx, store, HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c2"}, Cause: HaltCrashed}, out); err != nil {
		t.Fatalf("crashed halt = %v; want it resolved", err)
	}

	old := t0.Add(2 * time.Minute)
	if err := ResolveHalt(ctx, store, contended, out, WithMinHaltAge(time.Minute), WithClock(func() time.Time { return old })); err != nil {
		t.Fatalf("aged contended halt = %v; want it resolved", err)
	}
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); !ok || string(rec.Result) != `"charged"` {
		t.Fatalf("resolved record = %+v, %v", rec, ok)
	}
}

// The new verbs write exactly what the names they replace wrote, so a journal is the same
// whichever a caller used, and they check their arguments under their own names.
func TestVerbs_WrappersWriteTheSameRecords(t *testing.T) {
	ctx := context.Background()
	type verb struct{ newer, older func(*Journal) error }
	for name, v := range map[string]verb{
		"AnswerInterrupt/Resume": {
			func(d *Journal) error { return d.AnswerInterrupt(ctx, "r1", "q", 42) },
			func(d *Journal) error { return d.AnswerInterrupt(ctx, "r1", "q", 42) },
		},
		"Enqueue/Send": {
			func(d *Journal) error { return d.Enqueue(ctx, "r1", "jobs", "k1", "body") },
			func(d *Journal) error { return d.Enqueue(ctx, "r1", "jobs", "k1", "body") },
		},
	} {
		a, b := NewMemStore(), NewMemStore()
		j := mustJournal(a)
		j2 := mustJournal(b)
		if err := v.newer(j); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := v.older(j2); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// The salt is random per record, so compare what the verb chose to write.
		type written struct {
			Name, ToolUseID, Approver string
			Kind                      StepKind
			Result                    json.RawMessage
			Approved                  bool
			Signature                 []byte
		}
		project := func(d *Journal) []written {
			recs, _ := d.History(ctx, "r1")
			var out []written
			for _, r := range recs {
				out = append(out, written{r.Name, r.ToolUseID, r.Approver(), r.Kind, r.Result, r.Approved, r.Signature()})
			}
			return out
		}
		ra, rb := project(j), project(j2)
		ja, _ := json.Marshal(ra)
		jb, _ := json.Marshal(rb)
		if len(ra) != 2 || string(ja) != string(jb) { // the @journal header, then the verb's record
			t.Errorf("%s: records differ:\n new %s\n old %s", name, ja, jb)
		}
	}
	for name, err := range map[string]error{
		"SubmitDecision":  SubmitDecision(ctx, memJournal(), Decision{ToolUseID: "c1", ApproverID: "a", Signature: []byte("s")}),
		"AnswerInterrupt": memJournal().AnswerInterrupt(ctx, "", "q", 1),
		"Enqueue":         memJournal().Enqueue(ctx, "", "jobs", "k", 1),
	} {
		if !errors.Is(err, ErrConfig) || err.Error()[:len(name)] != name {
			t.Errorf("%s with no runID = %v; want ErrConfig naming %s", name, err, name)
		}
	}
}

// An interrupt is answered against the pause's own RunID and Name.
func TestAnswerInterrupt_ResumesThePause(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	ask := MustFunc("ask", "ask a human", func(ctx context.Context, _ struct{}) (string, error) {
		n, err := Interrupt[int](ctx, "how-many", "how many?")
		if err != nil {
			return "", err
		}
		return fmt.Sprint(n), nil
	}, WithSafety(Safety{ReadOnly: true}))
	_, err := mustNew(
		&greedyModel{script: [][]Emit{toolTurn("c1", "ask", `{}`), textTurn("done")}},
		store,
		WithTools(ask),
	).Run(ctx, "r1", UserText("go"))
	p, ok := errors.AsType[*InterruptPending](err)
	if !ok || p.Name != "how-many" || p.Prompt != "how many?" || p.Paused() != (RunRef{RunID: "r1", RootRunID: "r1"}) {
		t.Fatalf("run = %v; want *InterruptPending at how-many for r1", err)
	}
	if err := store.AnswerInterrupt(ctx, p.RunID, p.Name, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := mustNew(&greedyModel{script: [][]Emit{textTurn("done")}}, store, WithTools(ask)).Run(ctx, p.RootRunID, UserText("go")); err != nil {
		t.Fatal(err)
	}
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); !ok || string(rec.Result) != `"3"` {
		t.Fatalf("tool result = %+v; want the answer", rec)
	}
}

// An approval or a halt raised inside a tool call (a sub-agent's) belongs to the sub-run's
// journal, so it propagates whatever the calling tool's safety: re-driving the call re-enters
// the sub-run rather than firing an effect here. Any other pause from a tool that is not
// retry-safe is ErrConfig (TestInterrupt_RequiresRetrySafe).
func TestLoop_SubTreePausePropagatesFromAnyTool(t *testing.T) {
	ref := RunRef{RunID: "r1>c1", RootRunID: "r1"}
	for _, p := range []Pause{
		&ApprovalPending{RunRef: ref, ToolUseID: "inner", ToolName: "gated"},
		&OutcomeUnknown{RunRef: ref, Op: OpRef{Kind: OpTool, ID: "inner", ToolName: "charge"}, Cause: HaltCrashed},
	} {
		delegate := MustFunc("delegate", "a side-effecting call that ran a sub-run", func(context.Context, struct{}) (string, error) {
			return "", fmt.Errorf("sub-run: %w", p)
		})
		_, err := mustNew(
			&greedyModel{script: [][]Emit{toolTurn("c1", "delegate", `{}`), textTurn("done")}},
			memJournal(),
			WithTools(delegate),
		).Run(context.Background(), "r1", UserText("go"))
		if got, ok := AsPause(err); !ok || got != p {
			t.Errorf("%T from a side-effecting tool: Run = %v; want the pause propagated", p, err)
		}
	}
}
