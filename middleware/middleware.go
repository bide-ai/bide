// Package middleware provides composable func(Handler) Handler batteries for the model
// call — the "LiteLLM for Go" layer at the semantic level (handlers see messages, tool
// calls, and token usage, not raw bytes). Attach with the agent.WithMiddleware option (and tool
// middleware with agent.WithToolMiddleware).
//
//	a, err := agent.New(model, journal, agent.WithTools(tools...), agent.WithMiddleware(
//		middleware.Retry(3, middleware.WithRetryIf(middleware.Retryable)),
//		middleware.RateLimit(middleware.NewRateLimiter(time.Second, 5)),
//	))
//
// Per-run limits that must hold across a resume live on the agent, where the journal is:
// agent.WithTokenBudget and agent.WithMaxTurns.
package middleware
