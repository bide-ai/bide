package audit_test

import (
	"context"
	"errors"

	"github.com/bide-ai/bide/agent"
)

// fixedHistory is a read-only Durable whose history is exactly the records it holds, salts
// included, standing in for a journal exported from a store (as bide-audit reads one). Copying
// records into another store through Do would give each a fresh salt, and so a different leaf.
type fixedHistory []agent.Record

func (h fixedHistory) History(context.Context, string) ([]agent.Record, error) { return h, nil }

func (fixedHistory) Do(context.Context, string, string, func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return agent.Record{}, errors.New("fixedHistory is read-only")
}
