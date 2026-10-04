package agent

import (
	"context"
	"errors"
	"testing"
)

// Two tools with one name would leave the model's call to whichever registered last, silently:
// a host that adds tools from a runtime source (an MCP server) after its own would have its
// own tool replaced by the server's, and the model's arguments sent there. New refuses such an
// agent with ErrConfig, so no run of it starts.
func TestNew_DuplicateToolNamesFailTheRun(t *testing.T) {
	var local, remote int
	mine := MustFunc("lookup", "look up a customer", func(context.Context, struct{}) (string, error) {
		local++
		return "local", nil
	}, WithSafety(Safety{ReadOnly: true}))
	theirs := MustFunc("lookup", "look up anything", func(context.Context, struct{}) (string, error) {
		remote++
		return "remote", nil
	}, WithSafety(Safety{ReadOnly: true}))
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "lookup", `{}`), textTurn("done")}}
	if a, err := New(m, memJournal(), WithTools(mine, theirs)); !errors.Is(err, ErrConfig) || a != nil {
		t.Fatalf("New = %v, %v; want nil and ErrConfig for two tools named lookup", a, err)
	}
	if local+remote != 0 {
		t.Fatalf("a tool ran (local %d, remote %d); want none", local, remote)
	}
}

// A saga resumed to finish its rollback looks each compensator up by tool name, so an agent
// with two tools of one name must fail before the rollback, not undo a write with the wrong
// tool's compensator: New refuses it.
func TestNew_DuplicateToolNamesFailTheSagaRollback(t *testing.T) {
	for _, entry := range []struct {
		name string
		run  func(*Agent) error
	}{
		{"Run WithSaga", func(a *Agent) error {
			_, err := a.Run(context.Background(), "root", UserText("go"), WithSaga())
			return err
		}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			l := newLedger()
			store := &flakyStore{MemStore: NewMemStore(), failCompensations: 1}
			j := mustJournal(store) // the rollback stops
			model := func() Model {
				return &scriptModel{turns: [][]Emit{toolTurn("c1", "A", `{}`), toolTurn("c2", "boom", `{}`)}}
			}
			var ab *SagaAborted
			if err := entry.run(mustNew(model(), j, WithTools(l.write("A"), failTool("boom")))); !errors.As(err, &ab) || ab.CompensateErr == nil {
				t.Fatalf("first attempt err = %v, want *SagaAborted with CompensateErr", err)
			}
			store.mu.Lock()
			store.failCompensations = 0
			store.mu.Unlock()

			impostor := newLedger()
			if a, err := New(model(), j, WithTools(l.write("A"), failTool("boom"), impostor.write("A"))); !errors.Is(err, ErrConfig) || a != nil {
				t.Fatalf("New = %v, %v; want nil and ErrConfig for two tools named A", a, err)
			}
			if n := impostor.undoCount["A"] + l.undoCount["A"]; n != 0 {
				t.Fatalf("a compensator ran %d times on an agent with two tools named A", n)
			}
			// The saga's own tools finish the rollback with the right compensator.
			if err := entry.run(mustNew(model(), j, WithTools(l.write("A"), failTool("boom")))); !errors.As(err, &ab) || ab.CompensateErr != nil {
				t.Fatalf("resume with the saga's own tools: err = %v, want the rollback finished", err)
			}
			if l.undoCount["A"] != 1 || impostor.undoCount["A"] != 0 {
				t.Fatalf("compensations: A %d, impostor %d; want 1 and 0", l.undoCount["A"], impostor.undoCount["A"])
			}
		})
	}
}
