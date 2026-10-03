package agent

import (
	"context"
	"errors"
	"testing"
)

// Two tools with one name leave the model's call to whichever New registered last, silently:
// a host that adds tools from a runtime source (an MCP server) after its own would have its
// own tool replaced by the server's, and the model's arguments sent there. New cannot fail, so
// every run of such an agent must fail with ErrConfig before any tool runs.
func TestNew_DuplicateToolNamesFailTheRun(t *testing.T) {
	var local, remote int
	mine := Func("lookup", "look up a customer", Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) {
		local++
		return "local", nil
	})
	theirs := Func("lookup", "look up anything", Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) {
		remote++
		return "remote", nil
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "lookup", `{}`), textTurn("done")}}
	_, err := mustNew(m, memJournal(), WithTools(mine, theirs)).Run(context.Background(), "r1", UserText("who is alice?"))
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("run err = %v, want ErrConfig for two tools named lookup", err)
	}
	if local+remote != 0 {
		t.Fatalf("a tool ran (local %d, remote %d); want none", local, remote)
	}
}

// A saga resumed to finish its rollback looks each compensator up by tool name, so an agent
// with two tools of one name must fail before the rollback, not undo a write with the wrong
// tool's compensator.
func TestNew_DuplicateToolNamesFailTheSagaRollback(t *testing.T) {
	for _, entry := range []struct {
		name string
		run  func(*Agent) error
	}{
		{"RunSaga", func(a *Agent) error {
			_, err := a.Run(context.Background(), "root", UserText("go"), WithSaga())
			return err
		}},
		{"RunSagaResult", func(a *Agent) error {
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
			err := entry.run(mustNew(model(), j, WithTools(l.write("A"), failTool("boom"), impostor.write("A"))))
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("resume err = %v, want ErrConfig for two tools named A", err)
			}
			if n := impostor.undoCount["A"] + l.undoCount["A"]; n != 0 {
				t.Fatalf("a compensator ran %d times on an agent with two tools named A", n)
			}
		})
	}
}
