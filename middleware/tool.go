package middleware

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	agent "github.com/dayna/go-agents"
)

// ToolRetry retries a failing tool call up to n additional times with exponential
// backoff + jitter (honoring an *agent.RateLimited RetryAfter), respecting context
// cancellation. Reuses RetryOption/WithBackoff from the model-side Retry.
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
			var (
				res json.RawMessage
				err error
			)
			for attempt := 0; attempt <= n; attempt++ {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				res, err = next(ctx, tu)
				if err == nil {
					return res, nil
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
func ToolLog(logf func(format string, args ...any)) agent.ToolMiddleware {
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, tu agent.ToolUse) (json.RawMessage, error) {
			start := time.Now()
			res, err := next(ctx, tu)
			dur := time.Since(start)
			if err != nil {
				logf("tool %s (%s) error in %s: %v", tu.Name, tu.ID, dur, err)
			} else {
				logf("tool %s (%s) ok in %s (%d bytes)", tu.Name, tu.ID, dur, len(res))
			}
			return res, err
		}
	}
}

// ToolCache memoizes successful tool results by (name, args) in a process-local map and
// short-circuits a repeat call — the tool never runs on a hit. It demonstrates the
// short-circuit power of tool middleware.
//
// Use ONLY for pure / read-only tools whose output depends solely on their arguments:
// a cached result skips the real call entirely. Errors are never cached. This is an
// in-memory, unbounded, per-instance cache — it is NOT the durable journal (which
// already dedupes each tool-use ID at-most-once); it dedupes DISTINCT calls with
// identical arguments within one process.
func ToolCache() agent.ToolMiddleware {
	var cache sync.Map // key string -> json.RawMessage
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, tu agent.ToolUse) (json.RawMessage, error) {
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
