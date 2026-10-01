# Reliability middleware

The agent loop is thin; the reliability it needs (timeouts, retries, rate limits, cost tracking,
hedging) lives in composable middleware in [`middleware/`](../../middleware). Model middleware wraps
the model call and is attached with `agent.Use`; tool middleware wraps tool execution. All of it is
optional and stdlib-only, and it rides the durable substrate, so a retried or hedged call is still
journaled at most once and a crash still resumes safely.

<!-- docsnip: setup model agent.Model; store agent.Durable; tools []agent.Tool; backupModel agent.Model -->
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
Put `Hedge` outermost (it should race whole attempts) and `Retry` inside it (retry a target that
failed): middleware inside `Hedge` wraps every target, backups included. Order is a real choice;
this is the usual one. `RateLimit` and `Cost` count every request actually sent wherever they sit,
so each retried attempt and each hedged target takes a token and counts as spend: each adds a hook to
the call (`agent.ModelCall.AddHook`), and the agent's model handler runs every hook once around each
request it sends, numbering the requests of a turn 1, 2, 3 across every attempt and target
(`ModelCall.Attempt`).

## Model middleware (`agent.Use`)

### Timeouts and retries: `Retry`

`Retry(n, opts...)` retries the model call up to `n` extra times, sleeping with full-jitter
exponential backoff between attempts and honoring context cancellation. `n` must be 0 or more: with
a negative `n`, `Retry` and `ToolRetry` fail every call with `agent.ErrConfig` and call nothing.

- `WithBackoff(base, max)` sets the initial and capped sleep (defaults 200ms / 10s). If the error is
  `*agent.RateLimited`, its `Retry-After` hint overrides the computed backoff.
- `WithTimeout(d)` bounds each attempt with its own deadline, so a hung call fails that attempt (and
  is retried) instead of blocking the run. The parent context still governs overall cancellation.
- `WithRetryIf(pred)` classifies which errors are worth retrying. Pass the ready-made `Retryable`:
  it retries `*agent.RateLimited`, per-attempt `context.DeadlineExceeded`, and `*agent.APIError`
  with a 5xx or 408 status; it fails fast on parent cancellation and 4xx (auth, validation), so you
  do not burn attempts or tokens on terminal errors. It also fails fast on errors the same request
  would repeat: `agent.ErrConfig` (a request the adapter refused to build, such as a schema the
  provider cannot take), `agent.ErrQuotaExhausted` (used-up quota or credit, even when the provider
  sends it as a 429), `agent.ErrResponseTooLarge`, and `agent.ErrTruncatedToolArgs` (a tool call cut
  off by the output token limit, which the same request hits again). It retries
  `agent.ErrToolUseIDReused` (a model turn with a missing, reused, or malformed tool-use ID; a fresh
  attempt can issue valid ones), `agent.ErrStreamProtocol` (a stream that broke its provider's event
  protocol, a fault of that one response), and a provider's mid-stream server error. See [error surfacing](models.md#error-surfacing-shared-across-all-three-adapters)
  for how adapters classify provider errors.

<!-- docsnip: setup a *agent.Agent -->
```go
a.Use(middleware.Retry(3, middleware.WithRetryIf(middleware.Retryable), middleware.WithTimeout(30*time.Second)))
```

### Hedging: `Hedge`

`Hedge(delay, backups...)` races the model call against one or more backup models and returns the
first successful response, cancelling the rest. See [the hedge design and boundaries](#when-to-hedge-vs-retry)
below; the running demo is [`examples/hedge`](../../examples/hedge/main.go).

<!-- docsnip: setup a *agent.Agent; openaiModel agent.Model -->
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
- Every target, backups included, goes through the middleware listed after `Hedge` and the agent's
  checks on each model response.
- A backup is the same call with its `Model` set to the backup (`c := call; c.Model = backup`);
  nothing else changes.
- `Hedge` does not wait for the losers: it returns as soon as one target wins. The run counts a
  loser's spend with its next turn or, when the run ends first, waits for it (at most two seconds,
  and not past the run's context) and journals it in a late spend record (`@spend-late/<id>`). A
  loser that has not reached the model when the turn ends is not sent. A loser whose model ignores
  cancellation longer keeps running, with the middleware inside `Hedge` on its path, after the run
  has ended, and is not in `Result.Spend`.
- Streaming (`Agent.Stream`) needs no code in `Hedge`. One request of a turn streams live at a time,
  the first to start; the others run without streaming. If the streaming target wins, the caller saw
  it live. If another target wins, the caller gets `agent.TurnRestarted`, which retracts what was
  streamed, then the winner's response. Nothing a loser sends reaches the caller after the turn.

### Rate limiting: `RateLimit`

`RateLimit(NewRateLimiter(interval, burst))` caps the model call rate with a token bucket (one token
per `interval`, up to `burst` in reserve), blocking until a token is free or the context is
cancelled. Share one `*RateLimiter` across agents to cap an aggregate rate. It takes a token for
every request the agent sends, wherever it sits in the chain: each attempt of a `Retry` and each
target a `Hedge` launches waits for its own. An interval of 0 or less sets no limit. Waiters are not
served in arrival order: a new call can take a freed token ahead of one already waiting.

<!-- docsnip: setup a *agent.Agent -->
```go
rl := middleware.NewRateLimiter(time.Second, 5) // 5 calls/sec sustained, burst 5
a.Use(middleware.RateLimit(rl))
```

### Cost tracking: `Cost`

`Cost(meter, rates)` accumulates token usage into a `*CostMeter` at the per-token `Rates` you set,
so you can read spend across a run without touching the loop.

<!-- docsnip: setup a *agent.Agent -->
```go
meter := &middleware.CostMeter{}
a.Use(middleware.Cost(meter, middleware.Rates{InputPer1M: 3.00, OutputPer1M: 15.00})) // USD per 1M tokens
// ... after running ...
s := meter.Snapshot()
fmt.Printf("spent $%.4f, usage %+v\n", s.SpendUSD, s.Spend)
```

The meter keeps two views, read together with `Snapshot()`. `Answer` and `AnswerUSD` count the
answers, the response each call returned and the run records, once per call wherever `Cost` sits:
inside a `Hedge` a losing target's response is not an answer (`Cost` counts answers through
`agent.ModelCall.OnAnswer`). `Spend` and `SpendUSD` count every
request sent, wherever `Cost` sits: failed attempts a `Retry` repeated and losing `Hedge` targets
are billed too, and under `agent.Replay` so is the discarded spend the original run recorded. The run itself
keeps the same split: `Result.Usage` is the answers, `Result.Spend` everything, and
`WithTokenBudget` stops on everything, including model calls that failed for good. Both cover the
run's agent tree: a sub-agent's model calls count toward its parent's `Result` and budget.

### Writing model middleware

A model middleware is a `func(next agent.ModelHandler) agent.ModelHandler`, where
`agent.ModelHandler` is `func(ctx, agent.ModelCall) (agent.ModelResponse, error)`. The rules that
keep the run's accounting and streaming correct are enforced, not conventions:

- Pass on the call you received, or a copy with fields changed. A `ModelCall` built from scratch
  would drop the hooks outer middleware added, so the agent's model handler refuses it with
  `agent.ErrConfig`.
- `Request.Messages` and `Request.Tools` arrive clipped at every handler, so an `append` allocates:
  two hedged branches that append to the same call never share a backing array.
- Hooks are append-only (`AddHook`). The run's spend meter is not a hook, so a middleware cannot hide
  a request from `WithTokenBudget` or `Result.Spend`.
- A response you build yourself (a cache, a fallback) is checked like a model's, and a streaming
  caller gets it replayed after a `TurnRestarted`.
- Once the agent has the turn's answer, the turn is over: a request of it that reaches the model
  handler later (a call a middleware kept) is refused with `agent.ErrConfig`, since nothing would
  record it.
- To act on the answer the turn records rather than on each response you see (you may sit inside a
  `Hedge` or a `Retry`), register with `call.OnAnswer(key, fn)`: `fn` runs once per turn with the
  answer, however many times your middleware is called for it.

Outside an agent, `agent.CallModel(ctx, model, req, mw...)` sends one call through the same model
handler, hooks included.

## Tool middleware

Tool execution has its own wrappers (attached with `agent.UseTool`):

- `ToolRetry(n, opts...)` retries a tool call with the same backoff/classification options as
  `Retry`, for tools that are retry-safe (`ReadOnly` or `Idempotent`). A tool that is not
  runs once and its error goes to the model as is, since a failed side effect may still have
  taken effect.
- `ToolRateLimit(rl)` caps a tool's call rate (share a `*RateLimiter` to bound a downstream API).
- `ToolCache()` memoizes a `ReadOnly` tool's result for identical arguments within a process.
  Other tools always run: two calls with the same arguments to a side effect are two effects.

- `ToolLog(logf)` logs each tool call and its outcome. A failed call is logged by
  `ErrorSummary(err)` (category, condition, provider status), not its text, which can carry the
  call's arguments; pass `LogErrorText()` to log the text the agent journals for the call (its
  `WithToolErrorRedactor` text, with URL credentials redacted).

Tool middleware receives an `agent.ToolCall`: the model's `ToolUse`, the registered tool's `Spec`,
and the `RunID`. It reads the call's `Spec.Safety` before it retries, caches or skips a call. A
tool's `Timeout` (`agent.WithTimeout`) bounds the whole chain, middleware included, and the agent
does not start a tool whose deadline passed in the middleware.

A middleware reaches the tool only through `next`: never by calling the tool itself, and never by
leaving `next` running after it returns (the agent refuses an invocation of `next` that comes after
the chain returned).

**A denial must wrap `agent.ErrToolNotCalled`.** A middleware that ends a call without calling
`next` (a policy denial, a limiter that gives up) returns an error wrapping
`agent.ErrToolNotCalled`, and only then; `ToolRateLimit` and `ToolRetry` do. The agent needs
positive proof that a side effect was not called: such a call is recorded as a known failure the
model sees, but a chain that returns an error without calling `next`, and without
`ErrToolNotCalled`, leaves a side effect's outcome unknown, records nothing, and the run halts for
it.

"Failed" needs positive proof as well: a middleware that turns a side effect's success into an
error, or returns another error for a call whose tool did not itself fail (a refusal of a retry,
say), makes the outcome unknown, and the run halts rather than tell the model a fired side effect
failed. A retry-safe tool's error is recorded as a failure, except in a saga for a step that
changes state (`Idempotent`, not `ReadOnly`): there it is recorded with an unknown outcome and
listed in `SagaAborted.UnknownOutcome`. A result needs positive proof too: a chain that returns a
result while the tool it began is still running (a middleware that left `next` running and
answered from a cache) has an unknown outcome, since the tool's effect may land after a saga's
compensation; a side effect halts, and a retry-safe saga step is reported as unknown and never
compensated. The agent decides from its own copy of
the spec, so a middleware that changes `call.Spec` changes nothing it enforces. A middleware passes
`next` the `ToolCall` it was given, or a copy with other `Use.Args`: one that changes `Use.Name` or
`Use.ID`, or builds its own `ToolCall`, gets `ErrConfig` and the tool is not called. The agent also enforces
at-most-once below every middleware: a tool that is not retry-safe runs at most once per tool
call, and a middleware that calls it again gets `agent.ErrToolReinvoked` without the tool
running.

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
