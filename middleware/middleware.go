// Package middleware provides composable func(Handler) Handler batteries for the model
// call — the "LiteLLM for Go" layer at the semantic level (handlers see messages, tool
// calls, and token usage, not raw bytes). Attach with agent.Agent.Use.
//
//	a := agent.New(model, store, tools...).Use(
//		middleware.Retry(3, middleware.WithRetryIf(middleware.Retryable)),
//		middleware.RateLimit(middleware.NewRateLimiter(time.Second, 5)),
//	)
//
// Per-run limits that must hold across a resume live on the agent, where the journal is:
// Agent.WithTokenBudget and Agent.WithMaxTurns.
package middleware
