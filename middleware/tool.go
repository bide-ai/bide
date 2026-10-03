package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
)

// ToolRetry retries a failing tool call up to n additional times with exponential
// backoff + jitter (honoring an *agent.RateLimited RetryAfter), respecting context
// cancellation. Reuses RetryOption/WithBackoff from the model-side Retry.
//
// It retries only a tool that is retry-safe (the call's Spec.Safety is RetrySafe: ReadOnly or
// Idempotent). A tool that is not runs once, and its error is returned as is: a failed call to a
// side effect may still have taken effect, as when a payment gateway times out after charging,
// and running it again could repeat it.
//
// Default backoff: base=200ms, max=10s. Override with WithBackoff.
//
// n must be at least 0 (0 runs the tool once). With n < 0 every call fails with an error wrapping
// agent.ErrConfig without running the tool, whatever its safety.
//
//	a, err := agent.New(model, journal, agent.WithTools(tools...), agent.WithToolMiddleware(middleware.ToolRetry(3)))
func ToolRetry(n int, opts ...RetryOption) agent.ToolMiddleware {
	cfg := retryConfig{base: defaultBackoffBase, max: defaultBackoffMax}
	for _, o := range opts {
		o(&cfg)
	}

	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, call agent.ToolCall) (json.RawMessage, error) {
			if err := checkRetryCount("ToolRetry", n); err != nil {
				return nil, fmt.Errorf("%w (%w)", err, agent.ErrToolNotCalled) // next is never called
			}
			if !call.Spec.Safety.RetrySafe() {
				return next(ctx, call)
			}
			var (
				res json.RawMessage
				err error
			)
			for attempt := 0; attempt <= n; attempt++ {
				if ctx.Err() != nil {
					if attempt == 0 { // next was never called
						return nil, fmt.Errorf("%w (%w)", ctx.Err(), agent.ErrToolNotCalled)
					}
					return nil, ctx.Err()
				}
				err = cfg.run(ctx, func(actx context.Context) error {
					res, err = next(actx, call)
					return err
				})
				if err == nil {
					return res, nil
				}
				if cfg.retryIf != nil && !cfg.retryIf(err) {
					return res, err
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if attempt == n {
					break
				}

				// Compute how long to sleep before the next attempt.
				d := sleepDuration(err, attempt, cfg)
				if d > 0 {
					select {
					case <-time.After(d):
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			}
			return res, err
		}
	}
}

// ToolLog logs one line per tool call — name, duration, and ok/error — via logf (pass
// log.Printf, t.Logf, or a structured logger's Printf-shaped method). Attach with the
// agent.WithToolMiddleware option.
//
//	a, err := agent.New(model, journal, agent.WithTools(tools...), agent.WithToolMiddleware(middleware.ToolLog(log.Printf)))
//
// A failed call is logged by its ErrorSummary (category, condition, provider status), not its
// text: a tool's error text commonly embeds the call's arguments or a URL with a credential in
// it, and a log line is not the place for them. Pass LogErrorText to log the text the agent
// journals for the call instead (see agent.ToolCall.ErrorText).
func ToolLog(logf func(format string, args ...any), opts ...ToolLogOption) agent.ToolMiddleware {
	var cfg toolLogConfig
	for _, o := range opts {
		o(&cfg)
	}
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, call agent.ToolCall) (json.RawMessage, error) {
			start := time.Now()
			res, err := next(ctx, call)
			dur := time.Since(start)
			if err != nil {
				detail := ErrorSummary(err)
				if cfg.errorText {
					detail = call.ErrorText(err)
				}
				logf("tool %s (%s) error in %s: %s", call.Use.Name, call.Use.ID, dur, detail)
			} else {
				logf("tool %s (%s) ok in %s (%d bytes)", call.Use.Name, call.Use.ID, dur, len(res))
			}
			return res, err
		}
	}
}

// ToolLogOption configures ToolLog.
type ToolLogOption func(*toolLogConfig)

type toolLogConfig struct{ errorText bool }

// LogErrorText makes ToolLog log a failed call's error text instead of its ErrorSummary: the text
// the agent journals for the call and sends to the model (agent.ToolCall.ErrorText), which is the
// agent's WithToolErrorRedactor text if one is set, with every URL in it redacted. It can still
// carry content (the call's arguments, a provider's echo of them), so enable it only where the
// log may hold that content.
func LogErrorText() ToolLogOption { return func(c *toolLogConfig) { c.errorText = true } }

// ToolCache memoizes successful tool results by (name, args) in a process-local map and
// short-circuits a repeat call — the tool never runs on a hit. It demonstrates the
// short-circuit power of tool middleware.
//
// It caches only tools marked ReadOnly (the call's Spec.Safety); every other tool call runs, since two
// calls with the same arguments to a side effect are two effects, such as two separate
// charges. Even for a ReadOnly tool a hit returns the earlier result, so use it only where
// the output depends solely on the arguments. Errors are never cached. This is an
// in-memory, unbounded, per-instance cache — it is NOT the durable journal (which
// already dedupes each tool-use ID at-most-once); it dedupes DISTINCT calls with
// identical arguments within one process.
func ToolCache() agent.ToolMiddleware {
	var cache sync.Map // key string -> json.RawMessage
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, call agent.ToolCall) (json.RawMessage, error) {
			if !call.Spec.Safety.ReadOnly {
				return next(ctx, call)
			}
			key := call.Use.Name + "\x00" + string(call.Use.Args)
			if v, ok := cache.Load(key); ok {
				return v.(json.RawMessage), nil // hit — skip the tool
			}
			res, err := next(ctx, call)
			if err == nil {
				cache.Store(key, res)
			}
			return res, err
		}
	}
}
