package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// B1 (model 10's regress/options-from-caller and regress/limits-from-caller): a token budget and a
// system prompt passed per run survive RecoverLoop. The recovery drive passes no option
// (ResumeAgent) and runs under the run's journaled options, not the agent's defaults (no budget,
// no prompt), so the budget stops it.
func TestP14_B1_PerRunOptionsSurviveRecoverLoop(t *testing.T) {
	j, m := p14Journal(t)
	var pay counter
	model := &usageModel{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "pay")}},
		{calls: []agent.ToolUse{call("c2", "lookup")}},
		{text: "done"},
	}}
	a := p14Build(t, model, j, agent.WithTools(
		pay.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval())),
		agent.MustFunc("lookup", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))))
	ctx := context.Background()
	_, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithTokenBudget(30), agent.WithSystemPrompt("per-run"))
	if _, ok := errors.AsType[*agent.ApprovalPending](err); !ok {
		t.Fatalf("first drive = %v, want the approval pause", err)
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	// The process that started the run is gone; a worker's RecoverLoop takes the run over.
	lctx, cancel := context.WithCancel(ctx)
	errs := make(chan error, 8)
	done := make(chan error, 1)
	go func() {
		done <- agent.RecoverLoop(lctx, j, agent.ResumeAgent(a), agent.WithRecoverInterval(time.Hour),
			agent.WithRecoverErrors(func(err error) { errs <- err }))
	}()
	var got error
	select {
	case got = <-errs:
	case <-time.After(30 * time.Second):
		t.Fatal("RecoverLoop reported nothing")
	}
	cancel()
	<-done
	if !errors.Is(got, agent.ErrBudgetExceeded) {
		t.Fatalf("recovery drive = %v, want ErrBudgetExceeded under the journaled budget", got)
	}
	if pay.n.Load() != 1 {
		t.Fatalf("pay ran %d times, want once", pay.n.Load())
	}
	if has(t, m, "r", "run:complete") {
		t.Fatal("the recovery drive ran past the run's budget")
	}
	if s := systemText(model.m.lastReq(t)); s != "per-run" {
		t.Fatalf("the recovery drive's system prompt = %q, want the journaled one", s)
	}
}

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
	lookup := agent.MustFunc("lookup", "", func(context.Context, struct{}) (string, error) {
		writeMarker(t, m, "r", "run:cancelled", reason{"stop"}) // Cancel lands while the turn's call runs
		return "ok", nil
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
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
	pay := agent.MustFunc("pay", "", func(context.Context, struct{}) (string, error) {
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
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
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
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) {
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

// Tests added by the mutation check of the P14 rules: each kills a mutant the rule tests above let
// survive.

// Rule 10: an amendment binds the drives after it that pass no option: here the limit is raised
// from 1 to 3 by a drive that only pauses, and the next drive, which passes nothing, runs three
// turns.
func TestP14Rule10_AmendmentBindsLaterDrives(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	a, model, _ := approvalAgent(t, j)
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithMaxTurns(1)); !agent.IsPause(err) {
		t.Fatalf("first drive = %v, want the approval pause", err)
	}
	if _, err := a.Resume(ctx, "r", agent.WithMaxTurns(3)); !agent.IsPause(err) {
		t.Fatalf("amending drive = %v, want the pause again", err)
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Resume(ctx, "r"); !errors.Is(err, agent.ErrMaxTurns) {
		t.Fatalf("drive with no option = %v, want ErrMaxTurns at the amended 3", err)
	}
	if n := model.calls.Load(); n != 3 {
		t.Fatalf("model calls = %d, want 3 under the amendment", n)
	}
}

// Rule 3: once a call's post-claim check finds the run cancelled, the turn's calls not yet started
// never start, a retry-safe one included.
func TestP14Rule03_SiblingsDoNotStartAfterCancel(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	var pay, look counter
	model := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "pay"), call("c2", "pay2"), call("c3", "lookup")}, hook: func(agent.Request) {
			writeMarker(t, m, "r", "run:cancelled", reason{"stop"})
		}},
		{text: "done"},
	}}
	a := p14Build(t, model, j, agent.WithTools(pay.tool("pay", agent.Safety{}), pay.tool("pay2", agent.Safety{}), look.tool("lookup", agent.Safety{ReadOnly: true})))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithMaxConcurrency(1)); !errors.Is(err, agent.ErrRunCancelled) {
		t.Fatalf("run = %v, want ErrRunCancelled", err)
	}
	if pay.n.Load() != 0 || look.n.Load() != 0 || has(t, m, "r", "attempt:tool:c2") {
		t.Fatalf("pay %d, lookup %d: a call started after the run was found cancelled", pay.n.Load(), look.n.Load())
	}
}

// failOnce fails the first Insert of name with an error that leaves nothing written.
type failOnce struct {
	agent.Store
	name string
	once sync.Once
}

func (s *failOnce) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	var fail bool
	if name == s.name {
		s.once.Do(func() { fail = true })
	}
	if fail {
		return agent.Entry{}, false, errors.New("store unavailable")
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// Rule 5: a saga whose final turn is recorded completes, even when the rollback request is seen
// before its run:complete is: here run:complete's first write failed, the request landed, and the
// next drive completes the run.
func TestP14Rule05_SagaRecordedAnswerBeatsALaterRequest(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	j, err := agent.NewJournal(&failOnce{Store: mem, name: "run:complete"})
	if err != nil {
		t.Fatal(err)
	}
	a := p14Build(t, &p14Model{turns: []p14Turn{{text: "done"}}}, j)
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithSaga()); !errors.Is(err, agent.ErrStorage) {
		t.Fatalf("first drive = %v, want the failed run:complete", err)
	}
	writeMarker(t, mem, "r", "run:cancel-requested", reason{"late"})
	res, err := a.Resume(ctx, "r")
	if err != nil || res.Message.Text() != "done" {
		t.Fatalf("resume = %v, %v; want the recorded answer", res, err)
	}
}

// Rule 9: a recovery Resumer passes no per-run option: one given a journaled setting is ErrConfig.
func TestP14Rule09_ResumerRefusesJournaledOptions(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	a, model, _ := approvalAgent(t, j)
	pauseThenApprove(t, a, j)
	start, _, err := agent.RecordedStart(ctx, j, "r")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []agent.RunOption{agent.WithMaxTurns(5), agent.WithTokenBudget(9), agent.WithSystemPrompt("x"),
		agent.WithToolFilter("pay"), agent.WithSaga(), agent.WithIdentity(agent.Identity{OnBehalfOf: "d"})} {
		if err := agent.ResumeAgent(a, o)(ctx, "r", start); !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("ResumeAgent with a journaled option = %v, want ErrConfig", err)
		}
	}
	if model.calls.Load() != 1 {
		t.Fatal("a refused Resumer drove the run")
	}
	if err := agent.ResumeAgent(a, agent.WithIdentity(agent.Identity{Actor: "worker-2"}))(ctx, "r", start); err != nil {
		t.Fatalf("ResumeAgent with the deployment's Actor = %v", err)
	}
}

// Rule 16 with model 10's L3 rule: an open turn whose run completed before Cancel landed is not
// cancelled (its first end marker is run:complete), so it is not closed, and another message is
// still refused until the turn's own message is sent again and recorded.
func TestP14Rule16_CompletedFirstIsNotClosed(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "pay")}}, {text: "answer"}}}
	var pay counter
	a := p14Build(t, model, j, agent.WithTools(pay.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	s, err := a.Session(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Send(ctx, agent.UserText("x"))
	pa, ok := errors.AsType[*agent.ApprovalPending](err)
	if !ok {
		t.Fatalf("Send(x) = %v, want the approval pause", err)
	}
	writeMarker(t, m, pa.RunID, "run:complete", nil)
	writeMarker(t, m, pa.RunID, "run:cancelled", reason{"late"})
	if _, err := s.Send(ctx, agent.UserText("y")); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Send(y) = %v, want ErrConfig: x's turn completed, it was not cancelled", err)
	}
}

// The P14 contract's journaling rules (rules 8 to 13), the principal, typed runs, image input and
// a Result on every error kind.

func ptr[T any](v T) *T { return &v }

// systemText is the text of the system message a request starts with, if any.
func systemText(req agent.Request) string {
	if len(req.Messages) > 0 && req.Messages[0].Role == agent.RoleSystem {
		return req.Messages[0].Text()
	}
	return ""
}

// racingStore inserts other's run:start just before the first Insert of run:start it is asked
// for, as a concurrent first drive would.
type racingStore struct {
	agent.Store
	once  sync.Once
	other []byte
}

func (s *racingStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if name == "run:start" {
		s.once.Do(func() { _, _, _ = s.Store.Insert(ctx, runID, name, s.other) })
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// Rule 8: the first drive inserts run:start with its caller's options and runs under the stored
// entry the insert returns. Here a concurrent first drive's run:start lands first, with a system
// prompt and a one-turn limit; this drive passed neither, and runs under them.
func TestP14Rule08_FirstDriveRunsUnderStoredStart(t *testing.T) {
	ctx := context.Background()
	other := agent.RunStart{Input: agent.UserText("go"), Settings: agent.RunSettings{MaxTurns: ptr(1), SystemPrompt: ptr("theirs")}}
	b, _ := json.Marshal(other)
	data, err := agent.JournalEntry("run:start", agent.Record{Kind: agent.StepValue, Result: b})
	if err != nil {
		t.Fatal(err)
	}
	rs := &racingStore{Store: agent.NewMemStore(), other: data}
	j, err := agent.NewJournal(rs)
	if err != nil {
		t.Fatal(err)
	}
	var c counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "lookup")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(c.tool("lookup", agent.Safety{ReadOnly: true})), agent.WithSystemPrompt("mine"))
	_, err = a.Run(ctx, "r", agent.UserText("go"))
	if !errors.Is(err, agent.ErrMaxTurns) {
		t.Fatalf("run = %v, want ErrMaxTurns under the stored one-turn limit", err)
	}
	if got := systemText(model.lastReq(t)); got != "theirs" {
		t.Fatalf("system prompt = %q, want the stored run:start's", got)
	}
}

// approvalAgent builds an agent whose model calls pay (gated by an approval), then lookup, then
// lookup again, then answers; it returns the agent, its model and the pay counter.
func approvalAgent(t *testing.T, j *agent.Journal, opts ...agent.Option) (*agent.Agent, *p14Model, *counter) {
	t.Helper()
	var c counter
	model := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "pay")}},
		{calls: []agent.ToolUse{call("c2", "lookup")}},
		{calls: []agent.ToolUse{call("c3", "lookup")}},
		{text: "done"},
	}}
	tools := agent.WithTools(c.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval())),
		agent.MustFunc("lookup", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true})))
	return p14Build(t, model, j, append([]agent.Option{tools}, opts...)...), model, &c
}

// pauseThenApprove runs a's first drive of "r" with opts to its approval pause and approves.
func pauseThenApprove(t *testing.T, a *agent.Agent, j *agent.Journal, opts ...agent.RunOption) {
	t.Helper()
	ctx := context.Background()
	if _, err := a.Run(ctx, "r", agent.UserText("go"), opts...); err == nil {
		t.Fatal("the first drive did not pause")
	} else if _, ok := errors.AsType[*agent.ApprovalPending](err); !ok {
		t.Fatalf("first drive = %v, want the approval pause", err)
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
}

// Rule 9: every later drive runs under run:start's options, whatever the agent's defaults are and
// whatever entry point resumes it.
func TestP14Rule09_LaterDrivesRunUnderJournaledOptions(t *testing.T) {
	for _, entry := range []string{"Resume", "Run"} {
		t.Run(entry, func(t *testing.T) {
			ctx := context.Background()
			j, _ := p14Journal(t)
			a, model, _ := approvalAgent(t, j, agent.WithSystemPrompt("agent"), agent.WithMaxTurns(10), agent.WithSampling(agent.Temperature(0.9)))
			pauseThenApprove(t, a, j, agent.WithSystemPrompt("per-run"), agent.WithMaxTurns(2), agent.WithSampling(agent.Temperature(0.3)))
			var err error
			if entry == "Resume" {
				_, err = a.Resume(ctx, "r")
			} else {
				_, err = a.Run(ctx, "r", agent.UserText("go"))
			}
			if !errors.Is(err, agent.ErrMaxTurns) {
				t.Fatalf("resume = %v, want ErrMaxTurns under the journaled limit of 2", err)
			}
			req := model.lastReq(t)
			if got := systemText(req); got != "per-run" {
				t.Fatalf("system prompt = %q, want the journaled one", got)
			}
			if req.Sampling.Temperature == nil || *req.Sampling.Temperature != 0.3 {
				t.Fatalf("temperature = %v, want the journaled 0.3", req.Sampling.Temperature)
			}
		})
	}
}

// Resume of a run with no run:start is ErrNotStarted, and drives nothing.
func TestP14Rule09_ResumeOfUnstartedRun(t *testing.T) {
	j, _ := p14Journal(t)
	model := &p14Model{turns: []p14Turn{{text: "done"}}}
	a := p14Build(t, model, j)
	res, err := a.Resume(context.Background(), "nope")
	if !errors.Is(err, agent.ErrNotStarted) || res == nil || model.calls.Load() != 0 {
		t.Fatalf("Resume = %v, %v (model calls %d); want ErrNotStarted with a Result", res, err, model.calls.Load())
	}
}

// Rule 10: a later drive with a different limit writes run:limits:<n> before it drives and runs
// under it, and the drives after it run under the amendment.
func TestP14Rule10_LimitAmendment(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	model := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "lookup")}},
		{calls: []agent.ToolUse{call("c2", "lookup")}},
		{calls: []agent.ToolUse{call("c3", "lookup")}},
		{text: "done"},
	}}
	var c counter
	a := p14Build(t, model, j, agent.WithTools(c.tool("lookup", agent.Safety{ReadOnly: true})))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithMaxTurns(1)); !errors.Is(err, agent.ErrMaxTurns) {
		t.Fatalf("first drive = %v, want ErrMaxTurns", err)
	}
	if _, err := a.Resume(ctx, "r", agent.WithMaxTurns(2)); !errors.Is(err, agent.ErrMaxTurns) {
		t.Fatalf("raised to 2 = %v, want ErrMaxTurns at 2", err)
	}
	if !has(t, m, "r", "run:limits:0") {
		t.Fatal("the raise was not journaled as run:limits:0")
	}
	// A drive that passes nothing runs under the amendment, not the agent's unbounded default.
	if _, err := a.Resume(ctx, "r"); !errors.Is(err, agent.ErrMaxTurns) {
		t.Fatalf("drive with no option = %v, want ErrMaxTurns under the amendment", err)
	}
	if n := model.calls.Load(); n != 2 {
		t.Fatalf("model calls = %d, want 2", n)
	}
	res, err := a.Resume(ctx, "r", agent.WithMaxTurns(4))
	if err != nil || res.Message.Text() != "done" {
		t.Fatalf("raised to 4 = %v, %v; want the answer", res, err)
	}
	if !has(t, m, "r", "run:limits:1") {
		t.Fatal("the second raise was not journaled as run:limits:1")
	}
	// The same limit is no amendment.
	if _, err := a.Resume(ctx, "r", agent.WithMaxTurns(4)); err != nil || has(t, m, "r", "run:limits:2") {
		t.Fatalf("repeat of the limit = %v, or it wrote an amendment", err)
	}
}

// Rule 10 for the token budget: an amendment raises a budget the run spent.
func TestP14Rule10_TokenBudgetAmendment(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	model := &usageModel{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "lookup")}},
		{text: "done"},
	}}
	var c counter
	a := p14Build(t, model, j, agent.WithTools(c.tool("lookup", agent.Safety{ReadOnly: true})))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithTokenBudget(10)); !errors.Is(err, agent.ErrBudgetExceeded) {
		t.Fatalf("first drive = %v, want ErrBudgetExceeded", err)
	}
	if _, err := a.Resume(ctx, "r"); !errors.Is(err, agent.ErrBudgetExceeded) {
		t.Fatalf("drive with no option = %v, want the journaled budget", err)
	}
	res, err := a.Resume(ctx, "r", agent.WithTokenBudget(1000))
	if err != nil || res.Message.Text() != "done" || !has(t, m, "r", "run:limits:0") {
		t.Fatalf("raised budget = %v, %v; want the answer and run:limits:0", res, err)
	}
}

// usageModel is a p14Model whose every turn reports 20 tokens.
type usageModel struct {
	turns []p14Turn
	m     p14Model
}

func (u *usageModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	u.m.turns = u.turns
	s, err := u.m.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan agent.Emit, 8)
	go func() {
		defer close(ch)
		for e, err := range s.Events() {
			if err != nil {
				ch <- agent.Emit{Err: err}
				return
			}
			if f, ok := e.(agent.Finish); ok {
				f.Usage = agent.Usage{InputTokens: 10, OutputTokens: 10}
				e = f
			}
			ch <- agent.Emit{Event: e}
		}
	}()
	return agent.NewStream(ch), nil
}

// Rule 11: a later drive with any other different setting is ErrConfig, before any model call.
func TestP14Rule11_OtherMismatchesAreErrConfig(t *testing.T) {
	first := []agent.RunOption{
		agent.WithSystemPrompt("P"), agent.WithSampling(agent.Temperature(0.3)),
		agent.WithToolChoice(agent.ToolChoice{Mode: "auto"}), agent.WithToolFilter("pay", "lookup"),
		agent.WithIdentity(agent.Identity{Actor: "a1", OnBehalfOf: "desk", AuthorityRef: "g1"}),
	}
	for _, tc := range []struct {
		name  string
		input string
		opts  []agent.RunOption
	}{
		{"input", "other", nil},
		{"system prompt", "go", []agent.RunOption{agent.WithSystemPrompt("Q")}},
		{"sampling", "go", []agent.RunOption{agent.WithSampling(agent.Temperature(0.4))}},
		{"tool choice", "go", []agent.RunOption{agent.WithToolChoice(agent.ToolChoice{Mode: "none"})}},
		{"tool filter", "go", []agent.RunOption{agent.WithToolFilter("lookup")}},
		{"principal", "go", []agent.RunOption{agent.WithIdentity(agent.Identity{OnBehalfOf: "other desk"})}},
		{"saga", "go", []agent.RunOption{agent.WithSaga()}},
		{"output mode on an untyped run", "go", []agent.RunOption{agent.WithOutputMode(agent.OutputNative)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			j, _ := p14Journal(t)
			a, model, pay := approvalAgent(t, j)
			pauseThenApprove(t, a, j, first...)
			before := model.calls.Load()
			res, err := a.Run(ctx, "r", agent.UserText(tc.input), tc.opts...)
			if !errors.Is(err, agent.ErrConfig) || res == nil {
				t.Fatalf("drive = %v, %v; want ErrConfig with a Result", res, err)
			}
			if model.calls.Load() != before || pay.n.Load() != 0 {
				t.Fatal("a mismatched drive called the model or a tool")
			}
		})
	}
	// The same values are no mismatch.
	ctx := context.Background()
	j, _ := p14Journal(t)
	a, _, _ := approvalAgent(t, j)
	pauseThenApprove(t, a, j, first...)
	if _, err := a.Run(ctx, "r", agent.UserText("go"), first...); err != nil {
		t.Fatalf("drive with the journaled values = %v, want the answer", err)
	}
}

// Rule 12: the turn limit is checked against the drive's journaled limit when it starts, before
// any model call, whatever the agent's own limit is.
func TestP14Rule12_TurnLimitAtStart(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	a, model, pay := approvalAgent(t, j, agent.WithMaxTurns(10))
	pauseThenApprove(t, a, j, agent.WithMaxTurns(1))
	_, err := a.Resume(ctx, "r")
	if !errors.Is(err, agent.ErrMaxTurns) {
		t.Fatalf("resume = %v, want ErrMaxTurns", err)
	}
	if model.calls.Load() != 1 || pay.n.Load() != 1 {
		t.Fatalf("model calls %d, pay %d; want the pending call run and no second turn", model.calls.Load(), pay.n.Load())
	}
}

// Rule 13 (L5, model 10's findings/filter-request-only): the filter narrows the tools the model
// is offered, and a call outside it is refused at dispatch with an error result recorded.
func TestP14Rule13_FilterEnforcedAtDispatch(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	var pay, look counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(pay.tool("pay", agent.Safety{}), look.tool("lookup", agent.Safety{ReadOnly: true})))
	res, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithToolFilter("lookup"))
	if err != nil || res.Message.Text() != "done" {
		t.Fatalf("run = %v, %v", res, err)
	}
	if pay.n.Load() != 0 {
		t.Fatal("a tool outside the filter ran")
	}
	if names := toolNames(model.lastReq(t)); !slices.Equal(names, []string{"lookup"}) {
		t.Fatalf("offered tools = %v, want [lookup]", names)
	}
	rec, ok, err := j.Get(ctx, "r", "tool:c1")
	if err != nil || !ok || !rec.IsError {
		t.Fatalf("refused call's record = %+v, %v, %v; want an error result", rec, ok, err)
	}
	_ = m
}

// Rule 13 for a turn replayed from the journal, dispatched by a later drive that passes no filter:
// the journaled filter still refuses the call.
func TestP14Rule13_FilterHoldsOnResume(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var pay counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "lookup"), call("c2", "pay")}}, {text: "done"}}}
	gated := agent.MustFunc("lookup", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}),
		agent.WithApproval(agent.SingleApproval()))
	a := p14Build(t, model, j, agent.WithTools(pay.tool("pay", agent.Safety{}), gated))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithToolFilter("lookup")); err == nil {
		t.Fatal("the first drive did not pause")
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Resume(ctx, "r"); err != nil {
		t.Fatalf("resume = %v", err)
	}
	if pay.n.Load() != 0 {
		t.Fatal("the journaled turn's call outside the filter ran on resume")
	}
}

func toolNames(req agent.Request) []string {
	var out []string
	for _, s := range req.Tools {
		out = append(out, s.Name)
	}
	return out
}

// The principal is journaled and restored; the Actor stays live.
func TestP14_PrincipalRestoredActorLive(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var seen agent.Identity
	who := agent.MustFunc("who", "", func(ctx context.Context, _ struct{}) (string, error) {
		seen, _ = agent.IdentityFrom(ctx)
		return "ok", nil
	}, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithApproval(agent.SingleApproval()))
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "who")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(who))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithIdentity(agent.Identity{Actor: "v1", OnBehalfOf: "desk", AuthorityRef: "grant-1"})); err == nil {
		t.Fatal("no pause")
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Resume(ctx, "r", agent.WithIdentity(agent.Identity{OnBehalfOf: "elsewhere"})); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("another principal = %v, want ErrConfig", err)
	}
	if _, err := a.Resume(ctx, "r", agent.WithIdentity(agent.Identity{Actor: "v2"})); err != nil {
		t.Fatal(err)
	}
	if seen != (agent.Identity{Actor: "v2", OnBehalfOf: "desk", AuthorityRef: "grant-1"}) {
		t.Fatalf("the tool saw %+v; want the live actor and the journaled principal", seen)
	}
}

type answerV struct {
	V int `json:"v"`
}

type answerW struct {
	W string `json:"w"`
}

// Typed runs journal their schema; they are resumed by ResumeTyped with their type, and refused
// by an untyped drive or another type.
func TestP14_TypedRunResumedByResumeTyped(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var pay counter
	model := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "pay")}},
		{calls: []agent.ToolUse{{ID: "f1", Name: "final_answer", Args: json.RawMessage(`{"v":7}`)}}},
	}}
	a := p14Build(t, model, j, agent.WithTools(pay.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	if _, res, err := a.RunTyped[answerV](ctx, "r", agent.UserText("go")); err == nil || res == nil {
		t.Fatalf("typed run = %v, %v; want the approval pause with a Result", res, err)
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	start, ok, err := agent.RecordedStart(ctx, j, "r")
	if err != nil || !ok || start.Typed == nil || start.Typed.Mode != agent.OutputTool {
		t.Fatalf("recorded start = %+v, %v, %v; want the typed start", start, ok, err)
	}
	before := model.calls.Load()
	if _, err := a.Run(ctx, "r", agent.UserText("go")); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("untyped drive of a typed run = %v, want ErrConfig", err)
	}
	if _, _, err := a.RunTyped[answerW](ctx, "r", agent.UserText("go")); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("another type = %v, want ErrConfig", err)
	}
	if err := agent.ResumeAgent(a)(ctx, "r", start); !errors.Is(err, agent.ErrNotResumable) {
		t.Fatalf("ResumeAgent on a typed run = %v, want ErrNotResumable", err)
	}
	if err := agent.ResumeTyped[answerW](a)(ctx, "r", start); !errors.Is(err, agent.ErrNotResumable) {
		t.Fatalf("ResumeTyped of another type = %v, want ErrNotResumable", err)
	}
	if model.calls.Load() != before {
		t.Fatal("a refused drive called the model")
	}
	if err := agent.ResumeTyped[answerV](a)(ctx, "r", start); err != nil {
		t.Fatalf("ResumeTyped = %v", err)
	}
	v, res, err := a.RunTyped[answerV](ctx, "r", agent.UserText("go"))
	if err != nil || v.V != 7 || !bytes.Contains(res.Output, []byte(`7`)) {
		t.Fatalf("finished typed run = %+v, %+v, %v; want v=7 and the journaled output", v, res, err)
	}
}

// Image input is journaled in run:start and given to the model on every drive.
func TestP14_ImageInput(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	a, model, _ := approvalAgent(t, j)
	img := agent.ImageData("image/png", []byte{0x89, 'P', 'N', 'G'})
	in := agent.UserParts(agent.Text{Text: "what is this?"}, img)
	if _, err := a.Run(ctx, "r", in); err == nil {
		t.Fatal("no pause")
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Resume(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	req := model.lastReq(t)
	var got *agent.Image
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if im, ok := p.(agent.Image); ok {
				got = &im
			}
		}
	}
	if got == nil || !bytes.Equal(got.Data, img.Data) || got.Mime != "image/png" {
		t.Fatalf("the resumed drive sent the image %+v, want the journaled one", got)
	}
	start, _, _ := agent.RecordedStart(ctx, j, "r")
	if len(start.Input.Parts) != 2 {
		t.Fatalf("run:start input = %+v, want the text and the image", start.Input)
	}
	// Another image is another input.
	other := agent.UserParts(agent.Text{Text: "what is this?"}, agent.ImageData("image/png", []byte{1}))
	if _, err := a.Run(ctx, "r", other); err != nil && !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("another image = %v", err)
	}
}

// A Result is returned on every error kind once the run ID is valid; an invalid run ID has none.
func TestP14_ResultOnEveryErrorKind(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("provider down")
	for _, tc := range []struct {
		name  string
		turns []p14Turn
		opts  []agent.RunOption
		want  func(error) bool
	}{
		{"answer", []p14Turn{{text: "done"}}, nil, func(err error) bool { return err == nil }},
		{"pause", []p14Turn{{calls: []agent.ToolUse{call("c1", "gated")}}}, nil, func(err error) bool { return agent.IsPause(err) }},
		{"model failure", []p14Turn{{err: boom}}, nil, func(err error) bool { return errors.Is(err, agent.ErrModel) }},
		{"turn limit", []p14Turn{{calls: []agent.ToolUse{call("c1", "lookup")}}, {text: "done"}}, []agent.RunOption{agent.WithMaxTurns(1)}, func(err error) bool { return errors.Is(err, agent.ErrMaxTurns) }},
		{"bad option", []p14Turn{{text: "done"}}, []agent.RunOption{agent.WithMaxTurns(-1)}, func(err error) bool { return errors.Is(err, agent.ErrConfig) }},
		{"saga abort", []p14Turn{{calls: []agent.ToolUse{call("c1", "fail")}}}, []agent.RunOption{agent.WithSaga()}, func(err error) bool {
			_, ok := errors.AsType[*agent.SagaAborted](err)
			return ok
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, _ := p14Journal(t)
			model := &p14Model{turns: tc.turns}
			a := p14Build(t, model, j, agent.WithTools(
				agent.MustFunc("gated", "", func(context.Context, struct{}) (string, error) { return "", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithApproval(agent.SingleApproval())),
				agent.MustFunc("lookup", "", func(context.Context, struct{}) (string, error) { return "", nil }, agent.WithSafety(agent.Safety{ReadOnly: true})),
				agent.MustFunc("fail", "", func(context.Context, struct{}) (string, error) { return "", errors.New("declined") })))
			res, err := a.Run(ctx, "r", agent.UserText("go"), tc.opts...)
			if !tc.want(err) {
				t.Fatalf("err = %v", err)
			}
			if res == nil || res.RunID != "r" {
				t.Fatalf("Result = %+v; want one for run r", res)
			}
			if err != nil && res.Message.Parts != nil {
				t.Fatalf("a failed run's Result carries a message %+v", res.Message)
			}
		})
	}
	j, _ := p14Journal(t)
	a := p14Build(t, &p14Model{turns: []p14Turn{{text: "x"}}}, j)
	for _, id := range []string{"", "a>b"} {
		if res, err := a.Run(ctx, id, agent.UserText("go")); res != nil || !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("Run(%q) = %v, %v; want ErrConfig and no Result", id, res, err)
		}
	}
	if err := agent.ValidateRunID("ok:run/1"); err != nil {
		t.Fatal(err)
	}
	if err := agent.ValidateRunID("x>y"); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), ">") {
		t.Fatalf("ValidateRunID = %v", err)
	}
}

// Cancellation cases that the tests in cancel_tree_x_test.go do not cover.

// A SendOnce run main started (run:start with no kind) still drives after the upgrade: its
// run:start reads as any kind.
func TestRev138_MainSendOnceRunStillDrives(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	model := &p14Model{turns: []p14Turn{{text: "hello"}}}
	a := p14Build(t, model, j)
	putMainStart(t, j, "s>@event/k", "hi") // as main's SendOnce journaled key k's run
	s, err := a.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.SendOnce(ctx, "k", agent.UserText("hi"))
	var msg agent.Message
	if res != nil {
		msg = res.Message
	}
	if err != nil || msg.Text() != "hello" {
		t.Fatalf("SendOnce of a key main started = %q, %v; want its answer", msg.Text(), err)
	}
}

// A legacy run:start still holds a drive to its input: another input is ErrConfig.
func TestRev138_MainStartStillHoldsTheInput(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	a := p14Build(t, &p14Model{turns: []p14Turn{{text: "hello"}}}, j)
	putMainStart(t, j, "r", "go")
	if _, err := a.Run(ctx, "r", agent.UserText("other")); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("a drive of a legacy run with another input = %v, want ErrConfig", err)
	}
}

// afterAmend is a Store that lands Cancel right after a drive's run:limits:0 insert.
type afterAmend struct {
	*agent.MemStore
	j    **agent.Journal
	done bool
}

func (s *afterAmend) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	e, ok, err := s.MemStore.Insert(ctx, runID, name, data)
	if name == "run:limits:0" && ok && !s.done {
		s.done = true
		if cerr := agent.Cancel(ctx, *s.j, runID, "stop"); cerr != nil {
			return e, ok, cerr
		}
	}
	return e, ok, err
}

// Model 10's DAmend goes back to DOpen: a Cancel that lands between a limit amendment and the
// drive's next model call is seen before the call.
func TestRev138_RecheckAfterAmendment(t *testing.T) {
	ctx := context.Background()
	var j *agent.Journal
	st := &afterAmend{MemStore: agent.NewMemStore(), j: &j}
	j, err := agent.NewJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	var c counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(c.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	if _, err := a.Run(ctx, "r", agent.UserText("go")); err == nil {
		t.Fatal("want the approval pause")
	}
	calls := model.calls.Load()
	_, err = a.Run(ctx, "r", agent.UserText("go"), agent.WithMaxTurns(9))
	if !errors.Is(err, agent.ErrRunCancelled) {
		t.Errorf("drive after its amendment and a Cancel = %v, want ErrRunCancelled", err)
	}
	if n := model.calls.Load() - calls; n != 0 {
		t.Errorf("the drive called the model %d times after Cancel returned nil", n)
	}
}

// A saga whose step failed and whose rollback request was written in the same turn ends
// run:cancelled: the next drive checks the request before the recorded failure.
func TestRev138_CancelAndFailureInOneTurnEndsCancelled(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	var undone counter
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { undone.n.Add(1); return nil })
	pay := agent.MustFunc("pay", "", func(context.Context, struct{}) (string, error) {
		writeMarker(t, m, "r", "run:cancel-requested", reason{"stop"})
		return "", errors.New("card declined")
	})
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "book")}}, {calls: []agent.ToolUse{call("c2", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(book, pay))
	_, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithSaga())
	st, _ := agent.Status(ctx, j, "r")
	if !errors.Is(err, agent.ErrRunCancelled) || st.State != agent.RunCancelled {
		t.Fatalf("saga cancelled and failed in one turn = %v, Status %s; want ErrRunCancelled and cancelled", err, st.State)
	}
	if undone.n.Load() != 1 {
		t.Fatalf("compensated %d, want 1", undone.n.Load())
	}
}

// The same, with the failure recorded and its rollback stopped before it finished (a compensator
// failed once): the request written meanwhile is checked before the recorded failure, so the next
// drive rolls back for the request and ends run:cancelled.
func TestRev138_RequestBeforeRecordedFailureOnTheNextDrive(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	failed := false
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error {
			if !failed {
				failed = true
				writeMarker(t, m, "r", "run:cancel-requested", reason{"stop"})
				return errors.New("the refund service is down")
			}
			return nil
		})
	pay := agent.MustFunc("pay", "", func(context.Context, struct{}) (string, error) {
		return "", errors.New("card declined")
	})
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "book")}}, {calls: []agent.ToolUse{call("c2", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(book, pay))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithSaga()); err == nil {
		t.Fatal("want the first rollback to stop")
	}
	if has(t, m, "r", "run:aborted") || has(t, m, "r", "run:cancelled") {
		t.Fatal("the stopped rollback wrote an end marker")
	}
	_, err := a.Resume(ctx, "r")
	st, _ := agent.Status(ctx, j, "r")
	if !errors.Is(err, agent.ErrRunCancelled) || st.State != agent.RunCancelled {
		t.Fatalf("next drive of a failed saga with a rollback request = %v, Status %s; want ErrRunCancelled and cancelled", err, st.State)
	}
	// The drive read the request first (model 10's DOpen order) and rolled back for it, not for the
	// failure that its rollback then found requested.
	if strings.Contains(err.Error(), "a step failed after the cancellation was requested") {
		t.Fatalf("the drive rolled back for the failure first: %v", err)
	}
}

// A saga that aborted, driven again without WithSaga, reports its abort (*SagaAborted), not
// ErrConfig.
func TestRev138_AbortedSagaResumedWithoutWithSaga(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	pay := agent.MustFunc("pay", "", func(context.Context, struct{}) (string, error) {
		return "", errors.New("card declined")
	})
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(pay))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithSaga()); err == nil {
		t.Fatal("want the saga to abort")
	}
	_, err := a.Resume(ctx, "r")
	if _, ok := errors.AsType[*agent.SagaAborted](err); !ok || errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Resume of an aborted saga = %v; want *SagaAborted, not ErrConfig", err)
	}
	_, err = a.Run(ctx, "r", agent.UserText("go"))
	if _, ok := errors.AsType[*agent.SagaAborted](err); !ok || errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Run without WithSaga of an aborted saga = %v; want *SagaAborted, not ErrConfig", err)
	}
}

// ErrNotStarted is a transient race (a run whose first drive has not written run:start yet), not
// a configuration error: it wraps no category.
func TestRev138_ErrNotStartedHasNoCategory(t *testing.T) {
	for _, cat := range []error{agent.ErrConfig, agent.ErrStorage, agent.ErrProtocol} {
		if errors.Is(agent.ErrNotStarted, cat) {
			t.Errorf("ErrNotStarted wraps %v", cat)
		}
	}
}

// ToolChoice{Mode: "none"} is enforced at dispatch: a call the model makes anyway is refused with
// an error result, and the tool never runs.
func TestRev138_ToolChoiceNoneEnforcedAtDispatch(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var c counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(c.tool("pay", agent.Safety{})))
	res, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithToolChoice(agent.ToolChoice{Mode: "none"}))
	if err != nil || res.Message.Text() != "done" {
		t.Fatalf("run = %v, %v", res, err)
	}
	if n := c.n.Load(); n != 0 {
		t.Fatalf("a tool ran %d times under ToolChoice none", n)
	}
}

// Recover reports a saga whose cancellation's rollback finished as cancelled, not as a failure.
func TestRev138_RecoverReportsACancelRollbackAsCancelled(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	var undone counter
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { undone.n.Add(1); return nil })
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "book")}}, {calls: []agent.ToolUse{call("c2", "pay")}}, {text: "done"}}}
	var c counter
	a := p14Build(t, model, j, agent.WithTools(book, c.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithSaga()); err == nil {
		t.Fatal("want the approval pause")
	}
	writeMarker(t, m, "r", "run:cancel-requested", reason{"stop"})
	n, err := agent.Recover(ctx, j, agent.ResumeAgent(a), agent.WithLeaseHolder("w"))
	st, _ := agent.Status(ctx, j, "r")
	if err != nil || n != 1 || st.State != agent.RunCancelled || undone.n.Load() != 1 {
		t.Fatalf("Recover = %d, %v; Status %s; compensated %d; want 1, nil, cancelled, 1", n, err, st.State, undone.n.Load())
	}
}

// A sub-run reads its tree root's cancellation at its turn boundaries and after a won claim, not
// only when it opens: a Cancel of the root that lands while the sub-run runs stops it at its next
// check.
func TestRev138_SubRunChecksTheRootAtEveryCheck(t *testing.T) {
	for _, c := range []struct {
		where string
		saga  bool
	}{{"turn boundary", false}, {"after the claim", false}, {"turn boundary", true}, {"after the claim", true}} {
		where := c.where
		t.Run(fmt.Sprintf("%s/saga=%v", where, c.saga), func(t *testing.T) {
			ctx := context.Background()
			j, _ := p14Journal(t)
			var paid counter
			cancelRoot := func() {
				if err := agent.Cancel(ctx, j, "r", "stop"); err != nil {
					t.Errorf("Cancel = %v", err)
				}
			}
			mark := agent.MustFunc("mark", "", func(context.Context, struct{}) (string, error) {
				if where == "turn boundary" {
					cancelRoot()
				}
				return "ok", nil
			}, agent.WithSafety(agent.Safety{ReadOnly: true}))
			subModel := &p14Model{turns: []p14Turn{
				{calls: []agent.ToolUse{call("s1", "mark")}},
				{calls: []agent.ToolUse{call("s2", "pay")}, hook: func(agent.Request) {
					if where == "after the claim" {
						cancelRoot()
					}
				}},
				{text: "paid"},
			}}
			sub := p14Build(t, subModel, j, agent.WithTools(mark, paid.tool("pay", agent.Safety{})))
			parentModel := &p14Model{turns: []p14Turn{
				{calls: []agent.ToolUse{{ID: "p1", Name: "helper", Args: []byte(`{"task":"pay"}`)}}},
				{text: "done"},
			}}
			parent := p14Build(t, parentModel, j, agent.WithTools(agent.MustSubAgent("helper", "", sub)))
			var opts []agent.RunOption
			if c.saga {
				opts = append(opts, agent.WithSaga())
			}
			_, err := parent.Run(ctx, "r", agent.UserText("go"), opts...)
			st, _ := agent.Status(ctx, j, "r")
			if !errors.Is(err, agent.ErrRunCancelled) || st.State != agent.RunCancelled {
				t.Errorf("parent run = %v, Status %s; want ErrRunCancelled and cancelled", err, st.State)
			}
			if n := paid.n.Load(); n != 0 {
				t.Errorf("the sub-run's side effect fired %d times after its root was cancelled", n)
			}
			if where == "turn boundary" && subModel.calls.Load() != 1 {
				t.Errorf("the sub-run called its model %d times; want 1, the turn boundary stops it", subModel.calls.Load())
			}
		})
	}
}

// A cancelled saga turn whose step completed: the next message's Send drives the turn's rollback
// (the session owns its turns), which compensates the step and writes run:cancelled, and records
// the turn closed.
func TestRev138_SessionRollsBackACancelledSagaTurn(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var undone counter
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { undone.n.Add(1); return nil })
	var c counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "book")}}, {calls: []agent.ToolUse{call("c2", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(book, c.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	s, err := a.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, agent.UserText("one"), agent.WithSaga()); err == nil {
		t.Fatal("want the approval pause")
	}
	if err := agent.Cancel(ctx, j, "s>@turn/0", "stop"); err != nil {
		t.Fatalf("Cancel = %v", err)
	}
	if st, _ := agent.Status(ctx, j, "s>@turn/0"); st.State != agent.RunStarted {
		t.Fatalf("a saga turn's Cancel wrote its end: Status %s, want started until its rollback", st.State)
	}
	_, err = s.Send(ctx, agent.UserText("two"))
	if errors.Is(err, agent.ErrConfig) {
		t.Fatalf("the next message was refused: %v", err)
	}
	if st, _ := agent.Status(ctx, j, "s>@turn/0"); st.State != agent.RunCancelled || undone.n.Load() != 1 {
		t.Fatalf("turn 0: Status %s, compensated %d; want cancelled and 1", st.State, undone.n.Load())
	}
	if h := s.History(); len(h) != 0 {
		t.Fatalf("the closed turn is in the transcript: %v", h)
	}
	if st, _ := agent.Status(ctx, j, "s>@turn/1"); st.State == agent.RunNotStarted {
		t.Fatalf("the next message's turn did not run (%v)", err)
	}
}

// A sub-run reads its tree root's cancellation when it opens: a sub-agent called after its root
// was cancelled (the Cancel landed during the parent's model turn) does not call its model.
func TestRev138_SubRunOpenChecksTheRoot(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	subModel := &p14Model{turns: []p14Turn{{text: "sub"}}}
	sub := p14Build(t, subModel, j)
	parentModel := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{{ID: "p1", Name: "helper", Args: []byte(`{"task":"x"}`)}}, hook: func(agent.Request) {
			if err := agent.Cancel(ctx, j, "r", "stop"); err != nil {
				t.Errorf("Cancel = %v", err)
			}
		}},
		{text: "done"},
	}}
	parent := p14Build(t, parentModel, j, agent.WithTools(agent.MustSubAgent("helper", "", sub)))
	if _, err := parent.Run(ctx, "r", agent.UserText("go")); !errors.Is(err, agent.ErrRunCancelled) {
		t.Fatalf("parent run = %v, want ErrRunCancelled", err)
	}
	if n := subModel.calls.Load(); n != 0 {
		t.Fatalf("the sub-run called its model %d times after its root was cancelled", n)
	}
}

// A saga sub-run that rolls back for its root's cancellation records the root's reason in its own
// run:cancelled.
func TestRev138_SagaSubRunRecordsTheRootsReason(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	var undone counter
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { undone.n.Add(1); return nil })
	mark := agent.MustFunc("mark", "", func(context.Context, struct{}) (string, error) {
		if err := agent.Cancel(ctx, j, "r", "the customer left"); err != nil {
			t.Errorf("Cancel = %v", err)
		}
		return "ok", nil
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	subModel := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("s1", "book")}}, {calls: []agent.ToolUse{call("s2", "mark")}}, {text: "sub"}}}
	sub := p14Build(t, subModel, j, agent.WithTools(book, mark))
	parentModel := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{{ID: "p1", Name: "helper", Args: []byte(`{"task":"x"}`)}}}, {text: "done"}}}
	parent := p14Build(t, parentModel, j, agent.WithTools(agent.MustSubAgent("helper", "", sub)))
	if _, err := parent.Run(ctx, "r", agent.UserText("go"), agent.WithSaga()); !errors.Is(err, agent.ErrRunCancelled) {
		t.Fatalf("parent saga = %v, want ErrRunCancelled", err)
	}
	if undone.n.Load() != 1 {
		t.Fatalf("compensated %d, want 1", undone.n.Load())
	}
	var subs []string
	for id, err := range m.Runs(ctx, agent.RunFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		if agent.IsSubRun(id) {
			subs = append(subs, id)
		}
	}
	if len(subs) != 1 {
		t.Fatalf("sub-runs = %v, want one", subs)
	}
	if st, _ := agent.Status(ctx, j, subs[0]); st.State != agent.RunCancelled || st.Terminal != "the customer left" {
		t.Fatalf("the sub-run's Status = %+v, want cancelled with the root's reason", st)
	}
}

// A cancelled turn's run whose lease another worker still holds: Send of its message returns
// ErrRunCancelled (the run's end), not ErrTurnContended.
func TestRev138_CancelledTurnUnderAnotherLease(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	var c counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(c.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	s, err := a.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, agent.UserText("one")); err == nil {
		t.Fatal("want the approval pause")
	}
	if err := agent.Cancel(ctx, j, "s>@turn/0", "stop"); err != nil {
		t.Fatalf("Cancel = %v", err)
	}
	if ok, err := m.AcquireLease(ctx, "s>@turn/0", "another-worker", time.Hour); err != nil || !ok {
		t.Fatalf("AcquireLease = %v, %v", ok, err)
	}
	if _, err := s.Send(ctx, agent.UserText("one")); !errors.Is(err, agent.ErrRunCancelled) {
		t.Fatalf("Send of the cancelled message under another worker's lease = %v, want ErrRunCancelled", err)
	}
}

// Rule 16 (S3, model 12's findings/s3-cancel-wedge): Cancel of an open Send turn's run does not
// block the session for ever. The cancelled message's caller gets ErrRunCancelled; the next
// message's Send reads the open turn's run's end markers, records the cancelled turn closed, and
// is answered.
func TestP14Rule16_CancelledTurnIsClosed(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var pay counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "pay")}}, {text: "answer"}}}
	a := p14Build(t, model, j, agent.WithTools(pay.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	s, err := a.Session(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Send(ctx, agent.UserText("x"))
	pa, ok := errors.AsType[*agent.ApprovalPending](err)
	if !ok {
		t.Fatalf("Send(x) = %v, want the approval pause", err)
	}
	if err := agent.Cancel(ctx, j, pa.RunID, "operator"); err != nil {
		t.Fatalf("Cancel of the turn's run = %v", err)
	}
	if _, err := s.Send(ctx, agent.UserText("x")); !errors.Is(err, agent.ErrRunCancelled) {
		t.Fatalf("Send(x) again = %v, want ErrRunCancelled", err)
	}
	// "y" is a new message, and its model turn answers at once (the model has seen no assistant
	// turn in y's run).
	model.turns = []p14Turn{{text: "answer to y"}}
	res, err := s.Send(ctx, agent.UserText("y"))
	var got agent.Message
	if res != nil {
		got = res.Message
	}
	if err != nil || got.Text() != "answer to y" {
		t.Fatalf("Send(y) = %q, %v; want y answered once x's cancelled turn is closed", got.Text(), err)
	}
	if pay.n.Load() != 0 {
		t.Fatal("the cancelled turn's call ran")
	}
	// A fresh handle sees the same journal: y is answered, and x's cancelled turn holds no answer
	// in the transcript.
	s2, err := a.Session(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	h := s2.History()
	if len(h) != 2 || h[0].Text() != "y" || h[1].Text() != "answer to y" {
		t.Fatalf("history = %v, want only y's turn", h)
	}
	// x's turn is closed: sending x again opens a new turn, which the model answers.
	if got, err := agenttest.Answer(s2.Send(ctx, agent.UserText("x"))); err != nil || got.Text() != "answer to y" {
		t.Fatalf("Send(x) after the close = %q, %v; want a new turn answered", got.Text(), err)
	}
}

// Send takes a Message and run options, and returns a Result.
func TestP14_SessionSendResult(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	model := &p14Model{turns: []p14Turn{{text: "seen"}}}
	a := p14Build(t, model, j)
	s, err := a.Session(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	in := agent.UserParts(agent.Text{Text: "look"}, agent.ImageData("image/png", []byte{1, 2}))
	res, err := s.Send(ctx, in, agent.WithSystemPrompt("be brief"))
	if err != nil || res == nil || res.Message.Text() != "seen" {
		t.Fatalf("Send = %+v, %v", res, err)
	}
	if systemText(model.lastReq(t)) != "be brief" {
		t.Fatal("the turn did not run under its run option")
	}
	res, err = s.SendOnce(ctx, "evt-1", agent.UserText("hi"))
	if err != nil || res.Message.Text() != "seen" {
		t.Fatalf("SendOnce = %+v, %v", res, err)
	}
	h := s.History()
	if len(h) != 4 || len(h[0].Parts) != 2 {
		t.Fatalf("history = %v, want both turns, the first with its image", h)
	}
}
