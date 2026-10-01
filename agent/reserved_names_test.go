package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// A developer-chosen step name shares the run's journal with the engine's own keys, so a name
// with an engine prefix is refused. On main a Step named "run:complete" wrote the completion
// marker, and IsComplete reported a run that never finished as complete.
func TestStep_ReservedNameIsRefused(t *testing.T) {
	for _, name := range []string{"run:complete", "run:aborted", "@llm/0", "@saga/compensate/x", "@spend/0",
		"tool:x", "attempt:step:x", "attempt:tool:x", "approval:x", "approval-tally:x", "signal:x",
		"await-timeout:x", "await-resolved:x", "timer:x", "interrupt:x", "chan:1:c:k", "chanack:1:c:k",
		"turn/0", "start/0", "from/x", "audit:policy:x"} {
		store := NewMemStore()
		ran := false
		_, err := Step(context.Background(), store, "r", name, func(context.Context) (string, error) { ran = true; return "v", nil })
		if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), name) || ran {
			complete, _ := IsComplete(context.Background(), store, "r")
			t.Fatalf("Step %q: err = %v, ran = %v (IsComplete %v); want ErrConfig naming it, not run", name, err, ran, complete)
		}
	}
}

// Parallel refuses a reserved task name before any task runs.
func TestParallel_ReservedTaskNameIsRefused(t *testing.T) {
	var ran int
	task := func(context.Context) (int, error) { ran++; return 1, nil }
	_, err := Parallel(context.Background(), NewMemStore(), "r", []Task[int]{
		{Name: "fine", Fn: task, Safety: Safety{ReadOnly: true}},
		{Name: "@llm/0", Fn: task, Safety: Safety{ReadOnly: true}}})
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "@llm/0") || ran != 0 {
		t.Fatalf("err = %v, %d tasks ran; want ErrConfig naming %q and none run", err, ran, "@llm/0")
	}
}

// A sub-agent's run is "<parent>><call>", so a top-level run ID with a '>' could name one; it is
// refused. A '/' stays allowed ("tenant/123").
func TestRun_RunIDWithSubRunSeparatorIsRefused(t *testing.T) {
	a := New(NewScriptedModel(TextTurn("done")), NewMemStore())
	for _, run := range []func(string) error{
		func(id string) error { _, err := a.Run(context.Background(), id, "go"); return err },
		func(id string) error { _, err := a.RunSaga(context.Background(), id, "go"); return err },
		func(id string) error { _, err := a.RunResult(context.Background(), id, "go"); return err },
		func(id string) error { _, err := a.Stream(context.Background(), id, "go").Final(); return err },
	} {
		if err := run("tenant>1"); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "tenant>1") {
			t.Fatalf("err = %v, want ErrConfig naming the run ID", err)
		}
	}
	b := New(NewScriptedModel(TextTurn("done")), NewMemStore())
	if out, err := b.Run(context.Background(), "tenant/123", "go"); err != nil || out.Text() != "done" {
		t.Fatalf("run tenant/123: %q, %v", out.Text(), err)
	}
}

// A session's run IDs are "<id>>@...", so a session ID with a '>' is refused. (A SendOnce key is
// encoded, so it may hold one; see TestSendOnce_AnyKeyGetsItsOwnTurn.)
func TestSession_IDWithSubRunSeparatorIsRefused(t *testing.T) {
	a := New(NewScriptedModel(TextTurn("done")), NewMemStore())
	if _, err := a.Session(context.Background(), "chat>1"); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "chat>1") {
		t.Fatalf("Session err = %v, want ErrConfig naming the id", err)
	}
}

// Recover leaves a sub-agent's run to its root: the root's re-run resumes it, and the sub-run
// alone cannot be driven by the root agent's resume callback.
func TestRecover_SkipsSubRuns(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	charge := Func("charge", "charge a card", Safety{},
		func(context.Context, struct{}) (string, error) { return "charged", nil }, WithApproval(SingleApproval()))
	root := clerkTree(store, charge)
	var pa *PendingApproval
	if _, err := root.Run(ctx, "p", "go"); !errors.As(err, &pa) {
		t.Fatalf("run: %v, want a pending approval inside the sub-agent", err)
	}
	var asked []string
	if _, err := Recover(ctx, store, func(ctx context.Context, runID string, _ RunStart) error {
		asked = append(asked, runID)
		_, err := root.Run(ctx, runID, "go")
		return err
	}); err != nil {
		t.Fatalf("Recover: %v (asked %v)", err, asked)
	}
	if !slices.Equal(asked, []string{"p"}) {
		t.Fatalf("Recover asked to re-drive %v, want only the root run p", asked)
	}
}
