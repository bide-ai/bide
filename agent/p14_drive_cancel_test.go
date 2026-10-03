package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// The P14 contract's drive rules for Cancel (docs/design/api-v1.md, "The P14 contract (model 10)"):
// rules 2, 3, the drive's side of rule 4, and the drive's side of rule 5. Cancel itself is
// simulated by writing its marker through the store, as another process would.

// Rule 2: a drive checks run:cancelled when it starts (from its Load) and at every turn boundary,
// and starts no claim after seeing it.
func TestP14Rule02_DriveChecksCancelAtStart(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	var c counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(c.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	res, err := a.Run(ctx, "r", agent.UserText("go"))
	if _, ok := errors.AsType[*agent.ApprovalPending](err); !ok {
		t.Fatalf("first drive = %v, want the approval pause", err)
	}
	if res == nil {
		t.Fatal("no Result on a pause")
	}
	writeMarker(t, m, "r", "run:cancelled", reason{"stop"})
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	before := model.calls.Load()
	res, err = a.Run(ctx, "r", agent.UserText("go"))
	if !errors.Is(err, agent.ErrRunCancelled) || res == nil {
		t.Fatalf("drive of a cancelled run = %v, %v; want ErrRunCancelled with a Result", res, err)
	}
	if n := c.n.Load(); n != 0 {
		t.Fatalf("the approved call ran %d times after run:cancelled", n)
	}
	if model.calls.Load() != before || has(t, m, "r", "attempt:tool:c1") {
		t.Fatal("a cancelled run called the model or claimed a call")
	}
	// Whatever input it is given, as a finished run.
	if _, err := a.Run(ctx, "r", agent.UserText("other")); !errors.Is(err, agent.ErrRunCancelled) {
		t.Fatalf("drive with another input = %v, want ErrRunCancelled", err)
	}
}

func TestP14Rule02_DriveChecksCancelAtTurnBoundary(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	var c counter
	lookup := agent.Func("lookup", "", agent.Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) {
		writeMarker(t, m, "r", "run:cancelled", reason{"stop"}) // Cancel lands while the turn's call runs
		return "ok", nil
	})
	model := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "lookup")}},
		{calls: []agent.ToolUse{call("c2", "pay")}},
		{text: "done"},
	}}
	a := p14Build(t, model, j, agent.WithTools(lookup, c.tool("pay", agent.Safety{})))
	res, err := a.Run(ctx, "r", agent.UserText("go"))
	if !errors.Is(err, agent.ErrRunCancelled) || res == nil {
		t.Fatalf("run = %v, %v; want ErrRunCancelled with a Result", res, err)
	}
	if n := model.calls.Load(); n != 1 {
		t.Fatalf("the model was called %d times; the turn boundary after run:cancelled must stop the run", n)
	}
	if c.n.Load() != 0 || has(t, m, "r", "run:complete") {
		t.Fatal("the run went on past run:cancelled")
	}
}

// Rule 3 (L2, model 10's regress/cancel-turn-check): Cancel lands after the drive's turn check and
// before its claim. The drive reads run:cancelled again once the claim is won, records the attempt
// as not started, and stops: the effect never fires.
func TestP14Rule03_CheckAfterWonClaim(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	var c counter
	model := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "pay")}, hook: func(agent.Request) {
			writeMarker(t, m, "r", "run:cancelled", reason{"stop"}) // after the turn check, before the claim
		}},
		{text: "done"},
	}}
	a := p14Build(t, model, j, agent.WithTools(c.tool("pay", agent.Safety{})))
	res, err := a.Run(ctx, "r", agent.UserText("go"))
	if !errors.Is(err, agent.ErrRunCancelled) || res == nil {
		t.Fatalf("run = %v, %v; want ErrRunCancelled with a Result", res, err)
	}
	if n := c.n.Load(); n != 0 {
		t.Fatalf("the side effect fired %d times under a claim won after run:cancelled", n)
	}
	if !has(t, m, "r", "attempt:tool:c1") {
		t.Fatal("no claim: the test did not reach the post-claim check")
	}
	notStarted := false
	for r, err := range j.Records(ctx, "r") {
		if err != nil {
			t.Fatal(err)
		}
		notStarted = notStarted || strings.HasPrefix(r.Name, "attempt:not-started:")
	}
	if !notStarted {
		t.Fatal("the won claim was not recorded as not started")
	}
}

// Rule 3 keeps D1's "calls already in flight finish": a call past its post-claim check when Cancel
// lands is called, and its result recorded.
func TestP14Rule03_InFlightCallFinishes(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	pay := agent.Func("pay", "", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		writeMarker(t, m, "r", "run:cancelled", reason{"stop"}) // lands while the call runs
		return "paid", nil
	})
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(pay))
	if _, err := a.Run(ctx, "r", agent.UserText("go")); !errors.Is(err, agent.ErrRunCancelled) {
		t.Fatalf("run = %v, want ErrRunCancelled", err)
	}
	if !has(t, m, "r", "tool:c1") {
		t.Fatal("the call in flight did not record its result")
	}
}

// Rule 4 (L3, model 10's regress/cancel-verdict), the drive's side: Cancel lands after the drive's
// last turn check and before its run:complete. Both markers land; run:cancelled is first in
// journal order, so the drive reports the run cancelled, and so does every later drive.
func TestP14Rule04_DriveReadsBackEndMarkers(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	model := &p14Model{turns: []p14Turn{{text: "answer", hook: func(agent.Request) {
		writeMarker(t, m, "r", "run:cancelled", reason{"stop"})
	}}}}
	a := p14Build(t, model, j)
	res, err := a.Run(ctx, "r", agent.UserText("go"))
	if !errors.Is(err, agent.ErrRunCancelled) || res == nil {
		t.Fatalf("run = %v, %v; want ErrRunCancelled: run:cancelled is the first end marker", res, err)
	}
	if res.Message.Text() != "" {
		t.Fatalf("a cancelled run's Result carries the answer %q", res.Message.Text())
	}
	if _, err := a.Run(ctx, "r", agent.UserText("go")); !errors.Is(err, agent.ErrRunCancelled) {
		t.Fatalf("later drive = %v, want ErrRunCancelled", err)
	}
}

// Rule 4 the other way: run:complete is first, then run:cancelled lands. Every reader reports the
// run completed.
func TestP14Rule04_CompleteFirstIsComplete(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	model := &p14Model{turns: []p14Turn{{text: "answer"}}}
	a := p14Build(t, model, j)
	if _, err := a.Run(ctx, "r", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, m, "r", "run:cancelled", reason{"late"})
	res, err := a.Run(ctx, "r", agent.UserText("go"))
	if err != nil || res.Message.Text() != "answer" {
		t.Fatalf("drive = %v, %v; want the recorded answer", res, err)
	}
}

// Rule 5 (L4, model 10's findings/cancel-saga-marker), the drive's side: a saga's rollback request
// run:cancel-requested is not an end marker; the drive that sees it rolls the run back and writes
// run:cancelled, and reports the run cancelled.
func TestP14Rule05_SagaRollbackRequest(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	var undone counter
	book := agent.CompensatedFunc("book", "", agent.Safety{},
		func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { undone.n.Add(1); return nil })
	var c counter
	model := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "book")}},
		{calls: []agent.ToolUse{call("c2", "pay")}},
		{text: "done"},
	}}
	a := p14Build(t, model, j, agent.WithTools(book, c.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithSaga()); err == nil {
		t.Fatal("the saga did not pause for the approval")
	}
	writeMarker(t, m, "r", "run:cancel-requested", reason{"stop"})
	res, err := a.Resume(ctx, "r")
	if !errors.Is(err, agent.ErrRunCancelled) || res == nil {
		t.Fatalf("drive of a saga with a rollback request = %v, %v; want ErrRunCancelled", res, err)
	}
	if undone.n.Load() != 1 || c.n.Load() != 0 {
		t.Fatalf("compensated %d, paid %d; want the booking undone and nothing paid", undone.n.Load(), c.n.Load())
	}
	if !has(t, m, "r", "run:cancelled") || has(t, m, "r", "run:aborted") {
		t.Fatal("a cancelled saga's final marker must be run:cancelled, not run:aborted")
	}
}

// Rule 5 at a turn boundary: the request lands while the saga runs; the drive rolls back at its
// next check.
func TestP14Rule05_SagaRequestAtTurnBoundary(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	var undone counter
	book := agent.CompensatedFunc("book", "", agent.Safety{},
		func(context.Context, struct{}) (string, error) {
			writeMarker(t, m, "r", "run:cancel-requested", reason{"stop"})
			return "booked", nil
		},
		func(context.Context, struct{}, string) error { undone.n.Add(1); return nil })
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "book")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(book))
	_, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithSaga())
	if !errors.Is(err, agent.ErrRunCancelled) {
		t.Fatalf("saga = %v, want ErrRunCancelled", err)
	}
	if undone.n.Load() != 1 || model.calls.Load() != 1 || !has(t, m, "r", "run:cancelled") {
		t.Fatalf("undone %d, model calls %d; want the rollback at the turn boundary", undone.n.Load(), model.calls.Load())
	}
}

// Rule 5: a saga whose final answer is recorded before the request is seen completes.
func TestP14Rule05_SagaCompleteBeforeRequest(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	model := &p14Model{turns: []p14Turn{{text: "done"}}}
	a := p14Build(t, model, j)
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithSaga()); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, m, "r", "run:cancel-requested", reason{"late"})
	res, err := a.Resume(ctx, "r")
	if err != nil || res.Message.Text() != "done" {
		t.Fatalf("resume = %v, %v; want the completed answer", res, err)
	}
}
