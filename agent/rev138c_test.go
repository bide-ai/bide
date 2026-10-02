package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// The decisions on the second review of #138: the suspicions it raised, each proved by a test.

// Suspicion 1: a legacy run:start (no kind, no typed start) does not say which entry point started
// the run, and a typed run started on main has one. ResumeAgent must not recover it as an untyped
// run: it refuses it with ErrNotResumable, and ResumeAny hands it to the next Resumer, the
// deployment's own.
func TestRev138c_LegacyStartIsNotResumedByResumeAgent(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	model := &p14Model{turns: []p14Turn{{text: "plain answer"}}}
	a := p14Build(t, model, j)
	putMainStart(t, j, "r", "go") // as main journaled a run, typed or not: the record does not say
	start, ok, err := agent.RecordedStart(ctx, j, "r")
	if err != nil || !ok {
		t.Fatalf("RecordedStart = %v, %v", ok, err)
	}
	if err := agent.ResumeAgent(a)(ctx, "r", start); !errors.Is(err, agent.ErrNotResumable) {
		t.Fatalf("ResumeAgent of a legacy run:start = %v, want ErrNotResumable", err)
	}
	if n := model.calls.Load(); n != 0 {
		t.Fatalf("ResumeAgent drove a legacy run: %d model calls", n)
	}
	var mine int
	own := func(context.Context, string, agent.RunStart) error { mine++; return nil }
	if err := agent.ResumeAny(agent.ResumeAgent(a), own)(ctx, "r", start); err != nil || mine != 1 {
		t.Fatalf("ResumeAny(ResumeAgent, own) = %v, own called %d times; want nil and 1", err, mine)
	}
	if n, err := agent.Recover(ctx, j, agent.ResumeAgent(a), agent.WithLeaseHolder("w")); n != 0 || !errors.Is(err, agent.ErrNotResumable) || model.calls.Load() != 0 {
		t.Fatalf("Recover(ResumeAgent) = %d, %v, model calls %d; want 0, ErrNotResumable, 0", n, err, model.calls.Load())
	}
	if n, err := agent.Recover(ctx, j, agent.ResumeAgent(a), agent.WithLeaseHolder("w")); n != 0 || err != nil || model.calls.Load() != 0 {
		t.Fatalf("a second Recover(ResumeAgent) = %d, %v, model calls %d; want 0, nil (reported once), 0", n, err, model.calls.Load())
	}
}

// Suspicion 2: a saga sub-run whose step fails after its tree root's cancellation was requested
// rolls back for the failure; its end is run:cancelled (the root asked for the rollback), as a root
// saga's failure rollback after its own request ends.
func TestRev138c_SagaSubRunFailureInACancelledTreeEndsCancelled(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	var undone counter
	book := agent.CompensatedFunc("book", "", agent.Safety{},
		func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { undone.n.Add(1); return nil })
	// fail cancels the root inside its call (after its claim's check) and then fails: the sub-run
	// records the failure and rolls back for it.
	fail := agent.Func("fail", "", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		if err := agent.Cancel(ctx, j, "r", "the customer left"); err != nil {
			t.Errorf("Cancel = %v", err)
		}
		return "", errors.New("the card was declined")
	})
	subModel := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("s1", "book")}}, {calls: []agent.ToolUse{call("s2", "fail")}}, {text: "sub"}}}
	sub := p14Build(t, subModel, j, agent.WithTools(book, fail))
	parentModel := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{{ID: "p1", Name: "helper", Args: []byte(`{"task":"x"}`)}}}, {text: "done"}}}
	parent := p14Build(t, parentModel, j, agent.WithTools(agent.SubAgent("helper", "", sub)))
	_, err := parent.RunMessage(ctx, "r", agent.UserText("go"), agent.WithSaga())
	if st, _ := agent.Status(ctx, j, "r"); st.State != agent.RunCancelled {
		t.Fatalf("the root's Status = %s (run %v), want cancelled", st.State, err)
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
	if undone.n.Load() != 1 {
		t.Fatalf("compensated %d, want 1", undone.n.Load())
	}
	if st, _ := agent.Status(ctx, j, subs[0]); st.State != agent.RunCancelled {
		t.Fatalf("the sub-run's Status = %+v, want cancelled: its tree root asked for the rollback", st)
	}
}

// Suspicion 3: the session drives a cancelled saga turn's rollback, compensators included, without
// holding the handle's mutex (taken only around the session's state), so a compensator, or another
// caller on the handle, is not blocked behind it.
func TestRev138c_TurnRollbackDoesNotHoldTheSessionMutex(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var s *agent.Session
	var held, ran bool
	book := agent.CompensatedFunc("book", "", agent.Safety{},
		func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error {
			ran, held = true, agent.SessionMuHeld(s)
			return nil
		})
	var c counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "book")}}, {calls: []agent.ToolUse{call("c2", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(book, c.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	var err error
	if s, err = a.Session(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendMessage(ctx, agent.UserText("one"), agent.WithSaga()); err == nil {
		t.Fatal("want the approval pause")
	}
	if err := agent.Cancel(ctx, j, "s>@turn/0", "stop"); err != nil {
		t.Fatalf("Cancel = %v", err)
	}
	_, err = s.SendMessage(ctx, agent.UserText("two"))
	if errors.Is(err, agent.ErrConfig) {
		t.Fatalf("the next message was refused: %v", err)
	}
	if !ran {
		t.Fatal("the turn's compensator did not run")
	}
	if held {
		t.Fatal("the session's mutex was held while the cancelled turn's compensator ran")
	}
	if st, _ := agent.Status(ctx, j, "s>@turn/0"); st.State != agent.RunCancelled {
		t.Fatalf("turn 0: Status %s, want cancelled", st.State)
	}
	if st, _ := agent.Status(ctx, j, "s>@turn/1"); st.State == agent.RunNotStarted {
		t.Fatalf("the next message's turn did not run (%v)", err)
	}
}
