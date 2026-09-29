package middleware

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
)

// ToolRetry retries a failing tool call up to n additional times with exponential
// backoff + jitter (honoring an *agent.RateLimited RetryAfter), respecting context
// cancellation. Reuses RetryOption/WithBackoff from the model-side Retry.
//
// It retries only a tool that is retry-safe (agent.Safety.RetrySafe: ReadOnly, Idempotent,
// or keyed). A tool that is not runs once, and its error is returned as is: a failed call to a
// side effect may still have taken effect, as when a payment gateway times out after charging,
// and running it again could repeat it.
//
// Default backoff: base=200ms, max=10s. Override with WithBackoff.
//
//	a := agent.New(model, store, tools...).UseTool(middleware.ToolRetry(3))
func ToolRetry(n int, opts ...RetryOption) agent.ToolMiddleware {
	cfg := retryConfig{base: defaultBackoffBase, max: defaultBackoffMax}
	for _, o := range opts {
		o(&cfg)
	}

	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, tu agent.ToolUse) (json.RawMessage, error) {
			if s, ok := agent.ToolSafety(ctx); !ok || !s.RetrySafe() {
				return next(ctx, tu)
			}
			var (
				res json.RawMessage
				err error
			)
			for attempt := 0; attempt <= n; attempt++ {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				err = cfg.run(ctx, func(actx context.Context) error {
					res, err = next(actx, tu)
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
// log.Printf, t.Logf, or a structured logger's Printf-shaped method). Attach with
// agent.Agent.UseTool.
//
//	a := agent.New(model, store, tools...).UseTool(middleware.ToolLog(log.Printf))
//
// A failed call is logged by its ErrorSummary (category, condition, provider status), not its
// text: a tool's error text commonly embeds the call's arguments or a URL with a credential in
// it, and a log line is not the place for them. Pass LogErrorText to log the full text.
func ToolLog(logf func(format string, args ...any), opts ...ToolLogOption) agent.ToolMiddleware {
	var cfg toolLogConfig
	for _, o := range opts {
		o(&cfg)
	}
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, tu agent.ToolUse) (json.RawMessage, error) {
			start := time.Now()
			res, err := next(ctx, tu)
			dur := time.Since(start)
			if err != nil {
				detail := ErrorSummary(err)
				if cfg.errorText {
					detail = err.Error()
				}
				logf("tool %s (%s) error in %s: %s", tu.Name, tu.ID, dur, detail)
			} else {
				logf("tool %s (%s) ok in %s (%d bytes)", tu.Name, tu.ID, dur, len(res))
			}
			return res, err
		}
	}
}

// ToolLogOption configures ToolLog.
type ToolLogOption func(*toolLogConfig)

type toolLogConfig struct{ errorText bool }

// LogErrorText makes ToolLog log a failed call's full error text instead of its ErrorSummary.
// The text can carry content (the call's arguments, a provider's echo of them, a credential in a
// URL), so enable it only where the log may hold that content.
func LogErrorText() ToolLogOption { return func(c *toolLogConfig) { c.errorText = true } }

// ToolCache memoizes successful tool results by (name, args) in a process-local map and
// short-circuits a repeat call — the tool never runs on a hit. It demonstrates the
// short-circuit power of tool middleware.
//
// It caches only tools marked ReadOnly (agent.Safety); every other tool call runs, since two
// calls with the same arguments to a side effect are two effects, such as two separate
// charges. Even for a ReadOnly tool a hit returns the earlier result, so use it only where
// the output depends solely on the arguments. Errors are never cached. This is an
// in-memory, unbounded, per-instance cache — it is NOT the durable journal (which
// already dedupes each tool-use ID at-most-once); it dedupes DISTINCT calls with
// identical arguments within one process.
func ToolCache() agent.ToolMiddleware {
	var cache sync.Map // key string -> json.RawMessage
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, tu agent.ToolUse) (json.RawMessage, error) {
			if s, ok := agent.ToolSafety(ctx); !ok || !s.ReadOnly {
				return next(ctx, tu)
			}
			key := tu.Name + "\x00" + string(tu.Args)
			if v, ok := cache.Load(key); ok {
				return v.(json.RawMessage), nil // hit — skip the tool
			}
			res, err := next(ctx, tu)
			if err == nil {
				cache.Store(key, res)
			}
			return res, err
		}
	}
}
