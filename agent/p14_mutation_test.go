package agent_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// Tests added by the mutation check of the P14 rules: each kills a mutant the rule tests above let
// survive.

// Rule 10: an amendment binds the drives after it that pass no option: here the limit is raised
// from 1 to 3 by a drive that only pauses, and the next drive, which passes nothing, runs three
// turns.
func TestP14Rule10_AmendmentBindsLaterDrives(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	a, model, _ := approvalAgent(t, j)
	if _, err := a.RunMessage(ctx, "r", agent.UserText("go"), agent.WithMaxTurns(1)); !agent.IsPause(err) {
		t.Fatalf("first drive = %v, want the approval pause", err)
	}
	if _, err := a.ResumeRun(ctx, "r", agent.WithMaxTurns(3)); !agent.IsPause(err) {
		t.Fatalf("amending drive = %v, want the pause again", err)
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ResumeRun(ctx, "r"); !errors.Is(err, agent.ErrMaxTurns) {
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
	if _, err := a.RunMessage(ctx, "r", agent.UserText("go"), agent.WithMaxConcurrency(1)); !errors.Is(err, agent.ErrRunCancelled) {
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
	if _, err := a.RunMessage(ctx, "r", agent.UserText("go"), agent.WithSaga()); !errors.Is(err, agent.ErrStorage) {
		t.Fatalf("first drive = %v, want the failed run:complete", err)
	}
	writeMarker(t, mem, "r", "run:cancel-requested", reason{"late"})
	res, err := a.ResumeRun(ctx, "r")
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
	_, err = s.Send(ctx, "x")
	pa, ok := errors.AsType[*agent.ApprovalPending](err)
	if !ok {
		t.Fatalf("Send(x) = %v, want the approval pause", err)
	}
	writeMarker(t, m, pa.RunID, "run:complete", nil)
	writeMarker(t, m, pa.RunID, "run:cancelled", reason{"late"})
	if _, err := s.Send(ctx, "y"); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Send(y) = %v, want ErrConfig: x's turn completed, it was not cancelled", err)
	}
}
