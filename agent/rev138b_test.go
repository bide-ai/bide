package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

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
	parent := p14Build(t, parentModel, j, agent.WithTools(agent.SubAgent("helper", "", sub)))
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
		t.Errorf("RunMessage = %v, want ErrRunCancelled", err)
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
	mid := p14Build(t, midModel, j2, agent.WithTools(agent.SubAgent("inner", "", inner)))
	parentModel := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{{ID: "p1", Name: "mid", Args: []byte(`{"task":"x"}`)}}}, {text: "done"}}}
	parent := p14Build(t, parentModel, j, agent.WithTools(agent.SubAgent("mid", "", mid)))
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
