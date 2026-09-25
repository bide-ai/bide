# Reliability middleware

The agent loop is thin; the reliability it needs (timeouts, retries, rate limits, cost tracking,
hedging) lives in composable middleware in [`middleware/`](../middleware). Model middleware wraps
the model call and is attached with `agent.Use`; tool middleware wraps tool execution. All of it is
optional and stdlib-only, and it rides the durable substrate, so a retried or hedged call is still
journaled at most once and a crash still resumes safely.

```go
a := agent.New(model, store, tools...).
    Use(
        middleware.Hedge(800*time.Millisecond, backupModel),                 // race a backup on the tail
        middleware.Retry(3, middleware.WithRetryIf(middleware.Retryable),    // then retry transient failures
            middleware.WithTimeout(30*time.Second)),                         // each attempt bounded
        middleware.RateLimit(middleware.NewRateLimiter(time.Second, 5)),     // cap model call rate
    )
```

`Use` composes outermost-first: the first middleware listed sees the call first and the model last.
Put `Hedge` outermost (it should race whole attempts), `Retry` inside it (retry a target that
failed), and `RateLimit` innermost (throttle actual calls). Order is a real choice; this is the
usual one.

## Model middleware (`agent.Use`)

### Timeouts and retries: `Retry`

`Retry(n, opts...)` retries the model call up to `n` extra times, sleeping with full-jitter
exponential backoff between attempts and honoring context cancellation.

- `WithBackoff(base, max)` sets the initial and capped sleep (defaults 200ms / 10s). If the error is
  `*agent.RateLimited`, its `Retry-After` hint overrides the computed backoff.
- `WithTimeout(d)` bounds each attempt with its own deadline, so a hung call fails that attempt (and
  is retried) instead of blocking the run. The parent context still governs overall cancellation.
- `WithRetryIf(pred)` classifies which errors are worth retrying. Pass the ready-made `Retryable`:
  it retries `*agent.RateLimited`, per-attempt `context.DeadlineExceeded`, and `*agent.APIError`
  with a 5xx or 408 status; it fails fast on parent cancellation and 4xx (auth, validation), so you
  do not burn attempts or tokens on terminal errors.

```go
a.Use(middleware.Retry(3, middleware.WithRetryIf(middleware.Retryable), middleware.WithTimeout(30*time.Second)))
```

### Hedging: `Hedge`

`Hedge(delay, backups...)` races the model call against one or more backup models and returns the
first successful response, cancelling the rest. See [the hedge design and boundaries](#when-to-hedge-vs-retry)
below; the running demo is [`examples/hedge`](../examples/hedge/main.go).

```go
// Primary is Anthropic; if it is quiet for 800ms, also try OpenAI and take the first good answer.
a.Use(middleware.Hedge(800*time.Millisecond, openaiModel))
```

- The primary fires immediately; backups fire after `delay` (so you pay for a backup only when the
  primary is slow), or immediately if `delay` is 0.
- A target that errors while backups are still pending brings the backups forward at once (fast
  failover), rather than waiting out the delay.
- The first nil-error result wins; losers are cancelled via a derived context. If all fail, the
  joined error is returned. With no backups it is a pass-through.

### Rate limiting: `RateLimit`

`RateLimit(NewRateLimiter(interval, burst))` caps the model call rate with a token bucket (one token
per `interval`, up to `burst` in reserve), blocking until a token is free or the context is
cancelled. Share one `*RateLimiter` across agents to cap an aggregate rate.

```go
rl := middleware.NewRateLimiter(time.Second, 5) // 5 calls/sec sustained, burst 5
a.Use(middleware.RateLimit(rl))
```

### Cost tracking: `Cost`

`Cost(meter, rates)` accumulates token usage into a `*CostMeter` at the per-token `Rates` you set,
so you can read spend across a run without touching the loop.

```go
meter := &middleware.CostMeter{}
a.Use(middleware.Cost(meter, middleware.Rates{InputPer1M: 3.00, OutputPer1M: 15.00})) // USD per 1M tokens
// ... after running ...
fmt.Printf("spent $%.4f, usage %+v\n", meter.Total(), meter.Usage())
```

## Tool middleware

Tool execution has its own wrappers (attached where you build the agent's tool set):

- `ToolRetry(n, opts...)` retries a tool call with the same backoff/classification options as
  `Retry`. Use it for flaky read-only or idempotent tools; a non-idempotent tool is still guarded
  by the at-most-once journal and `Safety`, so retries never double-fire a recorded effect.
- `ToolRateLimit(rl)` caps a tool's call rate (share a `*RateLimiter` to bound a downstream API).
- `ToolCache()` memoizes a tool's result for identical arguments within a run.
- `ToolLog(logf)` logs each tool call and result.

## When to hedge vs retry

Retry is reactive and sequential: it waits for a failure, then tries again. Hedge is proactive and
concurrent: it launches redundant attempts and takes the first. They compose (hedge for the tail and
for availability, retry for terminal-after-all-fail), and both are safe here because the durable loop
records only the winning response, at most once.

What hedging does not do, stated so it is not oversold:

- It reduces the tail (p95/p99), not the median; a single fast call is still fast.
- It is latency and availability, not correctness. Taking the first answer is not the same as
  requiring agreement across models (that would be a governance concern, not this).
- It costs extra generations on hedged requests; a positive `delay` limits that to the cases where
  the primary is actually slow.
- It hedges only the model generation. Tool calls and other side effects run in the loop above the
  middleware, under the at-most-once journal and `Safety`.
