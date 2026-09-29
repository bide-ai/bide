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
	_, err := New(m, NewMemStore(), mine, theirs).Run(context.Background(), "r1", "who is alice?")
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("run err = %v, want ErrConfig for two tools named lookup", err)
	}
	if local+remote != 0 {
		t.Fatalf("a tool ran (local %d, remote %d); want none", local, remote)
	}
}
