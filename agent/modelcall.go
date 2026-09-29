package agent

import "context"

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

// inModelCall marks ctx as an agent model call's context, with no hooks yet.
func inModelCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, modelHooksKey, []ModelCallHook{})
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
