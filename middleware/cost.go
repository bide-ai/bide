package middleware

import (
	"context"
	"sync"

	agent "github.com/dayna/go-agents"
)

// Rates holds the billing rates for a model in USD per 1,000,000 tokens.
type Rates struct {
	InputPer1M      float64
	OutputPer1M     float64
	CacheReadPer1M  float64
	CacheWritePer1M float64
}

// Cost returns the USD cost of the token usage u at these rates.
func (r Rates) Cost(u agent.Usage) float64 {
	return float64(u.InputTokens)/1e6*r.InputPer1M +
		float64(u.OutputTokens)/1e6*r.OutputPer1M +
		float64(u.CacheReadTokens)/1e6*r.CacheReadPer1M +
		float64(u.CacheWriteTokens)/1e6*r.CacheWritePer1M
}

// CostMeter accumulates token usage and computed cost across model calls.
// It is safe for concurrent use.
type CostMeter struct {
	mu    sync.Mutex
	total float64
	usage agent.Usage
}

// Total returns the accumulated USD cost so far.
func (m *CostMeter) Total() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

// Usage returns the accumulated token usage so far.
func (m *CostMeter) Usage() agent.Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usage
}

// Cost is a model middleware that records token usage and computes running cost
// after each successful model call. The caller reads accumulated values via m.
func Cost(m *CostMeter, r Rates) agent.Middleware {
	return func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
			msg, u, err := next(ctx, req)
			if err != nil {
				return msg, u, err
			}
			cost := r.Cost(u)

			m.mu.Lock()
			m.total += cost
			m.usage.InputTokens += u.InputTokens
			m.usage.OutputTokens += u.OutputTokens
			m.usage.CacheReadTokens += u.CacheReadTokens
			m.usage.CacheWriteTokens += u.CacheWriteTokens
			m.mu.Unlock()

			return msg, u, nil
		}
	}
}
