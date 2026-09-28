package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A redelivered signal dedups ACROSS PROCESSES on the on-disk store: the second delivery,
// from a freshly reopened store (a different process), is a no-op via the primary key on
// (run, name), so the signal is applied at most once and the first payload wins. This is the
// durable backing for "at-least-once transport in, exactly-once application"; the in-memory
// store only approximates it with singleflight, so this test covers the cross-process claim.
func TestSQLite_SignalRedeliveryDedupsAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	ctx := context.Background()
	runID := "r-sig"

	// Process 1 delivers the signal, then exits.
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Signal(ctx, s1, runID, "webhook", "first"); err != nil {
		t.Fatalf("Signal (process 1): %v", err)
	}
	s1.Close()

	// Process 2 reopens the same file and redelivers the SAME signal (at-least-once transport).
	// It must be a no-op: the on-disk record from process 1 already exists.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := agent.Signal(ctx, s2, runID, "webhook", "second"); err != nil {
		t.Fatalf("Signal (process 2): %v", err)
	}

	recs, err := s2.History(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	var payload string
	for _, r := range recs {
		if r.Kind == agent.StepSignal && r.Name == "signal:webhook" {
			n++
			_ = json.Unmarshal(r.Result, &payload)
		}
	}
	if n != 1 {
		t.Fatalf("signal recorded %d times across processes, want 1 (primary-key dedup)", n)
	}
	if payload != "first" {
		t.Fatalf("payload = %q, want first (first delivery wins)", payload)
	}
}
