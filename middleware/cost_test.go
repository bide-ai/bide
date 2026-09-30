package middleware

import (
	"context"
	"math"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func TestCost_AccumulatesCorrectly(t *testing.T) {
	rates := Rates{
		InputPer1M:      3.0,  // $3 / 1M input
		OutputPer1M:     15.0, // $15 / 1M output
		CacheReadPer1M:  0.3,  // $0.30 / 1M cache read
		CacheWritePer1M: 3.75, // $3.75 / 1M cache write
	}
	u := agent.Usage{
		InputTokens:      1_000_000,
		OutputTokens:     500_000,
		CacheReadTokens:  200_000,
		CacheWriteTokens: 100_000,
	}
	// Expected:
	//   input:       1.0 * 3.0       = 3.0
	//   output:      0.5 * 15.0      = 7.5
	//   cacheRead:   0.2 * 0.3       = 0.06
	//   cacheWrite:  0.1 * 3.75      = 0.375
	//   total                        = 10.935
	wantTotal := 10.935

	var meter CostMeter
	base := agent.ModelHandler(func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
		return agent.ModelResponse{Usage: u}, nil
	})
	h := Cost(&meter, rates)(base)

	ctx := context.Background()
	if _, err := h(ctx, agent.ModelCall{}); err != nil {
		t.Fatal(err)
	}
	// Second call — total doubles.
	if _, err := h(ctx, agent.ModelCall{}); err != nil {
		t.Fatal(err)
	}

	gotTotal := meter.Snapshot().AnswerUSD
	if math.Abs(gotTotal-wantTotal*2) > 1e-9 {
		t.Errorf("Total() = %v, want %v", gotTotal, wantTotal*2)
	}

	gotUsage := meter.Snapshot().Answer
	if gotUsage.InputTokens != u.InputTokens*2 {
		t.Errorf("Usage().InputTokens = %d, want %d", gotUsage.InputTokens, u.InputTokens*2)
	}
	if gotUsage.OutputTokens != u.OutputTokens*2 {
		t.Errorf("Usage().OutputTokens = %d, want %d", gotUsage.OutputTokens, u.OutputTokens*2)
	}
	if gotUsage.CacheReadTokens != u.CacheReadTokens*2 {
		t.Errorf("Usage().CacheReadTokens = %d, want %d", gotUsage.CacheReadTokens, u.CacheReadTokens*2)
	}
	if gotUsage.CacheWriteTokens != u.CacheWriteTokens*2 {
		t.Errorf("Usage().CacheWriteTokens = %d, want %d", gotUsage.CacheWriteTokens, u.CacheWriteTokens*2)
	}
}

func TestCost_SkipsOnError(t *testing.T) {
	var meter CostMeter
	base := agent.ModelHandler(func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
		return agent.ModelResponse{Usage: agent.Usage{InputTokens: 999}}, context.Canceled
	})
	h := Cost(&meter, Rates{InputPer1M: 1.0})(base)
	if _, err := h(context.Background(), agent.ModelCall{}); err == nil {
		t.Fatal("expected error")
	}
	if meter.Snapshot().AnswerUSD != 0 {
		t.Errorf("Total() = %v, want 0 (no cost on error)", meter.Snapshot().AnswerUSD)
	}
}
