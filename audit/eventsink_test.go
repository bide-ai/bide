package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"

	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/audit"
)

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
		// A tampered event must not verify against the honest proof.
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

// textOf extracts the concatenated text of a message (mirrors Message.Text()).
func textOf(m agent.Message) string { return m.Text() }
