package middleware_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// Cost rejects negative token counts rather than lowering its totals.
func TestCost_RejectsNegativeUsage(t *testing.T) {
	var meter middleware.CostMeter
	h := middleware.Cost(&meter, perInput)(func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
		return agent.ModelResponse{Usage: agent.Usage{InputTokens: -50}}, nil
	})
	if _, err := h(context.Background(), agent.ModelCall{}); !errors.Is(err, agent.ErrProtocol) {
		t.Fatalf("err = %v, want a protocol error", err)
	}
	if s := meter.Snapshot(); s != (middleware.CostSnapshot{}) {
		t.Fatalf("meter changed: %+v", s)
	}
}
