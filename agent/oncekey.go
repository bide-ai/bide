package agent

import (
	"context"
	"fmt"
	"sync/atomic"
)

// onceScope numbers the exactly-once operations one tool call or step makes: prefix names the
// call or step, and next is the number the next operation takes. A tool call or step that runs
// again starts a fresh scope with the same prefix, so it numbers its operations the same way.
type onceScope struct {
	prefix string
	next   *atomic.Int64
}

func withOnceScope(ctx context.Context, prefix string) context.Context {
	return context.WithValue(ctx, onceScopeKey, onceScope{prefix: prefix, next: new(atomic.Int64)})
}

// stepOnceScope returns ctx with the once scope of step name of runID nested in the scope of
// the tool call or step running in ctx, or ctx unchanged outside a tool call.
func stepOnceScope(ctx context.Context, runID, name string) context.Context {
	parent, ok := ctx.Value(onceScopeKey).(onceScope)
	if !ok {
		return ctx
	}
	// Lengths delimit the run and step names, so no two steps share a prefix.
	return withOnceScope(ctx, fmt.Sprintf("%s/step:%d:%s:%d:%s", parent.prefix, len(runID), runID, len(name), name))
}

// NextOnceKey returns a key for the next exactly-once operation (an idempotent append, say) that
// the tool call running in ctx makes, or "" outside a tool call. The call's operations are
// numbered in the order they ask for keys, and the numbering starts again each time the call
// runs, so an operation keeps its key when the call runs again after a crash, and two operations
// of one call never share one.
//
// The keys are scoped to one tool call: they dedupe the call running again (a resume after a crash,
// a retry-safe call a middleware retries), not the model calling the tool again. A retry the model
// makes, after a recorded error or a timeout, is a new call with a new tool-use ID, and gets new
// keys. Dedup across the model's retries needs a business key the tool derives from its arguments
// (an order id, a client request id the model passes), which the downstream dedupes on.
//
// The numbering is stable only if the call asks for its keys in the same order each time it runs.
// Work a call does concurrently must therefore run in steps (Step, or Parallel's tasks): each step
// numbers its own operations, under a key that names the step, so concurrent steps keep their
// keys whatever order they run in.
func NextOnceKey(ctx context.Context) string {
	s, ok := ctx.Value(onceScopeKey).(onceScope)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s#%d", s.prefix, s.next.Add(1)-1)
}
