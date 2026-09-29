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
	h := middleware.Cost(&meter, perInput)(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
		return agent.Message{}, agent.Usage{InputTokens: -50}, nil
	})
	if _, _, err := h(context.Background(), agent.Request{}); !errors.Is(err, agent.ErrProtocol) {
		t.Fatalf("err = %v, want a protocol error", err)
	}
	if meter.Total() != 0 || meter.Usage() != (agent.Usage{}) || meter.Spent() != (agent.Usage{}) {
		t.Fatalf("meter changed: Total %v Usage %+v Spent %+v", meter.Total(), meter.Usage(), meter.Spent())
	}
}
