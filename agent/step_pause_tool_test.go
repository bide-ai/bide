package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// A retry-safe tool whose body runs a side-effect Step per call, and the Step's fn fires its
// effect and then pauses (the misuse B3's guard is for). Before #92 the pause stopped the run, and
// a resume halted on the step's marker. With the guard, Step returns ErrConfig, the loop records
// it as an ordinary tool failure, the model is told the call failed and calls again under a new
// id, and the effect fires a second time.
func TestStepPauseGuard_InsideAToolIsNotRecordedAsAToolFailure(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	fired := 0
	type in struct {
		ID string `json:"id"`
	}
	book := agent.MustFunc("book", "", func(ctx context.Context, a in) (string, error) {
		return store.Step(ctx, "wf", "charge-"+a.ID, func(ctx context.Context) (string, error) {
			fired++ // the side effect
			return agent.Interrupt[string](ctx, "confirm-"+a.ID, "confirm the charge?")
		})
	}, agent.WithSafety(agent.Safety{Idempotent: true}))
	m := agenttest.NewScriptedModel(
		agenttest.ToolTurn("c1", "book", `{"id":"c1"}`),
		agenttest.ToolTurn("c2", "book", `{"id":"c2"}`), // the model retries a call it was told failed
		agenttest.TextTurn("done"))
	a := agenttest.MustNew(m, store, agent.WithTools(book))
	_, err := a.Run(ctx, "r", agent.UserText("book it"))
	if !errors.Is(err, agent.ErrConfig) || errors.Is(err, agent.ErrTool) {
		t.Errorf("Run = %v; want the guard's ErrConfig, not a tool failure", err)
	}
	// The call recorded nothing: a resume runs it again, and the step halts on its marker.
	_, err = a.Run(ctx, "r", agent.UserText("book it"))
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Errorf("resume = %v; want the step's halt", err)
	}
	if fired != 1 {
		t.Errorf("the side effect fired %d times, want once", fired)
	}
}
