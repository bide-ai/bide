package postgres

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	agent "github.com/dayna/go-agents"
)

// Set PG_DSN (e.g. postgres://postgres:postgres@localhost:5432/agents?sslmode=disable)
// to run the integration test; it skips otherwise so the suite stays green offline.
func TestPostgres_DoMemoizesAndHistory(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	ctx := context.Background()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	runID := "pg-test-" + t.Name()
	var runs int
	mk := func(context.Context) (agent.Record, error) {
		runs++
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
	}
	if _, err := store.Do(ctx, runID, "step", mk); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Do(ctx, runID, "step", mk); err != nil { // memoized
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("fn ran %d times, want 1 (not memoized)", runs)
	}
	h, err := store.History(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 || h[0].Name != "step" {
		t.Fatalf("history = %+v", h)
	}
}
