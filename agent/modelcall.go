package agent

import (
	"context"
	"fmt"
	"sync"
)

// ModelCallHook runs around each request the agent's model handler sends to a model: every
// retried attempt and every hedged target, wherever the middleware that installed it sits in the
// chain. Middleware whose effect belongs to requests actually sent, rather than to calls through
// it, uses one: RateLimit waits for a token in Before, Cost counts spend in After.
type ModelCallHook struct {
	// Before runs before the request is sent, with the request's context. An error fails the
	// call without sending it.
	Before func(ctx context.Context) error
	// After runs once a sent request has ended, successfully or not, with the usage it reported:
	// for a failed request, the part reported before it failed.
	After func(u Usage)
}

// WithModelCallHook returns ctx with h added to the hooks run around each model request made
// under it. Hooks run in the order they were added, so a middleware nearer the outside of the
// chain runs its Before first. ok is false, and ctx is returned unchanged, when ctx is not the
// context of an agent model call (a handler called directly, outside Agent), where nothing runs
// hooks: the middleware must then apply its effect itself.
func WithModelCallHook(ctx context.Context, h ModelCallHook) (_ context.Context, ok bool) {
	hooks, ok := ctx.Value(modelHooksKey).([]ModelCallHook)
	if !ok {
		return ctx, false
	}
	return context.WithValue(ctx, modelHooksKey, append(hooks[:len(hooks):len(hooks)], h)), true
}

// WithModel returns ctx under which the agent's model handler sends requests to m instead of
// the agent's model, so a middleware can send a call to another model through the rest of the
// chain (Hedge sends its backups this way). ok is false, and ctx is returned unchanged, when ctx
// is not the context of an agent model call.
func WithModel(ctx context.Context, m Model) (_ context.Context, ok bool) {
	if _, ok := ctx.Value(modelHooksKey).([]ModelCallHook); !ok {
		return ctx, false
	}
	return context.WithValue(ctx, modelOverrideKey, m), true
}

// inModelCall marks ctx as an agent model call's context. Its first hook adds every request's
// usage to meter.
func inModelCall(ctx context.Context, meter *spendMeter) context.Context {
	return context.WithValue(ctx, modelHooksKey, []ModelCallHook{{After: meter.add}})
}

// modelFor returns the model a request under ctx goes to: the one set by WithModel, else def.
func modelFor(ctx context.Context, def Model) Model {
	if m, ok := ctx.Value(modelOverrideKey).(Model); ok && m != nil {
		return m
	}
	return def
}

func modelHooks(ctx context.Context) []ModelCallHook {
	hooks, _ := ctx.Value(modelHooksKey).([]ModelCallHook)
	return hooks
}

// usageTotals is what a run's live model turns used: answer is the usage of the responses the
// run recorded, spend every request's, including requests whose responses were discarded (failed
// attempts, losing hedge targets) and model calls that failed.
type usageTotals struct {
	answer, spend Usage
}

// spendMeter collects the usage of the model requests a run sends until the run takes it to
// record. A request that ends after its turn was recorded (a hedge loser that outlived the race)
// is taken with the next turn.
type spendMeter struct {
	mu      sync.Mutex
	pending Usage
}

func (m *spendMeter) add(u Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	addUsage(&m.pending, u)
}

func (m *spendMeter) take() Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.pending
	m.pending = Usage{}
	return u
}

// discardedSpend is the part of spent beyond answer, the usage of the response a turn recorded,
// per field and never below zero: a response a middleware supplied without sending a request
// (a cache) may report usage no request spent.
func discardedSpend(spent, answer Usage) Usage {
	sub := func(a, b int) int { return max(a-b, 0) }
	return Usage{
		InputTokens:      sub(spent.InputTokens, answer.InputTokens),
		OutputTokens:     sub(spent.OutputTokens, answer.OutputTokens),
		CacheReadTokens:  sub(spent.CacheReadTokens, answer.CacheReadTokens),
		CacheWriteTokens: sub(spent.CacheWriteTokens, answer.CacheWriteTokens),
	}
}

// spendStepPrefix names the records that journal a failed model call's spend: "@spend/0",
// "@spend/1", and so on, numbered in the order the run wrote them.
const spendStepPrefix = "@spend/"

// recordSpend journals spent, the usage of a model call that failed, as the run's n-th spend
// record: a StepValue carrying it as DiscardedUsage. It is written even when ctx is cancelled,
// the common way a call fails, since the requests were billed either way.
func (a *Agent) recordSpend(ctx context.Context, runID string, n int, spent Usage) error {
	_, err := a.store.Do(context.WithoutCancel(ctx), runID, fmt.Sprintf("%s%d", spendStepPrefix, n), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, DiscardedUsage: &spent}, nil
	})
	if err != nil {
		return fmt.Errorf("record spend (run %s): %w (%w)", runID, err, ErrStorage)
	}
	return nil
}
