package middleware

import (
	"context"
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
// answers, the responses the calls returned, which a run records; and the spend, every request the
// calls sent, including failed attempts a Retry repeated and losing Hedge targets, which the
// provider bills too. Read both with Snapshot. It is safe for concurrent use.
type CostMeter struct {
	mu sync.Mutex
	s  CostSnapshot
}

// CostSnapshot is a CostMeter's totals at one moment. Answer and AnswerUSD are the usage and USD
// cost of the responses the calls returned; Spend and SpendUSD those of every request the calls
// sent.
type CostSnapshot struct {
	Answer, Spend       agent.Usage
	AnswerUSD, SpendUSD float64
}

// Snapshot returns the meter's totals, read together under one lock, so the answer and spend
// views are always of the same moment.
func (m *CostMeter) Snapshot() CostSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s
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
	m.s.SpendUSD += r.Cost(u)
	addTo(&m.s.Spend, u)
}

func (m *CostMeter) addAnswer(r Rates, u agent.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s.AnswerUSD += r.Cost(u)
	addTo(&m.s.Answer, u)
}

// Cost is a model middleware that records token usage and computes running cost, wherever it
// sits in the chain.
//
// The answer view counts the usage of each call's answer, the response the agent records (see
// agent.ModelCall.OnAnswer), exactly once per call: a Cost inside a Hedge does not count a losing
// target's response, and a Cost inside a Retry does not count a response a middleware above it
// rejected. The spend view counts every request the call sends, through a hook it adds to the call
// (see agent.ModelCallHook): each attempt of a Retry and each target of a Hedge below it, the usage
// a failed request reported before failing, and the discarded spend a replayed turn reports
// (agent.ModelAttempt.Discarded). Outside an agent, call the model through agent.CallModel, whose
// model handler runs the hooks and reports the answer too; a Cost handler called directly counts
// the response it returns as the answer. The caller reads the totals with m.Snapshot.
func Cost(m *CostMeter, r Rates) agent.Middleware {
	hook := agent.ModelCallHook{After: func(_ context.Context, _ agent.ModelCall, a agent.ModelAttempt) {
		m.addSpent(r, a.Response.Usage)
		m.addSpent(r, a.Discarded)
	}}
	key := new(int) // this Cost's registration, one per turn (see agent.ModelCall.OnAnswer)
	answer := func(_ context.Context, resp agent.ModelResponse) { m.addAnswer(r, resp.Usage) }
	return func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			inTurn := call.OnAnswer(key, answer)
			resp, err := next(ctx, call.AddHook(hook))
			if err != nil {
				return resp, err
			}
			if verr := resp.Usage.Validate(); verr != nil {
				// A negative count (from a middleware inside Cost that built the response) would
				// lower the totals: reject it, and record nothing.
				return agent.ModelResponse{}, fmt.Errorf("middleware: cost: %w", verr)
			}
			if !inTurn {
				m.addAnswer(r, resp.Usage)
			}
			return resp, nil
		}
	}
}
