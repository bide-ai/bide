package middleware_test

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// (d) A call a middleware kept reaches a Cost after its turn is over, answered by a cache below
// Cost without reaching the model: Cost must not count it as a second answer of the turn.
func TestAdv104_KeptCallAfterTurnIsNotAnAnswer(t *testing.T) {
	var stored agent.ModelCall
	var next0 agent.ModelHandler
	keep := func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			stored, next0 = call, next
			return next(ctx, call)
		}
	}
	cache := func(agent.ModelHandler) agent.ModelHandler {
		return func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
			return agent.ModelResponse{Message: agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.Text{Text: "cached"}}}, Usage: billed}, nil
		}
	}
	var meter middleware.CostMeter
	a := agent.New(&stubModel{text: "x"}, agent.NewMemStore()).Use(keep, middleware.Cost(&meter, perInput), cache)
	if _, err := a.Run(context.Background(), "r", "q"); err != nil {
		t.Fatal(err)
	}
	before := meter.Snapshot()
	if _, err := next0(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if after := meter.Snapshot(); after != before || before.Answer != billed {
		t.Fatalf("meter %+v after the kept call, %+v before; want one answer %+v", after, before, billed)
	}
}
