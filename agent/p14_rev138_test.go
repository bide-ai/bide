package agent_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// The adversarial review of #138: the cases its tests (rev138_test.go) do not cover, each for a
// maintainer decision on its findings.

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
