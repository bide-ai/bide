package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
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
func sampleEvents() []agent.RunEvent {
	return []agent.RunEvent{
		agent.TurnStarted{Seq: 0},
		agent.ModelEvent{Event: agent.TextDelta{Text: "charging"}},
		agent.ToolStarted{ToolUseID: "t1", Name: "charge", Args: []byte(`{"amt":500}`)},
		agent.ToolCompleted{ToolUseID: "t1", Name: "charge", Result: []byte(`"ok"`)},
		agent.AssistantTurn{Message: agent.UserText("done"), Replayed: false},
		agent.Finished{Final: agent.UserText("done")},
	}
}

func buildLog(t *testing.T, evs []agent.RunEvent) *audit.EventLog {
	t.Helper()
	log := audit.NewEventLog()
	for i, e := range evs {
		if err := log.Add(e); err != nil {
			t.Fatalf("Add event %d: %v", i, err)
		}
	}
	return log
}

// history is a fixedHistory: exactly the records it holds, salts included,
// as a journal exported from a store is.
type history = fixedHistory

// toolResults is a journal of n tool results, each with its own fixed salt.
func toolResults(n int) history {
	h := make(history, n)
	for i := range h {
		id := fmt.Sprintf("t%d", i)
		h[i] = withSalt(agent.Record{Name: id, Kind: agent.StepToolResult, ToolUseID: id, Result: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))}, bytes.Repeat([]byte{byte(i + 1)}, agent.SaltSize))
	}
	return h
}

func projectLog(t *testing.T, h history) *audit.EventLog {
	t.Helper()
	log, err := audit.EventLogFromJournal(context.Background(), h.journal(), "run")
	if err != nil {
		t.Fatalf("EventLogFromJournal: %v", err)
	}
	return log
}

// TestEventLog_SaltedPerEvent: each added event commits to a fresh random salt, so two logs of the
// same events have different roots, and each proof discloses its own event's salt: the event
// verifies under it, and not under another salt or without one.
func TestEventLog_SaltedPerEvent(t *testing.T) {
	a := buildLog(t, sampleEvents())
	b := buildLog(t, sampleEvents())
	if bytes.Equal(a.Root(), b.Root()) || bytes.Equal(a.Head(), b.Head()) {
		t.Fatal("two logs of the same events share a commitment: their leaves are not salted")
	}
	p0, _ := a.Prove(0)
	p1, _ := a.Prove(1)
	if len(p0.Salt) != agent.SaltSize || bytes.Equal(p0.Salt, p1.Salt) {
		t.Fatalf("event salts %x and %x, want distinct %d-byte salts", p0.Salt, p1.Salt, agent.SaltSize)
	}
	evs := sampleEvents()
	if err := audit.VerifyEventInclusion(a.Root(), evs[0], p0); err != nil {
		t.Fatalf("event 0 does not verify under its proof's salt: %v", err)
	}
	other := p0
	other.Salt = p1.Salt
	if err := audit.VerifyEventInclusion(a.Root(), evs[0], other); err == nil {
		t.Fatal("event 0 verified under another event's salt")
	}
	for _, salt := range [][]byte{nil, p0.Salt[:agent.SaltSize-1]} {
		bad := p0
		bad.Salt = salt
		if err := audit.VerifyEventInclusion(a.Root(), evs[0], bad); err == nil {
			t.Fatalf("a proof with a %d-byte salt = %v; want an error", len(salt), err)
		}
	}
}

// TestEventLog_OrderMatters: reordering two events changes the commitment (tamper-evidence), even
// when each event keeps its salt.
func TestEventLog_OrderMatters(t *testing.T) {
	h := toolResults(4)
	swapped := append(history{}, h...)
	swapped[1], swapped[2] = swapped[2], swapped[1]
	if bytes.Equal(projectLog(t, h).Root(), projectLog(t, swapped).Root()) {
		t.Fatal("reordering events did not change the Root: not order-sensitive")
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
		if err := audit.VerifyEventInclusion(root, e, proof); err != nil {
			t.Fatalf("event %d (%T) failed its own inclusion proof: %v", i, e, err)
		}
		// A tampered event must not verify against the genuine proof.
		if err := audit.VerifyEventInclusion(root, agent.TurnStarted{Seq: 999}, proof); err == nil && i != 0 {
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
	sig, _ := audit.Sign(root, edS(priv))
	if audit.VerifySignature(root, sig, edV(pub)) != nil {
		t.Fatal("signature over event Root did not verify")
	}
	tampered := append([]byte{}, root...)
	tampered[0] ^= 0xff
	if audit.VerifySignature(tampered, sig, edV(pub)) == nil {
		t.Fatal("signature verified against a tampered root")
	}
}

// TestEventLog_STH: an STH over the event log signs Root↔Size↔Timestamp, verifies, and
// any change to the bundle invalidates it — the same anchoring the journal STH gives.
func TestEventLog_STH(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	evs := sampleEvents()
	log := buildLog(t, evs)

	sth := signTH(t, log.TreeHead("run", 1_700_000_000), priv)
	if sth.Verify(edV(pub)) != nil {
		t.Fatal("event-log STH did not verify")
	}
	if sth.Size != len(evs) {
		t.Fatalf("STH Size = %d, want %d", sth.Size, len(evs))
	}

	// An inclusion proof checks against the SIGNED root — auditor trusts sth, not raw bytes.
	proof, _ := log.Prove(2)
	if err := audit.VerifyEventInclusion(sth.Root, evs[2], proof); err != nil {
		t.Fatal("event proves against its own signed STH root but verification failed")
	}

	// Tamper each bound field: signature must break.
	bad := sth
	bad.Size++
	if bad.Verify(edV(pub)) == nil {
		t.Fatal("STH verified after Size tamper")
	}
	bad = sth
	bad.TimestampNanos++
	if bad.Verify(edV(pub)) == nil {
		t.Fatal("STH verified after Timestamp tamper")
	}
	bad = sth
	bad.Root = append([]byte{}, sth.Root...)
	bad.Root[0] ^= 0xff
	if bad.Verify(edV(pub)) == nil {
		t.Fatal("STH verified after Root tamper")
	}
}

// TestEventLog_Consistency: the first m events are provably an append-only prefix of the
// later log; a rewrite of an early event breaks the proof.
func TestEventLog_Consistency(t *testing.T) {
	evs := sampleEvents()
	m := 3

	log := buildLog(t, evs[:m])
	rootEarly := log.Root() // commitment when the log held m events
	for _, e := range evs[m:] {
		if err := log.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	proof, err := log.ProveConsistency(m)
	if err != nil {
		t.Fatalf("ProveConsistency: %v", err)
	}
	if audit.VerifyConsistency(rootEarly, log.Root(), proof) != nil {
		t.Fatal("a genuinely append-only history failed the consistency proof")
	}

	// Rewrite an early event, keeping its salt: the earlier root no longer reconciles.
	h := toolResults(6)
	full := projectLog(t, h)
	proof, _ = full.ProveConsistency(m)
	if audit.VerifyConsistency(projectLog(t, h[:m]).Root(), full.Root(), proof) != nil {
		t.Fatal("a projected prefix failed the consistency proof")
	}
	tampered := append(history{}, h[:m]...)
	tampered[1].Result = json.RawMessage(`{"n":"evil"}`)
	if audit.VerifyConsistency(projectLog(t, tampered).Root(), full.Root(), proof) == nil {
		t.Fatal("consistency proof accepted a rewritten early event")
	}
}

// TestRecord_DrainsRealStream: Record over a live Agent.Stream yields the terminal answer
// AND a committed log whose every event proves. This is the end-to-end sink.
func TestRecord_DrainsRealStream(t *testing.T) {
	a := agenttest.MustNew(eventModel{text: "hello"}, agenttest.MemJournal())
	stream := a.Stream(context.Background(), "run-1", agent.UserText("hi"))

	log := audit.NewEventLog()
	var seen []agent.RunEvent
	res, err := audit.RecordStream(log, stream, func(e agent.RunEvent) { seen = append(seen, e) })
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	msg := res.Message
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
		if err := audit.VerifyEventInclusion(root, e, proof); err != nil {
			t.Fatalf("event %d (%T) failed inclusion (err=%v)", i, e, err)
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
	j := agenttest.MustJournal(store)
	tool := agent.Func("lookup", "", agent.Safety{ReadOnly: true},
		func(_ context.Context, _ struct{}) (string, error) { return "ok", nil })
	if _, err := agenttest.MustNew(&twoTurnModel{}, j, agent.WithTools(tool)).Run(ctx, "run", agent.UserText("hi")); err != nil {
		t.Fatalf("run: %v", err)
	}

	log1, err := audit.EventLogFromJournal(ctx, j, "run")
	if err != nil {
		t.Fatalf("EventLogFromJournal: %v", err)
	}
	if log1.Len() == 0 {
		t.Fatal("durable projection is empty")
	}
	log2, _ := audit.EventLogFromJournal(ctx, j, "run")
	if !bytes.Equal(log1.Root(), log2.Root()) {
		t.Fatal("durable Root not deterministic across two projections of the same journal")
	}

	// Composes with the STH anchor.
	pub, priv, _ := ed25519.GenerateKey(nil)
	sth := signTH(t, log1.TreeHead("run", 1000), priv)
	if sth.Verify(edV(pub)) != nil || sth.Size != log1.Len() {
		t.Fatalf("durable event STH failed (verify=%v size=%d/%d)", sth.Verify(edV(pub)), sth.Size, log1.Len())
	}

	// The trail up to any earlier point (the projection of a journal prefix, as it stood before
	// the run finished) is an append-only prefix of the full run.
	recs, _ := j.History(ctx, "run")
	if log1.Len() < 2 {
		t.Fatalf("need >= 2 projected events, got %d", log1.Len())
	}
	k := len(recs) - 1
	for k > 0 && projectLog(t, history(recs[:k])).Len() == log1.Len() {
		k--
	}
	prefix := projectLog(t, history(recs[:k]))
	if prefix.Len() == 0 || prefix.Len() >= log1.Len() {
		t.Fatalf("prefix projects %d of %d events", prefix.Len(), log1.Len())
	}
	proof, err := log1.ProveConsistency(prefix.Len())
	if err != nil {
		t.Fatalf("ProveConsistency: %v", err)
	}
	if audit.VerifyConsistency(prefix.Root(), log1.Root(), proof) != nil {
		t.Fatal("durable event trail failed the append-only consistency proof")
	}
}

// textOf extracts the concatenated text of a message (mirrors Message.Text()).
func textOf(m agent.Message) string { return m.Text() }
