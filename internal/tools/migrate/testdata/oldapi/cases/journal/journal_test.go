package journal

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// A store used only as a journal is retyped.
func TestRetyped(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	if err := agent.Approve(ctx, store, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	recs, _ := store.History(ctx, "r")
	_ = recs
}

// A store also used as a store gets a journal beside it.
func TestBeside(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	_, _, _ = store.Insert(ctx, "r", "k", nil)
	_ = agent.Approve(ctx, store, "r", "c1", true)
	_ = useJournal(store)
	_ = store.Journal()
}

// A parameter typed Durable becomes a *Journal; an inline store becomes a MemJournal.
func useJournal(d agent.Durable) error {
	return agent.Approve(context.Background(), d, "r", "c", false)
}

func TestInline(t *testing.T) {
	_ = useJournal(agent.NewMemStore())
}

// A raw write in bide's own tests goes through journaltest.
func TestRawWrite(t *testing.T) {
	ctx := context.Background()
	j, _ := agent.NewJournal(agent.NewMemStore())
	_, _ = j.Do(ctx, "r", "k", func(context.Context) (agent.Record, error) { return agent.Record{}, nil })
}

// A store variable assigned again: each use is a journal over the value it has then, not over
// the first value only.
func TestReassigned(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	_ = agent.Approve(ctx, store, "r", "c1", true)
	store = agent.NewMemStore()
	_ = agent.Approve(ctx, store, "r", "c2", true)
}

// An audited store wraps the store beneath a journal.
func TestAuditedOverJournal(t *testing.T) {
	var d agent.Durable = agent.NewMemStore()
	as, err := audit.NewAuditedStore(d)
	_, _ = as, err
}
