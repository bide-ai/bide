package agent

import "context"

type ctxKey int

const (
	runScopeKey   ctxKey = 0
	sagaKey       ctxKey = 1
	runContextKey ctxKey = 3
	onceScopeKey  ctxKey = 4
)

// runCtx carries the store + runID into a tool's context so Interrupt can journal and
// read its resume value without the tool holding those handles.
type runCtx struct {
	store Durable
	runID string
	root  string // the top-level run; a sub-agent's runs inherit it
}

func withRunContext(ctx context.Context, store Durable, runID string) context.Context {
	return context.WithValue(ctx, runContextKey, runCtx{store: store, runID: runID, root: rootRunID(ctx, runID)})
}

// rootRunID is the top-level run for a run with this ID reached through ctx: the root recorded
// by an enclosing run (a sub-agent is called from its parent's tool context), or runID itself.
func rootRunID(ctx context.Context, runID string) string {
	if rc, ok := ctx.Value(runContextKey).(runCtx); ok && rc.root != "" {
		return rc.root
	}
	return runID
}

func runContext(ctx context.Context) (Durable, string, bool) {
	rc, ok := ctx.Value(runContextKey).(runCtx)
	return rc.store, rc.runID, ok
}

func withRunScope(ctx context.Context, scope string) context.Context {
	return withOnceScope(context.WithValue(ctx, runScopeKey, scope), scope)
}

// RunScope returns the hierarchical run scope for the current tool execution
// (SubRunID: the parent run ID, '>', the encoded tool-use ID). Sub-agents use it to derive a stable, resumable sub-run ID.
func RunScope(ctx context.Context) string {
	s, _ := ctx.Value(runScopeKey).(string)
	return s
}

func withSaga(ctx context.Context) context.Context { return context.WithValue(ctx, sagaKey, true) }

// InSaga reports whether the current tool execution is inside a saga run. Sub-agents use
// it to run transactionally (RunSaga) so a failure deep in the tree aborts the whole tree.
func InSaga(ctx context.Context) bool {
	v, _ := ctx.Value(sagaKey).(bool)
	return v
}
