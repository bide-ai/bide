package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journaltest"
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
	parent := p14Build(t, parentModel, j, agent.WithTools(agent.MustSubAgent("helper", "", sub)))
	_, err := parent.Run(ctx, "r", agent.UserText("go"))
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
	_, err := a.Run(ctx, "r", agent.UserText("go"))
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
	if _, err := journaltest.Do(context.Background(), j, runID, "run:start", func(context.Context) (agent.Record, error) {
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
	_, err = s.Send(ctx, agent.UserText("hi"))
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
	_, _, err := a.RunTyped[out](ctx, "r", agent.UserText("go"))
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
	_, err := a.Run(ctx, "r", agent.UserText("go"))
	st, _ := agent.Status(ctx, j, "r")
	done, _ := agent.IsComplete(ctx, j, "r")
	t.Logf("run = %v; Status = %s; IsComplete = %v", err, st.State, done)
	if st.State == agent.RunCancelled && done {
		t.Errorf("IsComplete = true for a run whose first end marker is run:cancelled")
	}
}

// rev138 F: a saga session turn (Send WithSaga) paused for approval is cancelled; Cancel
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
	if _, err := s.Send(ctx, agent.UserText("one"), agent.WithSaga()); err == nil {
		t.Fatal("want the approval pause")
	}
	if err := agent.Cancel(ctx, j, "s>@turn/0", "stop"); err != nil {
		t.Fatalf("Cancel = %v", err)
	}
	_, err = s.Send(ctx, agent.UserText("two"))
	t.Logf("next message after cancelling a saga turn = %v", err)
	// The next message runs its own turn (turn 1; the model's script pauses it for approval too).
	if errors.Is(err, agent.ErrConfig) {
		t.Errorf("the session refuses the next message after its saga turn was cancelled: %v", err)
	}
	if st, _ := agent.Status(ctx, j, "s>@turn/0"); st.State != agent.RunCancelled {
		t.Errorf("the cancelled saga turn's run is %s, want cancelled (its rollback driven by the session)", st.State)
	}
}

// rev138 G: a saga's cancel rollback finds that another drive's rollback wrote run:aborted first
// (rule 4: the writer reads back and reports the first marker). The drive reports ErrRunCancelled
// while the run's first end marker, and Status, say aborted.
func TestRev138_SagaCancelRollbackReportsWrongEnd(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) {
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
	_, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithSaga())
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
	_, err = a.Run(ctx, "r", agent.UserText("go"))
	t.Logf("run = %v; model calls %d; put fired %d", err, model.calls.Load(), c.n.Load())
	if model.calls.Load() != 0 || c.n.Load() != 0 {
		t.Errorf("after Cancel returned nil the drive called the model %d times and the tool %d times", model.calls.Load(), c.n.Load())
	}
}

// A sub-agent that journals to another store than its root (allowed outside a saga) never sees
// the root's cancellation: rootCancelled reads the sub-run's own store for the root's markers.
func TestReview138b_CrossStoreSubRunIgnoresRootCancel(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	j2, _ := p14Journal(t) // the sub-agent's own store
	var paid counter
	subModel := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("s1", "pay")}}, {text: "paid"}}}
	sub := p14Build(t, subModel, j2, agent.WithTools(paid.tool("pay", agent.Safety{})))
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
		t.Errorf("the cross-store sub-run called its model %d times after its root was cancelled", n)
	}
	if n := paid.n.Load(); n != 0 {
		t.Errorf("the cross-store sub-run's side effect fired %d times after its root was cancelled", n)
	}
}

// lostStartStore makes a drive lose its run:start insert to a concurrent first drive (same
// entry) after which a Cancel lands: both between the drive's Load and its insert.
type lostStartStore struct {
	*agent.MemStore
	armed bool
	race  func(ctx context.Context, runID string, data []byte)
}

func (s *lostStartStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if s.armed && name == "run:start" {
		s.armed = false
		s.race(ctx, runID, data)
	}
	return s.MemStore.Insert(ctx, runID, name, data)
}

// Model 10's DStart returns to DOpen whether or not the drive's run:start landed; the code reloads
// only when it did (wrote is empty for a lost insert), so a Cancel that landed between the drive's
// Load and its lost insert is not seen before the drive's first model call.
func TestReview138b_LostStartInsertSkipsReload(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	s := &lostStartStore{MemStore: m, armed: true}
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	other, err := agent.NewJournal(m) // the concurrent first drive and the Canceller, another process
	if err != nil {
		t.Fatal(err)
	}
	s.race = func(ctx context.Context, runID string, data []byte) {
		if _, _, err := m.Insert(ctx, runID, "run:start", data); err != nil { // the other drive's start
			t.Fatal(err)
		}
		if err := agent.Cancel(ctx, other, runID, "stop"); err != nil {
			t.Fatalf("Cancel = %v", err)
		}
	}
	model := &p14Model{turns: []p14Turn{{text: "answer"}}}
	a := p14Build(t, model, j)
	_, err = a.Run(ctx, "r", agent.UserText("go"))
	if n := model.calls.Load(); n != 0 {
		t.Errorf("the drive called its model %d times after the run was cancelled (err %v)", n, err)
	}
	if !errors.Is(err, agent.ErrRunCancelled) {
		t.Errorf("Run = %v, want ErrRunCancelled", err)
	}
}

// B2 on the saga path: a saga's drive that loses its run:start insert, after which a Cancel writes
// the rollback request, reloads too and rolls back before its first model call (the run ends
// run:cancelled).
func TestReview138b_LostStartInsertSkipsReloadSaga(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	s := &lostStartStore{MemStore: m, armed: true}
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	other, err := agent.NewJournal(m)
	if err != nil {
		t.Fatal(err)
	}
	s.race = func(ctx context.Context, runID string, data []byte) {
		if _, _, err := m.Insert(ctx, runID, "run:start", data); err != nil {
			t.Fatal(err)
		}
		if err := agent.Cancel(ctx, other, runID, "stop"); err != nil {
			t.Fatalf("Cancel = %v", err)
		}
	}
	model := &p14Model{turns: []p14Turn{{text: "answer"}}}
	a := p14Build(t, model, j)
	_, err = a.Run(ctx, "r", agent.UserText("go"), agent.WithSaga())
	if n := model.calls.Load(); n != 0 {
		t.Errorf("the saga's drive called its model %d times after its rollback was requested (err %v)", n, err)
	}
	if st, _ := agent.Status(ctx, other, "r"); st.State != agent.RunCancelled {
		t.Errorf("Status = %s (run %v), want cancelled", st.State, err)
	}
}

// B1, two levels down: the root on one store, its sub-agent on a second, and that sub-agent's own
// sub-agent on a third. The Cancel of the root lands while the middle run is in its model turn; the
// innermost sub-run reads the root's markers from the root's store (which neither its own store
// nor its parent's is), and does nothing.
func TestReview138b_CrossStoreNestedSubRunSeesRootCancel(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	j2, _ := p14Journal(t)
	j3, _ := p14Journal(t)
	var paid counter
	innerModel := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("i1", "pay")}}, {text: "paid"}}}
	inner := p14Build(t, innerModel, j3, agent.WithTools(paid.tool("pay", agent.Safety{})))
	midModel := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{{ID: "m1", Name: "inner", Args: []byte(`{"task":"x"}`)}}, hook: func(agent.Request) {
			if err := agent.Cancel(ctx, j, "r", "stop"); err != nil {
				t.Errorf("Cancel = %v", err)
			}
		}},
		{text: "mid"},
	}}
	mid := p14Build(t, midModel, j2, agent.WithTools(agent.MustSubAgent("inner", "", inner)))
	parentModel := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{{ID: "p1", Name: "mid", Args: []byte(`{"task":"x"}`)}}}, {text: "done"}}}
	parent := p14Build(t, parentModel, j, agent.WithTools(agent.MustSubAgent("mid", "", mid)))
	if _, err := parent.Run(ctx, "r", agent.UserText("go")); !errors.Is(err, agent.ErrRunCancelled) {
		t.Fatalf("parent run = %v, want ErrRunCancelled", err)
	}
	if n := innerModel.calls.Load(); n != 0 {
		t.Errorf("the innermost sub-run called its model %d times after its root was cancelled", n)
	}
	if n := paid.n.Load(); n != 0 {
		t.Errorf("the innermost sub-run's side effect fired %d times after its root was cancelled", n)
	}
}

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
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { undone.n.Add(1); return nil })
	// fail cancels the root inside its call (after its claim's check) and then fails: the sub-run
	// records the failure and rolls back for it.
	fail := agent.MustFunc("fail", "", func(context.Context, struct{}) (string, error) {
		if err := agent.Cancel(ctx, j, "r", "the customer left"); err != nil {
			t.Errorf("Cancel = %v", err)
		}
		return "", errors.New("the card was declined")
	})
	subModel := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("s1", "book")}}, {calls: []agent.ToolUse{call("s2", "fail")}}, {text: "sub"}}}
	sub := p14Build(t, subModel, j, agent.WithTools(book, fail))
	parentModel := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{{ID: "p1", Name: "helper", Args: []byte(`{"task":"x"}`)}}}, {text: "done"}}}
	parent := p14Build(t, parentModel, j, agent.WithTools(agent.MustSubAgent("helper", "", sub)))
	_, err := parent.Run(ctx, "r", agent.UserText("go"), agent.WithSaga())
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
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
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
	if _, err := s.Send(ctx, agent.UserText("one"), agent.WithSaga()); err == nil {
		t.Fatal("want the approval pause")
	}
	if err := agent.Cancel(ctx, j, "s>@turn/0", "stop"); err != nil {
		t.Fatalf("Cancel = %v", err)
	}
	_, err = s.Send(ctx, agent.UserText("two"))
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

// failCompleteOnce fails the first run:complete insert, as a crash between a run's answer and its
// end marker leaves it.
type failCompleteOnce struct {
	*agent.MemStore
	done bool
}

func (s *failCompleteOnce) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if name == "run:complete" && !s.done {
		s.done = true
		return agent.Entry{}, false, errors.New("injected: the store went away")
	}
	return s.MemStore.Insert(ctx, runID, name, data)
}

// Suspicion 3, the check after the rollback: a cancelled saga turn whose answer was recorded
// before the request completes when the session drives it (rule 5), and is not closed; the session
// drives it once, then refuses the other message (the turn is the first message's to finish), and
// does not drive it again and again.
func TestRev138c_TurnRollbackThatCompletesIsDrivenOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // a mutant loops until it
	defer cancel()
	st := &failCompleteOnce{MemStore: agent.NewMemStore()}
	j, err := agent.NewJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	model := &p14Model{turns: []p14Turn{{text: "answer"}}}
	a := p14Build(t, model, j)
	s, err := a.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, agent.UserText("one"), agent.WithSaga()); err == nil {
		t.Fatal("want the injected failure of run:complete")
	}
	if err := agent.Cancel(ctx, j, "s>@turn/0", "stop"); err != nil {
		t.Fatalf("Cancel = %v", err)
	}
	if _, err := s.Send(ctx, agent.UserText("two")); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("the next message = %v, want ErrConfig: turn 0 completed (its answer was recorded first), it is not closed", err)
	}
	if got, _ := agent.Status(ctx, j, "s>@turn/0"); got.State != agent.RunCompleted {
		t.Fatalf("turn 0: Status %s, want completed", got.State)
	}
	if n := model.calls.Load(); n != 1 {
		t.Fatalf("model calls = %d, want 1", n)
	}
}

// Suspicion 2 under a root that is not a saga: a programmatic saga sub-run of a plain run fails
// after the root's Cancel (run:cancelled, no rollback request); its failure rollback ends it
// run:cancelled too.
func TestRev138c_SagaSubRunOfAPlainRootEndsCancelled(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var undone counter
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { undone.n.Add(1); return nil })
	fail := agent.MustFunc("fail", "", func(context.Context, struct{}) (string, error) {
		if err := agent.Cancel(ctx, j, "r", "the customer left"); err != nil {
			t.Errorf("Cancel = %v", err)
		}
		return "", errors.New("the card was declined")
	})
	subModel := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("s1", "book")}}, {calls: []agent.ToolUse{call("s2", "fail")}}, {text: "sub"}}}
	sub := p14Build(t, subModel, j, agent.WithTools(book, fail))
	var subID string
	work := agent.MustFunc("work", "", func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := agent.RunInfoFrom(ctx)
		subID = info.SubRunFor("w")
		_, err := sub.Run(ctx, subID, agent.UserText("x"), agent.WithSaga())
		return "worked", err
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	parentModel := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("p1", "work")}}, {text: "done"}}}
	parent := p14Build(t, parentModel, j, agent.WithTools(work))
	_, err := parent.Run(ctx, "r", agent.UserText("go"))
	if st, _ := agent.Status(ctx, j, "r"); st.State != agent.RunCancelled {
		t.Fatalf("the root's Status = %s (run %v), want cancelled", st.State, err)
	}
	if undone.n.Load() != 1 {
		t.Fatalf("compensated %d, want 1", undone.n.Load())
	}
	if st, _ := agent.Status(ctx, j, subID); st.State != agent.RunCancelled || st.Terminal != "the customer left" {
		t.Fatalf("the sub-run's Status = %+v, want cancelled with the root's reason", st)
	}
}

// srbStore is a MemStore seen only as a Store: no Unwrap, so the session finds no Leaser.
type srbStore struct{ agent.Store }

// srbSetup builds a session whose saga turn for "one" ran book, paused on pay's approval, and was
// cancelled, so the next message drives the turn's rollback, which runs book's compensator. The
// compensator signals in (once), waits for out, counts the compensations in flight at once, and
// sets done before it returns.
func srbSetup(t *testing.T, leaser bool, onComp func()) (s *agent.Session, in, out chan struct{}, maxInFlight *atomic.Int32, done *atomic.Bool) {
	t.Helper()
	ctx := context.Background()
	var j *agent.Journal
	if leaser {
		j, _ = p14Journal(t)
	} else {
		store := srbStore{agent.NewMemStore()}
		if _, ok := agent.Capability[agent.Leaser](store); ok {
			t.Fatal("the store exposes a Leaser")
		}
		var err error
		if j, err = agent.NewJournal(store); err != nil {
			t.Fatal(err)
		}
	}
	in, out = make(chan struct{}), make(chan struct{})
	maxInFlight, done = new(atomic.Int32), new(atomic.Bool)
	var inFlight atomic.Int32
	var once sync.Once
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error {
			n := inFlight.Add(1)
			for m := maxInFlight.Load(); n > m && !maxInFlight.CompareAndSwap(m, n); m = maxInFlight.Load() {
			}
			if onComp != nil {
				onComp()
			}
			once.Do(func() { close(in) })
			<-out
			inFlight.Add(-1)
			done.Store(true)
			return nil
		})
	var c counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "book")}}, {calls: []agent.ToolUse{call("c2", "pay")}}, {text: "done"}, {text: "done2"}}}
	a := p14Build(t, model, j, agent.WithTools(book, c.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	var err error
	if s, err = a.Session(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, agent.UserText("one"), agent.WithSaga()); err == nil {
		t.Fatal("want the approval pause")
	}
	if err := agent.Cancel(ctx, j, "s>@turn/0", "stop"); err != nil {
		t.Fatal(err)
	}
	return s, in, out, maxInFlight, done
}

// Over a store with a Leaser, a second caller on the same handle while the first drives a
// cancelled saga turn's rollback (SRb, without s.mu) does not wait for it: it finds the turn run
// leased and gets ErrTurnContended, as a caller on another handle does.
func TestRev138c_SiblingSendDuringRollback(t *testing.T) {
	ctx := context.Background()
	s, in, out, maxInFlight, _ := srbSetup(t, true, nil)
	errA := make(chan error, 1)
	go func() { _, err := s.Send(ctx, agent.UserText("two")); errA <- err }()
	<-in // A is inside the compensator, without s.mu
	_, errB := s.Send(ctx, agent.UserText("three"))
	close(out)
	if err := <-errA; err != nil && !agent.IsPause(err) && !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("A: %v", err) // its own turn pauses (pay), or B's open turn refuses it
	}
	if !errors.Is(errB, agent.ErrTurnContended) {
		t.Fatalf("a sibling Send on the same handle during the rollback = %v, want ErrTurnContended", errB)
	}
	if n := maxInFlight.Load(); n != 1 {
		t.Fatalf("%d compensations ran at once", n)
	}
}

// Over a store with no Leaser, nothing keeps two drivers off one turn run, so the session holds
// s.mu across the rollback: the compensator runs with the handle's mutex held.
func TestRev138c_RollbackHoldsTheSessionMutexWithoutALeaser(t *testing.T) {
	ctx := context.Background()
	var s *agent.Session
	var held bool
	s, in, out, _, _ := srbSetup(t, false, func() { held = agent.SessionMuHeld(s) })
	close(out)
	if _, err := s.Send(ctx, agent.UserText("two")); err != nil && !agent.IsPause(err) {
		t.Fatal(err) // its own turn pauses on pay's approval
	}
	select {
	case <-in:
	default:
		t.Fatal("the turn's compensator did not run")
	}
	if !held {
		t.Fatal("over a store with no Leaser, the rollback ran without the session's mutex")
	}
}

// Over a store with no Leaser, a second caller on the same handle waits for the first's rollback
// of the cancelled saga turn instead of driving the same rollback concurrently: one compensation
// at a time, and the second caller returns only once the rollback is over. (The mutex itself is
// pinned by TestRev138c_RollbackHoldsTheSessionMutexWithoutALeaser; here B may reach the handle
// before or after A's rollback is released, and either way must not drive it too.)
func TestRev138c_SiblingSendWaitsForRollbackWithoutALeaser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute) // bounds a regression that blocks B
	defer cancel()
	s, in, out, maxInFlight, done := srbSetup(t, false, nil)
	errA := make(chan error, 1)
	go func() { _, err := s.Send(ctx, agent.UserText("two")); errA <- err }()
	<-in // A is inside the compensator
	errB := make(chan error, 1)
	returnedEarly := make(chan bool, 1)
	go func() {
		_, err := s.Send(ctx, agent.UserText("three"))
		returnedEarly <- !done.Load()
		errB <- err
	}()
	close(out)
	if err := <-errA; err != nil && !agent.IsPause(err) && !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("A: %v", err) // its own turn pauses (pay), or B's open turn refuses it
	}
	if <-returnedEarly {
		t.Fatal("B returned while A's rollback was in progress")
	}
	if err := <-errB; errors.Is(err, agent.ErrTurnContended) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("B = %v", err)
	}
	if n := maxInFlight.Load(); n != 1 {
		t.Fatalf("%d compensations ran at once", n)
	}
}
