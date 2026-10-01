package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// rev138 A: Cancel of the parent lands while the parent's model turn is in flight (after the turn
// check). The turn calls a sub-agent (Idempotent: no claim, so no post-claim check), and the
// sub-run's drive checks only its own run's run:cancelled: the sub-run's side effect fires after
// Cancel(parent) returned nil.
func TestRev138_SubRunFiresAfterParentCancel(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var c counter
	subModel := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("s1", "pay")}}, {text: "paid"}}}
	sub := p14Build(t, subModel, j, agent.WithTools(c.tool("pay", agent.Safety{})))
	parentModel := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{{ID: "p1", Name: "helper", Args: []byte(`{"task":"pay"}`)}}, hook: func(agent.Request) {
			if err := agent.Cancel(ctx, j, "r", "stop"); err != nil {
				t.Errorf("Cancel = %v", err)
			}
		}},
		{text: "done"},
	}}
	parent := p14Build(t, parentModel, j, agent.WithTools(agent.SubAgent("helper", "", sub)))
	_, err := parent.RunMessage(ctx, "r", agent.UserText("go"))
	t.Logf("parent run = %v; pay fired %d", err, c.n.Load())
	if n := c.n.Load(); n != 0 {
		t.Errorf("the sub-run's side effect fired %d times after Cancel(parent) returned nil", n)
	}
}

// rev138 B, under the maintainer's decision: a retry-safe call (no claim) keeps D1's in-flight
// semantics. Cancel takes effect at the next turn boundary or claim, with no Get per retry-safe
// call, so a retry-safe call of the turn in flight when Cancel lands may still run; the run then
// ends cancelled at the turn boundary, and the model is not called again.
func TestRev138_RetrySafeCallInTheTurnInFlightMayRun(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var c counter
	model := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "put")}, hook: func(agent.Request) {
			if err := agent.Cancel(ctx, j, "r", "stop"); err != nil {
				t.Errorf("Cancel = %v", err)
			}
		}},
		{text: "done"},
	}}
	a := p14Build(t, model, j, agent.WithTools(c.tool("put", agent.Safety{Idempotent: true})))
	_, err := a.RunMessage(ctx, "r", agent.UserText("go"))
	if !errors.Is(err, agent.ErrRunCancelled) {
		t.Errorf("run = %v, want ErrRunCancelled", err)
	}
	if n := c.n.Load(); n > 1 {
		t.Errorf("the retry-safe call ran %d times, want at most once (the turn in flight)", n)
	}
	if n := model.calls.Load(); n != 1 {
		t.Errorf("model calls = %d, want 1: the turn boundary sees the cancellation", n)
	}
}

// putMainStart writes run:start as main (before P14) journaled it: {"input": "<text>"}, kind empty
// (agent), through the journal so the header is written first.
func putMainStart(t *testing.T, j *agent.Journal, runID, input string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"input": input})
	if _, err := j.Do(context.Background(), runID, "run:start", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: b}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// rev138 C: a session turn whose run main started (run:start with no kind, i.e. agent) is ErrConfig
// on this branch: the turn drive's kind is session_turn. A turn left open across the upgrade (a
// pause, a crash) can then never finish, and the open turn refuses every other message.
func TestRev138_MainSessionTurnRunStillDrives(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	model := &p14Model{turns: []p14Turn{{text: "hello"}}}
	a := p14Build(t, model, j)
	putMainStart(t, j, "s>@turn/0", "hi") // as main's Send journaled turn 0's run
	s, err := a.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Send(ctx, "hi")
	t.Logf("Send of a turn main started = %v", err)
	if err != nil {
		t.Errorf("a session turn started on main does not resume: %v", err)
	}
}

// rev138 D: a typed run main started (run:start without a typed start) resumed through RunTyped
// on this branch is ErrConfig ("not a typed run").
func TestRev138_MainTypedRunStillResumes(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	model := &p14Model{turns: []p14Turn{{text: `{"n":1}`}}}
	a := p14Build(t, model, j)
	putMainStart(t, j, "r", "go")
	type out struct {
		N int `json:"n"`
	}
	_, err := agent.RunTyped[out](ctx, a, "r", "go")
	t.Logf("RunTyped of a typed run main started = %v", err)
	if errors.Is(err, agent.ErrConfig) {
		t.Errorf("a typed run started on main is refused on resume: %v", err)
	}
}

// rev138 E: Cancel lands during the final model turn; the drive writes run:complete after
// run:cancelled and reports ErrRunCancelled (rule 4), Status says RunCancelled, but the exported
// IsComplete (which examples/recover uses to skip finished runs) says the run completed.
func TestRev138_IsCompleteIgnoresFirstEnd(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	model := &p14Model{turns: []p14Turn{{text: "done", hook: func(agent.Request) {
		if err := agent.Cancel(ctx, j, "r", "stop"); err != nil {
			t.Errorf("Cancel = %v", err)
		}
	}}}}
	a := p14Build(t, model, j)
	_, err := a.RunMessage(ctx, "r", agent.UserText("go"))
	st, _ := agent.Status(ctx, j, "r")
	done, _ := agent.IsComplete(ctx, j, "r")
	t.Logf("run = %v; Status = %s; IsComplete = %v", err, st.State, done)
	if st.State == agent.RunCancelled && done {
		t.Errorf("IsComplete = true for a run whose first end marker is run:cancelled")
	}
}

// rev138 F: a saga session turn (SendMessage WithSaga) paused for approval is cancelled; Cancel
// writes only the rollback request, so cancelledFirst is false and the next message is refused:
// rule 16's wedge, for a saga turn, until the cancelled message is sent again.
func TestRev138_CancelledSagaTurnBlocksSession(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var c counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(c.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	s, err := a.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendMessage(ctx, agent.UserText("one"), agent.WithSaga()); err == nil {
		t.Fatal("want the approval pause")
	}
	if err := agent.Cancel(ctx, j, "s>@turn/0", "stop"); err != nil {
		t.Fatalf("Cancel = %v", err)
	}
	_, err = s.SendMessage(ctx, agent.UserText("two"))
	t.Logf("next message after cancelling a saga turn = %v", err)
	if err != nil {
		t.Errorf("the session refuses the next message after its saga turn was cancelled: %v", err)
	}
}

// rev138 G: a saga's cancel rollback finds that another drive's rollback wrote run:aborted first
// (rule 4: the writer reads back and reports the first marker). The drive reports ErrRunCancelled
// while the run's first end marker, and Status, say aborted.
func TestRev138_SagaCancelRollbackReportsWrongEnd(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	book := agent.CompensatedFunc("book", "", agent.Safety{},
		func(context.Context, struct{}) (string, error) {
			writeMarker(t, m, "r", "run:cancel-requested", reason{"stop"})
			return "booked", nil
		},
		func(context.Context, struct{}, string) error {
			// another drive of the saga, rolling back a failure, finishes first
			writeMarker(t, m, "r", "run:aborted", "a step failed")
			return nil
		})
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "book")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(book))
	_, err := a.RunMessage(ctx, "r", agent.UserText("go"), agent.WithSaga())
	st, _ := agent.Status(ctx, j, "r")
	t.Logf("saga drive = %v; Status = %s", err, st.State)
	if errors.Is(err, agent.ErrRunCancelled) && st.State == agent.RunAborted {
		t.Errorf("the drive reports ErrRunCancelled, but the run's first end marker is run:aborted")
	}
}

// afterStart is a Store that lands Cancel right after the first drive's run:start insert.
type afterStart struct {
	*agent.MemStore
	j    **agent.Journal
	done bool
}

func (s *afterStart) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	e, ok, err := s.MemStore.Insert(ctx, runID, name, data)
	if name == "run:start" && ok && !s.done {
		s.done = true
		if cerr := agent.Cancel(ctx, *s.j, runID, "stop"); cerr != nil {
			return e, ok, cerr
		}
	}
	return e, ok, err
}

// rev138 H (model fidelity): model 10's DStart goes back to DOpen, which reads the end markers
// again; the code's first drive does not re-read after its run:start insert, so a Cancel that
// lands between the insert and the first model call is not seen before the model is called and
// a non-claimed tool runs.
func TestRev138_NoRecheckAfterStart(t *testing.T) {
	ctx := context.Background()
	var j *agent.Journal
	st := &afterStart{MemStore: agent.NewMemStore(), j: &j}
	j, err := agent.NewJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	var c counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "put")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(c.tool("put", agent.Safety{Idempotent: true})))
	_, err = a.RunMessage(ctx, "r", agent.UserText("go"))
	t.Logf("run = %v; model calls %d; put fired %d", err, model.calls.Load(), c.n.Load())
	if model.calls.Load() != 0 || c.n.Load() != 0 {
		t.Errorf("after Cancel returned nil the drive called the model %d times and the tool %d times", model.calls.Load(), c.n.Load())
	}
}
