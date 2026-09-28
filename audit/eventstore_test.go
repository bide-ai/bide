package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/blackwell-systems/bide/agent"
	"github.com/blackwell-systems/bide/audit"
)

// TestMemEventStore_AppendOnly exercises the port contract: contiguous append, idempotent
// re-append of the same bytes, rejection of a different leaf at an existing seq (fork), and
// rejection of a non-contiguous gap.
func TestMemEventStore_AppendOnly(t *testing.T) {
	ctx := context.Background()
	s := audit.NewMemEventStore()

	for i, leaf := range [][]byte{[]byte("a"), []byte("b"), []byte("c")} {
		if err := s.Append(ctx, "run", i, leaf); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// Idempotent replay of an existing seq with identical bytes is a no-op.
	if err := s.Append(ctx, "run", 1, []byte("b")); err != nil {
		t.Fatalf("idempotent re-append should succeed: %v", err)
	}
	// A different leaf at an existing seq is a fork — must error.
	if err := s.Append(ctx, "run", 1, []byte("X")); err == nil {
		t.Fatal("re-appending different bytes at an existing seq must error")
	}
	// A gap (seq beyond the next position) must error.
	if err := s.Append(ctx, "run", 9, []byte("z")); err == nil {
		t.Fatal("non-contiguous append must error")
	}

	got, _ := s.Load(ctx, "run")
	if len(got) != 3 {
		t.Fatalf("Load len = %d, want 3", len(got))
	}
}

// TestPersistJournal_RoundTripsAndIsIdempotent: mirroring the journal into an EventStore
// yields a log whose Root equals the direct journal projection, survives the journal being
// dropped, and re-mirroring changes nothing (idempotent, no fork).
func TestPersistJournal_RoundTripsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	jStore := agent.NewMemStore()
	tool := agent.Func("lookup", "", agent.Safety{ReadOnly: true},
		func(_ context.Context, _ struct{}) (string, error) { return "ok", nil })
	if _, err := agent.New(&twoTurnModel{}, jStore, tool).Run(ctx, "run", "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}

	evStore := audit.NewMemEventStore()
	if err := audit.PersistJournal(ctx, evStore, jStore, "run"); err != nil {
		t.Fatalf("PersistJournal: %v", err)
	}

	// The store-backed log matches the direct journal projection.
	fromStore, err := audit.LoadEventLog(ctx, evStore, "run")
	if err != nil {
		t.Fatalf("LoadEventLog: %v", err)
	}
	fromJournal, _ := audit.EventLogFromJournal(ctx, jStore, "run")
	if fromStore.Len() == 0 || !bytes.Equal(fromStore.Root(), fromJournal.Root()) {
		t.Fatalf("store-backed Root != journal projection Root (len=%d)", fromStore.Len())
	}

	// Idempotent: re-mirroring appends nothing new and does not fork.
	if err := audit.PersistJournal(ctx, evStore, jStore, "run"); err != nil {
		t.Fatalf("second PersistJournal must be a no-op: %v", err)
	}
	reload, _ := audit.LoadEventLog(ctx, evStore, "run")
	if !bytes.Equal(reload.Root(), fromStore.Root()) {
		t.Fatal("re-mirroring changed the trail")
	}

	// The trail stands on its own: anchor and prove from the EventStore alone (journal gone).
	pub, priv, _ := ed25519.GenerateKey(nil)
	sth := audit.SignTreeHead(fromStore.TreeHead(1000), priv)
	proof, _ := fromStore.Prove(0)
	evs, _ := agent.ReplayEvents(ctx, jStore, "run") // the disclosed event (held by the verifier)
	ok, _ := audit.VerifyEventInclusion(sth.Root, evs[0], proof)
	if !sth.Verify(pub) || !ok {
		t.Fatalf("store-only anchor/proof failed (sth=%v incl=%v)", sth.Verify(pub), ok)
	}
}

// TestPersistJournal_IncrementalConsistency: mirroring a prefix then the full run leaves the
// earlier trail a provable append-only prefix of the later one — the crash/resume shape, but
// via the separate store lifecycle.
func TestPersistJournal_IncrementalConsistency(t *testing.T) {
	ctx := context.Background()
	jStore := agent.NewMemStore()
	tool := agent.Func("lookup", "", agent.Safety{ReadOnly: true},
		func(_ context.Context, _ struct{}) (string, error) { return "ok", nil })
	if _, err := agent.New(&twoTurnModel{}, jStore, tool).Run(ctx, "run", "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}
	evs, _ := agent.ReplayEvents(ctx, jStore, "run")
	if len(evs) < 2 {
		t.Fatalf("need >= 2 events, got %d", len(evs))
	}

	evStore := audit.NewMemEventStore()
	// Mirror a prefix (as if the process died after the first events).
	for seq, e := range evs[:len(evs)-1] {
		if err := audit.PersistEvent(ctx, evStore, "run", seq, e); err != nil {
			t.Fatalf("PersistEvent %d: %v", seq, err)
		}
	}
	early, _ := audit.LoadEventLog(ctx, evStore, "run")
	earlyRoot := early.Root()

	// Now mirror the whole journal; the missing tail is appended, prefix untouched.
	if err := audit.PersistJournal(ctx, evStore, jStore, "run"); err != nil {
		t.Fatalf("PersistJournal: %v", err)
	}
	full, _ := audit.LoadEventLog(ctx, evStore, "run")

	proof, err := full.ProveConsistency(early.Len())
	if err != nil {
		t.Fatalf("ProveConsistency: %v", err)
	}
	if !audit.VerifyConsistency(earlyRoot, full.Root(), proof) {
		t.Fatal("store-backed trail failed the append-only consistency proof")
	}
}
