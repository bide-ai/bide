package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// twoTurnModel calls a tool on turn 1, then answers on turn 2 — enough to journal a
// StepModel + StepToolResult + StepModel, so the durable projection has real content.
type twoTurnModel struct{ calls int }

func (m *twoTurnModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	m.calls++
	ch := make(chan agent.Emit, 2)
	if m.calls == 1 {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: json.RawMessage(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "final"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// eventTurn is a mock model turn that streams one text answer, so Agent.Stream produces a
// real, ordered AgentEvent sequence (TurnStarted → ModelEvent(TextDelta) → ModelEvent(Finish)
// → AssistantTurn → Finished).
type eventModel struct{ text string }

func (m eventModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: m.text}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	close(ch)
	return agent.NewStream(ch), nil
}

// sampleEvents is a hand-built lifecycle sequence for the pure-log tests.
func sampleEvents() []agent.AgentEvent {
	return []agent.AgentEvent{
		agent.TurnStarted{Seq: 0},
		agent.ModelEvent{Event: agent.TextDelta{Text: "charging"}},
		agent.ToolStarted{ToolUseID: "t1", Name: "charge", Args: []byte(`{"amt":500}`)},
		agent.ToolCompleted{ToolUseID: "t1", Name: "charge", Result: []byte(`"ok"`)},
		agent.AssistantTurn{Message: agent.UserText("done"), Replayed: false},
		agent.Finished{Final: agent.UserText("done")},
	}
}

func buildLog(t *testing.T, evs []agent.AgentEvent) *audit.EventLog {
	t.Helper()
	log := audit.NewEventLog()
	for i, e := range evs {
		if err := log.Add(e); err != nil {
			t.Fatalf("Add event %d: %v", i, err)
		}
	}
	return log
}

// TestEventLog_RootIsDeterministic: same events in the same order → same Root and Head.
func TestEventLog_RootIsDeterministic(t *testing.T) {
	a := buildLog(t, sampleEvents())
	b := buildLog(t, sampleEvents())
	if !bytes.Equal(a.Root(), b.Root()) {
		t.Fatal("Root not deterministic across identical event sequences")
	}
	if !bytes.Equal(a.Head(), b.Head()) {
		t.Fatal("Head not deterministic across identical event sequences")
	}
}

// TestEventLog_OrderMatters: reordering two events changes the commitment (tamper-evidence).
func TestEventLog_OrderMatters(t *testing.T) {
	evs := sampleEvents()
	swapped := append([]agent.AgentEvent{}, evs...)
	swapped[2], swapped[3] = swapped[3], swapped[2] // swap ToolStarted / ToolCompleted
	if bytes.Equal(buildLog(t, evs).Root(), buildLog(t, swapped).Root()) {
		t.Fatal("reordering events did not change the Root — not order-sensitive")
	}
}

// TestEventLog_InclusionProofs: every event proves against Root, and a wrong event does not.
func TestEventLog_InclusionProofs(t *testing.T) {
	evs := sampleEvents()
	log := buildLog(t, evs)
	root := log.Root()

	for i, e := range evs {
		proof, err := log.Prove(i)
		if err != nil {
			t.Fatalf("Prove(%d): %v", i, err)
		}
		ok, err := audit.VerifyEventInclusion(root, e, proof)
		if err != nil {
			t.Fatalf("VerifyEventInclusion(%d): %v", i, err)
		}
		if !ok {
			t.Fatalf("event %d (%T) failed its own inclusion proof", i, e)
		}
		// A tampered event must not verify against the genuine proof.
		if ok, _ := audit.VerifyEventInclusion(root, agent.TurnStarted{Seq: 999}, proof); ok && i != 0 {
			t.Fatalf("a forged event verified at index %d", i)
		}
	}

	if _, err := log.Prove(len(evs)); err == nil {
		t.Fatal("Prove past the end should error")
	}
}

// TestEventLog_SignAnchor: audit.Sign over the event Root round-trips (the anchoring path).
func TestEventLog_SignAnchor(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	root := buildLog(t, sampleEvents()).Root()
	sig := audit.Sign(root, priv)
	if !audit.VerifySignature(root, sig, pub) {
		t.Fatal("signature over event Root did not verify")
	}
	tampered := append([]byte{}, root...)
	tampered[0] ^= 0xff
	if audit.VerifySignature(tampered, sig, pub) {
		t.Fatal("signature verified against a tampered root")
	}
}

// TestEventLog_STH: an STH over the event log signs Root↔Size↔Timestamp, verifies, and
// any change to the bundle invalidates it — the same anchoring the journal STH gives.
func TestEventLog_STH(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	evs := sampleEvents()
	log := buildLog(t, evs)

	sth := audit.SignTreeHead(log.TreeHead("run", 1_700_000_000), priv)
	if !sth.Verify(pub) {
		t.Fatal("event-log STH did not verify")
	}
	if sth.Size != len(evs) {
		t.Fatalf("STH Size = %d, want %d", sth.Size, len(evs))
	}

	// An inclusion proof checks against the SIGNED root — auditor trusts sth, not raw bytes.
	proof, _ := log.Prove(2)
	if ok, _ := audit.VerifyEventInclusion(sth.Root, evs[2], proof); !ok {
		t.Fatal("event proves against its own signed STH root but verification failed")
	}

	// Tamper each bound field: signature must break.
	bad := sth
	bad.Size++
	if bad.Verify(pub) {
		t.Fatal("STH verified after Size tamper")
	}
	bad = sth
	bad.Timestamp++
	if bad.Verify(pub) {
		t.Fatal("STH verified after Timestamp tamper")
	}
	bad = sth
	bad.Root = append([]byte{}, sth.Root...)
	bad.Root[0] ^= 0xff
	if bad.Verify(pub) {
		t.Fatal("STH verified after Root tamper")
	}
}

// TestEventLog_Consistency: the first m events are provably an append-only prefix of the
// later log; a rewrite of an early event breaks the proof.
func TestEventLog_Consistency(t *testing.T) {
	evs := sampleEvents()
	m := 3

	early := buildLog(t, evs[:m])
	rootEarly := early.Root() // commitment when the log held m events

	full := buildLog(t, evs)
	rootFull := full.Root()

	proof, err := full.ProveConsistency(m)
	if err != nil {
		t.Fatalf("ProveConsistency: %v", err)
	}
	if !audit.VerifyConsistency(rootEarly, rootFull, proof) {
		t.Fatal("a genuinely append-only history failed the consistency proof")
	}

	// Rewrite an early event: the earlier root no longer reconciles.
	tampered := append([]agent.AgentEvent{}, evs...)
	tampered[1] = agent.ToolStarted{ToolUseID: "evil", Name: "exfiltrate"}
	rewritten := buildLog(t, tampered[:m]).Root()
	if audit.VerifyConsistency(rewritten, rootFull, proof) {
		t.Fatal("consistency proof accepted a rewritten early event")
	}
}

// TestRecord_DrainsRealStream: Record over a live Agent.Stream yields the terminal answer
// AND a committed log whose every event proves. This is the end-to-end sink.
func TestRecord_DrainsRealStream(t *testing.T) {
	a := agent.New(eventModel{text: "hello"}, agent.NewMemStore())
	stream := a.Stream(context.Background(), "run-1", "hi")

	log := audit.NewEventLog()
	var seen []agent.AgentEvent
	msg, err := audit.Record(log, stream, func(e agent.AgentEvent) { seen = append(seen, e) })
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := textOf(msg); got != "hello" {
		t.Fatalf("final message = %q, want %q", got, "hello")
	}
	if log.Len() == 0 || log.Len() != len(seen) {
		t.Fatalf("log.Len()=%d, forwarded=%d — want equal and non-zero", log.Len(), len(seen))
	}

	root := log.Root()
	for i, e := range seen {
		proof, err := log.Prove(i)
		if err != nil {
			t.Fatalf("Prove(%d): %v", i, err)
		}
		ok, err := audit.VerifyEventInclusion(root, e, proof)
		if err != nil || !ok {
			t.Fatalf("event %d (%T) failed inclusion (ok=%v err=%v)", i, e, ok, err)
		}
	}
	if _, ok := seen[len(seen)-1].(agent.Finished); !ok {
		t.Fatalf("last event = %T, want agent.Finished", seen[len(seen)-1])
	}
}

// TestEventLogFromJournal: the DURABLE projection is deterministic, composes with the STH /
// consistency surface, and its trail is provably append-only — the crash-durable audit
// artifact, built from the persisted journal rather than the ephemeral live stream.
func TestEventLogFromJournal(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	tool := agent.Func("lookup", "", agent.Safety{ReadOnly: true},
		func(_ context.Context, _ struct{}) (string, error) { return "ok", nil })
	if _, err := agent.New(&twoTurnModel{}, store, tool).Run(ctx, "run", "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}

	log1, err := audit.EventLogFromJournal(ctx, store, "run")
	if err != nil {
		t.Fatalf("EventLogFromJournal: %v", err)
	}
	if log1.Len() == 0 {
		t.Fatal("durable projection is empty")
	}
	log2, _ := audit.EventLogFromJournal(ctx, store, "run")
	if !bytes.Equal(log1.Root(), log2.Root()) {
		t.Fatal("durable Root not deterministic across two projections of the same journal")
	}

	// Composes with the STH anchor.
	pub, priv, _ := ed25519.GenerateKey(nil)
	sth := audit.SignTreeHead(log1.TreeHead("run", 1000), priv)
	if !sth.Verify(pub) || sth.Size != log1.Len() {
		t.Fatalf("durable event STH failed (verify=%v size=%d/%d)", sth.Verify(pub), sth.Size, log1.Len())
	}

	// The trail up to any earlier point is an append-only prefix of the full run.
	evs, _ := agent.ReplayEvents(ctx, store, "run")
	if len(evs) < 2 {
		t.Fatalf("need >= 2 projected events, got %d", len(evs))
	}
	prefix := audit.NewEventLog()
	for _, e := range evs[:len(evs)-1] {
		if err := prefix.Add(e); err != nil {
			t.Fatalf("prefix Add: %v", err)
		}
	}
	proof, err := log1.ProveConsistency(prefix.Len())
	if err != nil {
		t.Fatalf("ProveConsistency: %v", err)
	}
	if !audit.VerifyConsistency(prefix.Root(), log1.Root(), proof) {
		t.Fatal("durable event trail failed the append-only consistency proof")
	}
}

// textOf extracts the concatenated text of a message (mirrors Message.Text()).
func textOf(m agent.Message) string { return m.Text() }
