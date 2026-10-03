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

func (t *awaitForTool) Name() string                { return t.name }
func (t *awaitForTool) Description() string         { return "" }
func (t *awaitForTool) Safety() Safety              { return t.safety }
func (t *awaitForTool) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
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
		t.Fatalf("err = %v, want *Awaiting", err)
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
		t.Fatalf("err = %v, want *Awaiting", err)
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
		t.Fatalf("first run should pause with *Awaiting, got %v", err)
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
