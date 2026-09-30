package agent_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A sub-agent's saga that aborts because its tool failed is the sub-agent's verdict, even when
// the tool's error wraps ErrStorage (a tool that writes to a database): the parent's saga aborts
// on it and settles. Only a failure of the sub-run's own journal is not a verdict (see
// TestRefModel_SubAgentStorageFailureIsNotItsAnswer).
func TestSubAgent_SagaAbortWrappingStorageIsAVerdict(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	save := agent.Func("save", "", agent.Safety{}, func(context.Context, struct{}) (struct{}, error) {
		return struct{}{}, fmt.Errorf("insert row: %w", agent.ErrStorage)
	})
	sub := agent.New(agent.NewScriptedModel(agent.ToolTurn("s1", "save", `{}`), agent.TextTurn("saved")), store, save)
	root := agent.New(agent.NewScriptedModel(agent.ToolTurn("c1", "saver", `{"task":"save it"}`), agent.TextTurn("done")),
		store, agent.SubAgent("saver", "", sub))
	for attempt := range 3 {
		_, err := root.RunSaga(ctx, "r", "go")
		var sa *agent.SagaAborted
		if !errors.As(err, &sa) || sa.RunID != "r" || sa.CompensateErr != nil {
			t.Fatalf("attempt %d: RunSaga = %v; want the root saga aborted, rolled back", attempt, err)
		}
	}
}
