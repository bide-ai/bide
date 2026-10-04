// Package journaltest gives bide's own tests what a journal keeps unexported: a raw record write.
// A test that builds a journal in a given state (a marker with no result, a record of another
// version) writes it with Do. It is internal: an application's code writes records only through
// the engine (runs, Step, the verbs).
package journaltest

import (
	"context"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journalhook"
)

// Do runs fn as the named step name of runID in j, at most once, and returns the record the
// journal holds (see the journal's do): if the step is recorded, fn is not called.
func Do(ctx context.Context, j *agent.Journal, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	v, err := journalhook.Do(ctx, j, runID, name, func(ctx context.Context) (any, error) { return fn(ctx) })
	rec, _ := v.(agent.Record)
	return rec, err
}

// DoFresh is Do through the path the engine's live steps take (the journal's doFresh), which
// does not read the step first: for a step known not to be recorded.
func DoFresh(ctx context.Context, j *agent.Journal, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	v, err := journalhook.DoFresh(ctx, j, runID, name, func(ctx context.Context) (any, error) { return fn(ctx) })
	rec, _ := v.(agent.Record)
	return rec, err
}

// Put records rec as the step name of runID in j unless the step is recorded, and returns the
// record the journal holds.
func Put(ctx context.Context, j *agent.Journal, runID, name string, rec agent.Record) (agent.Record, error) {
	return Do(ctx, j, runID, name, func(context.Context) (agent.Record, error) { return rec, nil })
}
