// Package middleware provides composable func(Handler) Handler batteries for the model
// call — the "LiteLLM for Go" layer at the semantic level (handlers see messages, tool
// calls, and token usage, not raw bytes). Attach with agent.Agent.Use.
//
//	a := agent.New(model, store, tools...).Use(
//		middleware.Retry(3),
//		middleware.TokenBudget(100_000),
//	)
package middleware

import (
	"context"
	"fmt"
	"sync"

	"github.com/blackwell-systems/bide/agent"
)

// TokenBudget aborts the run once cumulative tokens (input+output) across the run's
// model calls exceed max. The cap is a HARD ceiling: the call that would exceed it is
// refused. State is per-middleware-instance, so it accumulates across the loop's turns.
func TokenBudget(max int) agent.Middleware {
	var (
		mu   sync.Mutex
		used int
	)
	return func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
			mu.Lock()
			over := used >= max
			mu.Unlock()
			if over {
				return agent.Message{}, agent.Usage{}, fmt.Errorf("%d used, cap %d: %w", used, max, agent.ErrBudgetExceeded)
			}
			msg, u, err := next(ctx, req)
			if err == nil {
				mu.Lock()
				used += u.InputTokens + u.OutputTokens
				mu.Unlock()
			}
			return msg, u, err
		}
	}
}
