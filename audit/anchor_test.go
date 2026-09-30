package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// mkSTH builds a distinct signed tree head for anchor-log tests (the anchor log doesn't care
// what each STH commits to, only that entries differ and are append-only).
func mkSTH(t *testing.T, priv ed25519.PrivateKey, size int) audit.SignedTreeHead {
	t.Helper()
	th := audit.TreeHead{Kind: audit.TreeJournal, RunID: "run", Size: size, Root: bytes.Repeat([]byte{byte(size)}, 32), Timestamp: int64(size)}
	return audit.SignTreeHead(th, priv)
}

// TestMemAnchorLog_InclusionAndConsistency: the transparency log is deterministic, proves any
// entry's inclusion (rejecting forgeries), and proves it only grew.
func TestMemAnchorLog_InclusionAndConsistency(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(nil)
	log := audit.NewMemAnchorLog()

	// Publish two, snapshot the root, publish two more.
	for i := 1; i <= 2; i++ {
		if err := log.Publish(ctx, "run", mkSTH(t, priv, i)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	rootEarly, _ := log.Root()
	for i := 3; i <= 4; i++ {
		if err := log.Publish(ctx, "run", mkSTH(t, priv, i)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	rootFull, _ := log.Root()

	// Every entry proves inclusion against the current root; a forged entry does not.
	entries := log.Entries()
	for _, e := range entries {
		proof, err := log.Prove(e.Seq)
		if err != nil {
			t.Fatalf("Prove(%d): %v", e.Seq, err)
		}
		if ok, _ := audit.VerifyAnchorInclusion(rootFull, e, proof); !ok {
			t.Fatalf("entry %d failed its own inclusion proof", e.Seq)
		}
	}
	proof0, _ := log.Prove(0)
	forged := entries[0]
	forged.RunID = "evil"
	if ok, _ := audit.VerifyAnchorInclusion(rootFull, forged, proof0); ok {
		t.Fatal("a forged anchor entry verified")
	}

	// The anchor log only grew: the first two entries are an append-only prefix.
	cproof, err := log.ProveConsistency(2)
	if err != nil {
		t.Fatalf("ProveConsistency: %v", err)
	}
	if !audit.VerifyConsistency(rootEarly, rootFull, cproof) {
		t.Fatal("anchor log failed its own append-only consistency proof")
	}
}

// TestAuditedStore_AnchorsEachStep is the end-to-end: running an agent through the audited
// store publishes one signed tree head per durable step, the last commits the whole journal,
// and that STH is provably present in the external transparency log.
func TestAuditedStore_AnchorsEachStep(t *testing.T) {
	ctx := context.Background()
	jStore := agent.NewMemStore()
	pub, priv, _ := ed25519.GenerateKey(nil)
	anchorLog := audit.NewMemAnchorLog()

	var ts int64
	store := audit.NewAuditedStore(jStore, priv, anchorLog).WithClock(func() int64 { ts++; return ts })
	tool := agent.Func("lookup", "", agent.Safety{ReadOnly: true},
		func(_ context.Context, _ struct{}) (string, error) { return "ok", nil })
	if _, err := agent.New(&twoTurnModel{}, store, tool).Run(ctx, "run", "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}

	recs, _ := jStore.History(ctx, "run")
	entries := anchorLog.Entries()
	// One anchored STH per journal growth: each step grows the journal by one, and the first
	// also writes the journal header.
	if len(entries) != len(recs)-1 || len(entries) == 0 {
		t.Fatalf("anchored %d entries, want %d (one per step)", len(entries), len(recs)-1)
	}
	// Sizes are monotonically increasing.
	for i := 1; i < len(entries); i++ {
		if entries[i].STH.Size <= entries[i-1].STH.Size {
			t.Fatalf("STH sizes not monotonic at %d: %d then %d", i, entries[i-1].STH.Size, entries[i].STH.Size)
		}
	}

	// The final STH signs the complete journal and verifies.
	last := entries[len(entries)-1]
	if last.STH.Size != len(recs) || !last.STH.Verify(pub) {
		t.Fatalf("final STH bad (size=%d/%d verify=%v)", last.STH.Size, len(recs), last.STH.Verify(pub))
	}
	jRoot, _ := audit.Root(ctx, jStore, "run")
	if !bytes.Equal(last.STH.Root, jRoot) {
		t.Fatal("final anchored root != journal Merkle root")
	}

	// End-to-end chain: the final commitment is provably anchored in the transparency log.
	anchorRoot, _ := anchorLog.Root()
	incl, _ := anchorLog.Prove(last.Seq)
	if ok, _ := audit.VerifyAnchorInclusion(anchorRoot, last, incl); !ok {
		t.Fatal("final STH not provably anchored in the transparency log")
	}
}

// crashyModel calls the tool on turn 1, then errors on turn 2 (a crash mid-run).
type crashyModel struct{ n int }

func (m *crashyModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	m.n++
	ch := make(chan agent.Emit, 2)
	if m.n == 1 {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: []byte(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	} else {
		ch <- agent.Emit{Err: errors.New("crash")}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// finalModel answers directly (used to heal the crashed run on resume).
type finalModel struct{}

func (finalModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: "final"}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	close(ch)
	return agent.NewStream(ch), nil
}

// TestAuditedStore_NoReanchorOnResume: a crash anchors the steps that committed; resuming
// anchors only the NEW step, never re-anchoring replayed ones. Each durable record is anchored
// exactly once, so total anchors == total records even across a crash+resume.
func TestAuditedStore_NoReanchorOnResume(t *testing.T) {
	ctx := context.Background()
	jStore := agent.NewMemStore()
	_, priv, _ := ed25519.GenerateKey(nil)
	anchorLog := audit.NewMemAnchorLog()
	var ts int64
	store := audit.NewAuditedStore(jStore, priv, anchorLog).WithClock(func() int64 { ts++; return ts })
	tool := agent.Func("lookup", "", agent.Safety{ReadOnly: true},
		func(_ context.Context, _ struct{}) (string, error) { return "ok", nil })

	// Crash mid-run: the tool call + result commit, the second model turn dies.
	if _, err := agent.New(&crashyModel{}, store, tool).Run(ctx, "run", "hi"); err == nil {
		t.Fatal("expected the injected crash to fail the run")
	}
	crashRecs, _ := jStore.History(ctx, "run")
	// One per record, the journal header aside: it lands with the first step.
	if anchorLog.Len() != len(crashRecs)-1 {
		t.Fatalf("post-crash anchored %d, want one per record (%d)", anchorLog.Len(), len(crashRecs)-1)
	}

	// Resume: replayed steps must NOT re-anchor; only the final turn adds one.
	if _, err := agent.New(finalModel{}, store, tool).Run(ctx, "run", "hi"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	finalRecs, _ := jStore.History(ctx, "run")
	if anchorLog.Len() != len(finalRecs)-1 {
		t.Fatalf("anchors=%d != records=%d — replayed steps were re-anchored", anchorLog.Len(), len(finalRecs))
	}
	if len(finalRecs) <= len(crashRecs) {
		t.Fatalf("resume did not grow the journal: %d -> %d", len(crashRecs), len(finalRecs))
	}
}

// errAnchor always fails Publish, to prove anchoring is a non-fatal side channel.
type errAnchor struct{ calls *int }

func (e errAnchor) Publish(context.Context, string, audit.SignedTreeHead) error {
	*e.calls++
	return errors.New("anchor unreachable")
}

// TestAuditedStore_PublishErrorDoesNotFailStep: a failed anchor must NOT fail the durable
// step — the journal is the source of truth and a non-idempotent step must not be retried.
func TestAuditedStore_PublishErrorDoesNotFailStep(t *testing.T) {
	ctx := context.Background()
	jStore := agent.NewMemStore()
	_, priv, _ := ed25519.GenerateKey(nil)
	var pubCalls, errCalls int
	store := audit.NewAuditedStore(jStore, priv, errAnchor{&pubCalls}).
		OnError(func(string, error) { errCalls++ })
	tool := agent.Func("lookup", "", agent.Safety{ReadOnly: true},
		func(_ context.Context, _ struct{}) (string, error) { return "ok", nil })

	out, err := agent.New(&twoTurnModel{}, store, tool).Run(ctx, "run", "hi")
	if err != nil {
		t.Fatalf("run must succeed despite anchor failures: %v", err)
	}
	if textOf(out) != "final" {
		t.Fatalf("answer = %q, want %q", textOf(out), "final")
	}
	if pubCalls == 0 || errCalls == 0 {
		t.Fatalf("expected publish attempts (%d) and OnError calls (%d)", pubCalls, errCalls)
	}
}
