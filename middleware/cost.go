package middleware

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bide-ai/bide/agent"
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

// CostMeter accumulates token usage and computed cost across model calls, in two views: the
// answers (Usage, Total), the responses the calls returned, which a run records; and the spend
// (Spent, SpentTotal), every request the calls sent, including failed attempts a Retry repeated
// and losing Hedge targets, which the provider bills too. It is safe for concurrent use.
type CostMeter struct {
	mu         sync.Mutex
	total      float64
	usage      agent.Usage
	spentTotal float64
	spent      agent.Usage
}

// Total returns the accumulated USD cost of the answers so far.
func (m *CostMeter) Total() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

// Usage returns the accumulated token usage of the answers so far.
func (m *CostMeter) Usage() agent.Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usage
}

// Spent returns the accumulated token usage of every model request sent so far.
func (m *CostMeter) Spent() agent.Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spent
}

// SpentTotal returns the accumulated USD cost of every model request sent so far.
func (m *CostMeter) SpentTotal() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spentTotal
}

func addTo(dst *agent.Usage, u agent.Usage) {
	dst.InputTokens += u.InputTokens
	dst.OutputTokens += u.OutputTokens
	dst.CacheReadTokens += u.CacheReadTokens
	dst.CacheWriteTokens += u.CacheWriteTokens
}

func (m *CostMeter) addSpent(r Rates, u agent.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spentTotal += r.Cost(u)
	addTo(&m.spent, u)
}

// Cost is a model middleware that records token usage and computes running cost. The answer view
// counts each successful call's returned usage. The spend view counts every request the call
// sends, wherever Cost sits in the chain: under an agent, each attempt of a Retry and each target
// of a Hedge below it (see agent.ModelCallHook); for a handler called outside an agent, the
// usage the call returns, successful or not. The caller reads accumulated values via m.
func Cost(m *CostMeter, r Rates) agent.Middleware {
	return func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
			hctx, hooked := agent.WithModelCallHook(ctx, agent.ModelCallHook{After: func(u agent.Usage) { m.addSpent(r, u) }})
			msg, u, err := next(hctx, req)
			if verr := u.Validate(); verr != nil {
				// A negative count would lower the totals: reject it, and record nothing.
				return agent.Message{}, agent.Usage{}, errors.Join(err, fmt.Errorf("middleware: cost: %w", verr))
			}
			if !hooked {
				m.addSpent(r, u)
			}
			if err != nil {
				return msg, u, err
			}
			m.mu.Lock()
			m.total += r.Cost(u)
			addTo(&m.usage, u)
			m.mu.Unlock()
			return msg, u, nil
		}
	}
}
