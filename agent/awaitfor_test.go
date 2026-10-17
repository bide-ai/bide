package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// awaitForTool blocks on an external signal with a deadline. It records whether the
// signal or the timeout won and the delivered payload, then returns a result so the run
// can complete past the await.
type awaitForTool struct {
	name    string
	safety  Safety
	sig     string
	d       time.Duration
	calls   *int
	got     *string
	arrived *bool
}

// Spec describes the tool to the agent (see Tool).
func (t *awaitForTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Description: "", Input: json.RawMessage(`{"type":"object"}`), Safety: t.safety}
}

func (t *awaitForTool) Call(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	*t.calls++
	v, ok, err := AwaitFor[string](ctx, t.sig, t.d)
	if err != nil {
		return nil, err
	}
	if t.got != nil {
		*t.got = v
	}
	if t.arrived != nil {
		*t.arrived = ok
	}
	return json.Marshal(map[string]any{"event": v, "arrived": ok})
}

// Signal-first: a tool pauses on AwaitFor; Signal delivers the event before the deadline;
// re-running the same run resolves the await with (payload, true) and completes.
func TestAwaitFor_SignalFirst(t *testing.T) {
	store := memJournal()
	var clk int64 = 1000
	now := func() time.Time { return time.Unix(atomic.LoadInt64(&clk), 0) }
	ctx := contextWithClock(context.Background(), now)

	var calls int
	var got string
	var arrived bool
	tool := &awaitForTool{name: "watch", safety: Safety{ReadOnly: true}, sig: "webhook", d: time.Hour, calls: &calls, got: &got, arrived: &arrived}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "watch", `{}`), textTurn("done")}}
	a := mustNew(m, store, WithTools(tool))

	_, err := a.Run(ctx, "r", UserText("hi"))
	var awt *SignalPending
	if !errors.As(err, &awt) {
		t.Fatalf("err = %v, want *SignalPending", err)
	}
	if awt.Name != "webhook" {
		t.Fatalf("awaiting = %+v", awt)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times before signal, want 1", calls)
	}

	// Deliver the signal before the deadline (clock has not advanced).
	if err := store.Signal(context.Background(), "r", "webhook", "payload-1"); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	res, err := a.Run(ctx, "r", UserText("hi")) // same agent, resumes
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	out := res.Message
	if textOf(out) != "done" {
		t.Fatalf("answer = %q", textOf(out))
	}
	if !arrived {
		t.Fatalf("arrived = false, want true (signal won the race)")
	}
	if got != "payload-1" {
		t.Fatalf("tool received %q, want payload-1", got)
	}
	if calls != 2 {
		t.Fatalf("tool ran %d times total, want 2 (re-run to resolve)", calls)
	}
}

// Timeout-first: no signal is delivered; advancing the clock past the deadline on the
// resume run resolves the await with (zero, false) and the run completes on the timeout
// branch.
func TestAwaitFor_TimeoutFirst(t *testing.T) {
	store := memJournal()
	var clk int64 = 1000
	now := func() time.Time { return time.Unix(atomic.LoadInt64(&clk), 0) }
	ctx := contextWithClock(context.Background(), now)

	var calls int
	var got string = "sentinel"
	var arrived bool = true
	tool := &awaitForTool{name: "watch", safety: Safety{ReadOnly: true}, sig: "webhook", d: time.Hour, calls: &calls, got: &got, arrived: &arrived}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "watch", `{}`), textTurn("done")}}
	a := mustNew(m, store, WithTools(tool))

	// First run: no signal, before the deadline, so the run pauses durably.
	_, err := a.Run(ctx, "r", UserText("hi"))
	var awt *SignalPending
	if !errors.As(err, &awt) {
		t.Fatalf("err = %v, want *SignalPending", err)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times before timeout, want 1", calls)
	}

	// Advance past the deadline (now+1h) and resume: the timeout wins.
	atomic.StoreInt64(&clk, 1000+3600)
	res, err := a.Run(ctx, "r", UserText("hi"))
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	out := res.Message
	if textOf(out) != "done" {
		t.Fatalf("answer = %q", textOf(out))
	}
	if arrived {
		t.Fatalf("arrived = true, want false (timeout won the race)")
	}
	if got != "" {
		t.Fatalf("tool received %q, want zero value on timeout", got)
	}
	if calls != 2 {
		t.Fatalf("tool ran %d times total, want 2 (re-run to resolve)", calls)
	}
}

// Deadline stability: the journaled deadline does not move across re-invocations. A
// resume before the deadline still pauses; only a resume at or after the original
// deadline expires. This proves the wake time is fixed on the first encounter (at-most
// once), not recomputed as now()+d on each resume.
func TestAwaitFor_DeadlineStable(t *testing.T) {
	store := memJournal()
	var clk int64 = 1000
	now := func() time.Time { return time.Unix(atomic.LoadInt64(&clk), 0) }
	ctx := contextWithClock(context.Background(), now)

	var calls int
	var arrived bool = true
	tool := &awaitForTool{name: "watch", safety: Safety{ReadOnly: true}, sig: "webhook", d: time.Hour, calls: &calls, arrived: &arrived}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "watch", `{}`), textTurn("done")}}
	a := mustNew(m, store, WithTools(tool))

	// First run at t=1000: deadline is journaled as 1000+3600.
	if _, err := a.Run(ctx, "r", UserText("hi")); !errorsIsAwaiting(err) {
		t.Fatalf("first run should pause with *SignalPending, got %v", err)
	}

	// Resume at +30m: still before the original deadline. If the deadline had drifted to
	// now()+1h it would push out, but it must stay fixed, so the run pauses again.
	atomic.StoreInt64(&clk, 1000+1800)
	if _, err := a.Run(ctx, "r", UserText("hi")); !errorsIsAwaiting(err) {
		t.Fatalf("resume before the fixed deadline should still pause, got %v", err)
	}

	// Confirm the journaled deadline is exactly the first-encounter value (1000+3600).
	recs, err := store.History(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	var deadline time.Time
	found := false
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == "await-timeout:webhook" {
			found = true
			if err := json.Unmarshal(r.Result, &deadline); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found {
		t.Fatal("no journaled deadline recorded")
	}
	if want := time.Unix(1000+3600, 0); !deadline.Equal(want) {
		t.Fatalf("journaled deadline = %s, want %s (must not drift across resumes)", deadline, want)
	}

	// Resume at the original deadline: now the timeout wins and the run completes.
	atomic.StoreInt64(&clk, 1000+3600)
	res, err := a.Run(ctx, "r", UserText("hi"))
	if err != nil {
		t.Fatalf("resume at the deadline should complete, got %v", err)
	}
	out := res.Message
	if textOf(out) != "done" {
		t.Fatalf("answer = %q", textOf(out))
	}
	if arrived {
		t.Fatalf("arrived = true, want false (timeout won)")
	}
}

func errorsIsAwaiting(err error) bool {
	var awt *SignalPending
	return errors.As(err, &awt)
}

// A tool that re-runs after AwaitFor resolved (here it pauses on a later Interrupt) sees the
// same outcome every time: once the timeout has won, a signal delivered after the deadline does
// not flip the call to the signal branch.
func TestAwaitFor_TimeoutOutcomeSurvivesALateSignal(t *testing.T) {
	store := memJournal()
	var clk int64 = 1000
	ctx := contextWithClock(context.Background(), func() time.Time { return time.Unix(atomic.LoadInt64(&clk), 0) })
	var outcomes []bool
	tool := MustFunc("watch", "", func(ctx context.Context, _ struct{}) (string, error) {
		_, ok, err := AwaitFor[string](ctx, "webhook", time.Minute)
		if err != nil {
			return "", err
		}
		outcomes = append(outcomes, ok)
		return Interrupt[string](ctx, "confirm", nil) // a later pause in the same tool
	}, WithSafety(Safety{ReadOnly: true}))
	a := mustNew(NewScriptedModel(ToolTurn("c1", "watch", `{}`), TextTurn("done")), store, WithTools(tool))
	var aw *SignalPending
	if _, err := a.Run(ctx, "r", UserText("go")); !errors.As(err, &aw) {
		t.Fatalf("first run: %v, want *SignalPending", err)
	}
	atomic.StoreInt64(&clk, 2000) // the deadline passes: the timeout wins
	var in *InterruptPending
	if _, err := a.Run(ctx, "r", UserText("go")); !errors.As(err, &in) {
		t.Fatalf("second run: %v, want *InterruptPending", err)
	}
	if err := store.Signal(context.Background(), "r", "webhook", "late"); err != nil {
		t.Fatal(err)
	}
	if err := store.AnswerInterrupt(context.Background(), "r", "confirm", "yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "r", UserText("go")); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if len(outcomes) != 2 || outcomes[0] || outcomes[1] {
		t.Fatalf("AwaitFor outcomes across re-runs of one call = %v, want [false false] (timeout, then timeout)", outcomes)
	}
}

// AwaitFor inside a sub-agent schedules its wake for the root run, which the waker's resume
// callback (the root agent) can drive; the woken tree completes on the timeout branch.
func TestSubAgentAwaitFor_WakesTheRoot(t *testing.T) {
	store := memJournal()
	var clk int64 = 1000
	now := func() time.Time { return time.Unix(atomic.LoadInt64(&clk), 0) }
	watch := MustFunc("watch", "wait for a webhook", func(ctx context.Context, _ struct{}) (string, error) {
		_, ok, err := AwaitFor[string](ctx, "webhook", time.Minute)
		if err != nil {
			return "", err
		}
		if ok {
			return "signaled", nil
		}
		return "timed out", nil
	}, WithSafety(Safety{ReadOnly: true}))
	root := clerkTree(store, watch)
	var woken []string
	var completed bool
	w := NewMemWaker(func(ctx context.Context, runID string) error {
		woken = append(woken, runID)
		res, err := root.Run(ctx, runID, UserText("go"))
		var msg Message
		if res != nil {
			msg = res.Message
		}
		completed = err == nil && msg.Text() == "parent done"
		return err
	})
	ctx := contextWithWaker(contextWithClock(context.Background(), now), w)
	_, err := root.Run(ctx, "p", UserText("go"))
	var aw *SignalPending
	if !errors.As(err, &aw) || aw.RunID != "p>s1" || aw.RootRunID != "p" {
		t.Fatalf("await = %+v (%v); want RunID p>s1 and RootRunID p", aw, err)
	}
	atomic.StoreInt64(&clk, 2000)
	if _, err := w.Fire(ctx, now()); err != nil {
		t.Fatalf("Fire: %v (woke %v)", err, woken)
	}
	if len(woken) != 1 || woken[0] != "p" || !completed {
		t.Fatalf("woke %v (completed=%v); want the root run p woken and completed", woken, completed)
	}
	if rec, ok := hasStep(t, store, "p>s1", ToolResultStep("c1")); !ok || string(rec.Result) != `"timed out"` {
		t.Fatalf("sub-run recorded %s for the await, want the timeout branch", rec.Result)
	}
}

// AwaitFor outside a run is ErrConfig. In a run, a recorded deadline or outcome that does not decode
// is ErrProtocol, a signal payload of another type than the tool asks for is ErrProtocol, and a
// store that cannot record the deadline is ErrStorage: none of them is a timeout or an arrival.
func TestAwaitFor_FaultsAreNotOutcomes(t *testing.T) {
	ctx := context.Background()
	if _, _, err := AwaitFor[string](ctx, "sig", time.Hour); !errors.Is(err, ErrConfig) {
		t.Errorf("AwaitFor outside a run = %v; want ErrConfig", err)
	}
	run := func(j *Journal) context.Context { return withRunContext(ctx, j, "r", "", false) }

	t.Run("corrupt deadline", func(t *testing.T) {
		j := memJournal()
		writeRaw(t, j, "r", awaitTimeoutStep("sig"), Record{Kind: StepValue, Result: []byte(`"soon"`)})
		if _, ok, err := AwaitFor[string](run(j), "sig", time.Hour); !errors.Is(err, ErrProtocol) || ok {
			t.Errorf("= %v, %v; want ErrProtocol", ok, err)
		}
	})
	t.Run("corrupt outcome", func(t *testing.T) {
		j := memJournal()
		writeRaw(t, j, "r", awaitResolvedStep("sig"), Record{Kind: StepValue, Result: []byte(`[1]`)})
		if _, ok, err := AwaitFor[string](run(j), "sig", time.Hour); !errors.Is(err, ErrProtocol) || ok {
			t.Errorf("= %v, %v; want ErrProtocol", ok, err)
		}
	})
	t.Run("payload of another type", func(t *testing.T) {
		j := memJournal()
		if err := j.Signal(ctx, "r", "sig", "text"); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := AwaitFor[int](run(j), "sig", time.Hour); !errors.Is(err, ErrProtocol) || ok {
			t.Errorf("= %v, %v; want ErrProtocol", ok, err)
		}
	})
	t.Run("deadline cannot be recorded", func(t *testing.T) {
		j := mustJournal(covInsertFail{NewMemStore(), awaitTimeoutStep("sig")})
		if _, ok, err := AwaitFor[string](run(j), "sig", time.Hour); !errors.Is(err, ErrStorage) || ok {
			t.Errorf("= %v, %v; want ErrStorage", ok, err)
		}
	})
	t.Run("race cannot be resolved", func(t *testing.T) {
		j := mustJournal(covInsertFail{NewMemStore(), awaitResolvedStep("sig")})
		now := time.Now()
		// The deadline has passed, so the timeout would be recorded; the store refuses it.
		rc := contextWithClock(run(j), func() time.Time { return now })
		if _, _, err := AwaitFor[string](rc, "sig", -time.Hour); !errors.Is(err, ErrStorage) {
			t.Errorf("= %v; want ErrStorage", err)
		}
	})
	t.Run("history unreadable", func(t *testing.T) {
		fj := mustJournal(loadFails{NewMemStore()})
		if _, _, err := AwaitFor[string](run(fj), "sig", time.Hour); !errors.Is(err, ErrStorage) {
			t.Errorf("= %v; want ErrStorage", err)
		}
	})
}

// corruptEntry stores bytes that do not decode as run r's entry name, after the run's header.
func corruptEntry(t *testing.T, s Store, j *Journal, r, name string) {
	t.Helper()
	if err := j.ensureHeader(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(context.Background(), r, name, []byte(`{"name":`)); err != nil {
		t.Fatal(err)
	}
}

// The first end marker in journal order is a run's end whichever writer asks: writeEnd reports an
// earlier marker over its own, firstEndOf picks the lowest position, and a marker of another kind
// than a value is no end.
func TestEndMarkers_FirstInJournalOrderWins(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	if got, err := writeEnd(ctx, j, "r", runCompleteStep, Record{Kind: StepValue, Result: []byte(`"done"`)}, []string{runCancelledStep}); err != nil || got.name != runCompleteStep {
		t.Fatalf("first writeEnd = %+v, %v", got, err)
	}
	got, err := writeEnd(ctx, j, "r", runCancelledStep, Record{Kind: StepValue, Result: []byte(`{"reason":"late"}`)}, []string{runCompleteStep})
	if err != nil || got.name != runCompleteStep || string(got.rec.Result) != `"done"` {
		t.Fatalf("later writeEnd = %+v, %v; want the earlier complete marker reported", got, err)
	}
	end, ok, err := firstEndOf(ctx, j, "r", runCancelledStep, runCompleteStep)
	if err != nil || !ok || end.name != runCompleteStep {
		t.Fatalf("firstEndOf = %+v, %v, %v; want complete", end, ok, err)
	}
	if c, err := cancelledFirst(ctx, j, "r"); err != nil || c {
		t.Fatalf("cancelledFirst = %v, %v; want false (complete came first)", c, err)
	}
	// A record of another kind under an end marker's name is no end of the run.
	j2 := memJournal()
	writeRaw(t, j2, "r", runCompleteStep, Record{Kind: StepApproval, ToolUseID: "x"})
	if _, ok, err := firstEndOf(ctx, j2, "r", runCompleteStep); err != nil || ok {
		t.Fatalf("firstEndOf over a non-value record = %v, %v; want none", ok, err)
	}
}

// A marker that cannot be read or decoded is an error, never a guess about which end came first.
func TestEndMarkers_StoreFaults(t *testing.T) {
	ctx := context.Background()
	t.Run("read-back fails", func(t *testing.T) {
		j := mustJournal(covGetFail{NewMemStore(), runCancelledStep})
		if _, err := writeEnd(ctx, j, "r", runCompleteStep, Record{Kind: StepValue}, []string{runCancelledStep}); !errors.Is(err, ErrStorage) {
			t.Fatalf("writeEnd = %v; want ErrStorage", err)
		}
	})
	t.Run("other marker corrupt", func(t *testing.T) {
		s := NewMemStore()
		j := mustJournal(s)
		corruptEntry(t, s, j, "r", runCancelledStep)
		if _, err := writeEnd(ctx, j, "r", runCompleteStep, Record{Kind: StepValue}, []string{runCancelledStep}); !errors.Is(err, ErrStorage) {
			t.Fatalf("writeEnd = %v; want ErrStorage", err)
		}
		if _, _, err := firstEndOf(ctx, j, "r", runCancelledStep); !errors.Is(err, ErrStorage) {
			t.Fatalf("firstEndOf = %v; want ErrStorage", err)
		}
	})
	t.Run("firstEndOf read fails", func(t *testing.T) {
		j := mustJournal(covGetFail{NewMemStore(), runCompleteStep})
		if _, _, err := firstEndOf(ctx, j, "r", runCompleteStep); !errors.Is(err, ErrStorage) {
			t.Fatalf("firstEndOf = %v; want ErrStorage", err)
		}
	})
	t.Run("cancelledFirst read fails", func(t *testing.T) {
		s := NewMemStore()
		writeRaw(t, mustJournal(s), "r", runCancelledStep, Record{Kind: StepValue})
		j := mustJournal(covGetFail{s, runCompleteStep})
		if _, err := cancelledFirst(ctx, j, "r"); !errors.Is(err, ErrStorage) {
			t.Fatalf("cancelledFirst = %v; want ErrStorage", err)
		}
	})
	t.Run("marker cannot be written", func(t *testing.T) {
		j := mustJournal(covInsertFail{NewMemStore(), runCompleteStep})
		if _, err := writeEnd(ctx, j, "r", runCompleteStep, Record{Kind: StepValue}, nil); !errors.Is(err, ErrStorage) {
			t.Fatalf("writeEnd = %v; want ErrStorage", err)
		}
	})
	t.Run("unencodable marker", func(t *testing.T) {
		if _, _, err := memJournal().putEntry(ctx, "r", runCompleteStep, Record{Kind: StepValue, Result: []byte(`{bad`)}); !errors.Is(err, ErrStorage) {
			t.Fatalf("putEntry = %v; want ErrStorage", err)
		}
	})
}

// A drive that finds its run over reports the end as the run's: the recorded answer of a completed
// run, ErrRunCancelled for a cancelled one (and for none, which only a cancelled read asks about),
// and ErrConfig for a saga's finished abort driven as a plain run.
func TestEndVerdict(t *testing.T) {
	ctx := context.Background()
	mk := func(name string, result string) *Agent {
		j := memJournal()
		if name != "" {
			writeRaw(t, j, "r", name, Record{Kind: StepValue, Result: []byte(result)})
		}
		return mustNew(&scriptModel{}, j)
	}
	if _, err := mk("", "").endVerdict(ctx, "r"); !errors.Is(err, ErrRunCancelled) {
		t.Errorf("no end marker = %v; want ErrRunCancelled", err)
	}
	if _, err := mk(runCancelledStep, `{"reason":"x"}`).endVerdict(ctx, "r"); !errors.Is(err, ErrRunCancelled) {
		t.Errorf("cancelled = %v; want ErrRunCancelled", err)
	}
	if _, err := mk(runAbortedStep, `"boom"`).endVerdict(ctx, "r"); !errors.Is(err, ErrConfig) {
		t.Errorf("aborted = %v; want ErrConfig", err)
	}
	a := mustNew(&scriptModel{}, mustJournal(loadFails{NewMemStore()}))
	if _, err := a.endVerdict(ctx, "r"); !errors.Is(err, ErrStorage) {
		t.Errorf("unreadable history = %v; want ErrStorage", err)
	}
}

// cancelTrip names why a saga was cancelled.
func TestCancelTripError(t *testing.T) {
	if got := (&cancelTrip{reason: "stop it"}).Error(); got != "saga cancelled: stop it" {
		t.Errorf("Error = %q", got)
	}
	if got := (&sagaTrip{toolName: "pay", cause: errors.New("declined")}).Error(); got != `saga step "pay" failed: declined` {
		t.Errorf("sagaTrip.Error = %q", got)
	}
}
