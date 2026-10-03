// Package journaltest is a stub of bide's internal journaltest for the migrate tool's tests.
package journaltest

import (
	"context"

	"github.com/bide-ai/bide/agent"
)

func Do(ctx context.Context, j *agent.Journal, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	panic("stub")
}
