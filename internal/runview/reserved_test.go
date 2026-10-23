package runview

import (
	"context"
	"slices"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// The viewer lists no run the engine reserves for its own records: neither the kill switch's
// control run nor a cap ledger run, whether or not it holds an end marker.
func TestJournalSource_ListsNoReservedRun(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	j, err := agent.NewJournal(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"r1", "r2", agent.StopControlRun, "@cap/ns/refunds/usd/d2026-10-22/0123abcd", "@cap/ns/refunds/usd/index"} {
		if _, _, err := store.Insert(ctx, run, "run:start", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	src := JournalSource{J: j}
	want := []string{"r1", "r2"}
	got, err := src.Runs(ctx)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("Runs = %v, %v; want %v", got, err, want)
	}
	got, err = src.Unfinished(ctx)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("Unfinished = %v, %v; want %v", got, err, want)
	}
	// The limit counts the runs listed, not the ones skipped.
	got, err = src.UnfinishedLimit(ctx, 2)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("UnfinishedLimit(2) = %v, %v; want %v", got, err, want)
	}
}
