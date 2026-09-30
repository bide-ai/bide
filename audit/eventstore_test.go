package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
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
	sth := signTH(t, fromStore.TreeHead("run", 1000), priv)
	proof, _ := fromStore.Prove(0)
	evs, _ := agent.ReplayEvents(ctx, jStore, "run") // the disclosed event (held by the verifier)
	incl := audit.VerifyEventInclusion(sth.Root, evs[0], proof)
	if sth.Verify(edV(pub)) != nil || incl != nil {
		t.Fatalf("store-only anchor/proof failed (sth=%v incl=%v)", sth.Verify(edV(pub)), incl)
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
	// Mirror a prefix: the process dies after the first events are appended.
	dying := &dyingStore{EventStore: evStore, left: len(evs) - 1}
	if err := audit.PersistJournal(ctx, dying, jStore, "run"); err == nil {
		t.Fatal("PersistJournal succeeded past the crash")
	}
	early, err := audit.LoadEventLog(ctx, evStore, "run")
	if err != nil || early.Len() != len(evs)-1 {
		t.Fatalf("after the crash the trail holds %d events (%v), want %d", early.Len(), err, len(evs)-1)
	}
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
	if audit.VerifyConsistency(earlyRoot, full.Root(), proof) != nil {
		t.Fatal("store-backed trail failed the append-only consistency proof")
	}
}

// dyingStore passes left appends through to its EventStore, then fails every append, as a
// process that dies mid-mirror.
type dyingStore struct {
	audit.EventStore
	left int
}

func (s *dyingStore) Append(ctx context.Context, runID string, seq int, leaf []byte) error {
	if s.left == 0 {
		return errors.New("process died")
	}
	s.left--
	return s.EventStore.Append(ctx, runID, seq, leaf)
}

// A leaf persists its event's salt: after a restart (a new process that has only the store), the
// reloaded trail has the same root and proves each event under the salt its leaf holds. A retried
// PersistEvent of the same event reuses that salt and is a no-op; a different event is a fork.
func TestEventStore_PersistsSalts(t *testing.T) {
	ctx := context.Background()
	evStore := audit.NewMemEventStore()
	evs := []agent.AgentEvent{agent.TurnStarted{Seq: 0}, agent.ToolCompleted{ToolUseID: "t1", Result: []byte(`true`)}}
	for seq, e := range evs {
		if err := audit.PersistEvent(ctx, evStore, "run", seq, e); err != nil {
			t.Fatalf("PersistEvent %d: %v", seq, err)
		}
	}
	a, err := audit.LoadEventLog(ctx, evStore, "run")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := audit.LoadEventLog(ctx, evStore, "run")
	if !bytes.Equal(a.Root(), b.Root()) {
		t.Fatal("two loads of one trail have different roots")
	}
	p0, _ := b.Prove(0)
	p1, _ := b.Prove(1)
	if len(p0.Salt) != agent.SaltSize || bytes.Equal(p0.Salt, p1.Salt) {
		t.Fatalf("reloaded salts %x, %x; want distinct %d-byte salts", p0.Salt, p1.Salt, agent.SaltSize)
	}
	for i, p := range []audit.EventInclusion{p0, p1} {
		if err := audit.VerifyEventInclusion(a.Root(), evs[i], p); err != nil {
			t.Fatalf("event %d does not verify from the reloaded trail: %v", i, err)
		}
	}

	if err := audit.PersistEvent(ctx, evStore, "run", 1, evs[1]); err != nil {
		t.Fatalf("a retried PersistEvent must be a no-op: %v", err)
	}
	if err := audit.PersistEvent(ctx, evStore, "run", 1, agent.TurnStarted{Seq: 9}); err == nil {
		t.Fatal("a different event at a stored seq was not refused")
	}
	if c, _ := audit.LoadEventLog(ctx, evStore, "run"); !bytes.Equal(c.Root(), a.Root()) {
		t.Fatal("a retried PersistEvent changed the trail")
	}
}

// A trail holding a leaf that is not a salted bide.audit.event-leaf.v2 leaf (an unsalted v1 leaf,
// or a v2 leaf without a full salt) is refused: its proof would have no salt to disclose.
func TestLoadEventLog_RefusesUnsaltedLeaves(t *testing.T) {
	ctx := context.Background()
	for _, leaf := range []string{
		"bide.audit.event-leaf.v1\x00" + `{"kind":"TurnStarted","event":{"Seq":0}}`,
		"bide.audit.event-leaf.v1\x00" + `{"kind":"TurnStarted","event":{"Seq":0},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
		"bide.audit.event-leaf.v2\x00" + `{"kind":"TurnStarted","event":{"Seq":0}}`,
		"bide.audit.event-leaf.v2\x00" + `{"kind":"TurnStarted","event":{"Seq":0},"salt":"AAAA"}`,
	} {
		evStore := audit.NewMemEventStore()
		if err := evStore.Append(ctx, "run", 0, []byte(leaf)); err != nil {
			t.Fatal(err)
		}
		if _, err := audit.LoadEventLog(ctx, evStore, "run"); err == nil {
			t.Errorf("LoadEventLog accepted %q", leaf)
		}
		if err := audit.PersistEvent(ctx, evStore, "run", 0, agent.TurnStarted{Seq: 0}); err == nil || !strings.Contains(err.Error(), "event 0 of run run") {
			t.Errorf("PersistEvent over %q = %v, want an error naming the stored event", leaf, err)
		}
	}
}

// The journal projection refuses a record without an agent.SaltSize salt: the event it projects
// would have no salt to derive.
func TestEventLogFromJournal_RefusesUnsaltedRecords(t *testing.T) {
	ctx := context.Background()
	h := toolResults(2)
	h[1] = withSalt(h[1], nil)
	if _, err := audit.EventLogFromJournal(ctx, h, "run"); err == nil {
		t.Error("EventLogFromJournal projected an unsalted record")
	}
	if err := audit.PersistJournal(ctx, audit.NewMemEventStore(), h, "run"); err == nil {
		t.Error("PersistJournal projected an unsalted record")
	}
}
