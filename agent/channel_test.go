package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// drainTool loops Receive -> record -> Ack over one channel, so it consumes every message on
// that channel in order, exactly once. It is retry-safe (ReadOnly): on resume it re-runs from
// the top and the journaled acks make Receive advance past the messages it already handled.
type drainTool struct {
	name  string
	ch    string
	calls *int
	got   *[]string
}

// Spec describes the tool to the agent (see Tool).
func (t *drainTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Description: "", Input: json.RawMessage(`{"type":"object"}`), Safety: Safety{ReadOnly: true}}
}

func (t *drainTool) Call(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	*t.calls++
	d, runID, _ := runContext(ctx)
	for {
		msg, err := Receive[string](ctx, t.ch)
		if err != nil {
			return nil, err // *SignalPending when drained: pauses the run durably
		}
		*t.got = append(*t.got, msg.Payload)
		if err := Ack(ctx, d, runID, t.ch, msg.Key); err != nil {
			return nil, err
		}
	}
}

// Order + exactly-once: three messages are consumed in delivery order, each once.
func TestChannel_OrderExactlyOnce(t *testing.T) {
	store := memJournal()
	ctx := context.Background()
	for _, m := range []struct{ k, v string }{{"k1", "one"}, {"k2", "two"}, {"k3", "three"}} {
		if err := store.Enqueue(ctx, "r", "inbox", m.k, m.v); err != nil {
			t.Fatalf("Send %s: %v", m.k, err)
		}
	}

	var calls int
	var got []string
	tool := &drainTool{name: "drain", ch: "inbox", calls: &calls, got: &got}
	// After draining all three the tool loops back to Receive, hits an empty channel, and
	// pauses with *SignalPending; the second turn's script line is never reached.
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "drain", `{}`), textTurn("done")}}
	a := mustNew(m, store, WithTools(tool))

	_, err := a.Run(ctx, "r", UserText("hi"))
	var awt *SignalPending
	if !errors.As(err, &awt) {
		t.Fatalf("err = %v, want *SignalPending after draining", err)
	}
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("consumed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("consumed %v, want %v (order)", got, want)
		}
	}
}

// Redelivery dedup: sending the same key twice with different payloads records one message and
// the first payload wins (at-most-once intake over at-least-once transport).
func TestSend_RedeliveryIsAtMostOnce(t *testing.T) {
	store := memJournal()
	ctx := context.Background()
	if err := store.Enqueue(ctx, "r", "inbox", "k1", "first"); err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(ctx, "r", "inbox", "k1", "second"); err != nil { // redelivery of the same key
		t.Fatal(err)
	}
	recs, err := store.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	var payload string
	for _, r := range recs {
		if r.Kind == StepSignal && r.Name == "chan:5:inbox:k1" {
			n++
			_ = json.Unmarshal(r.Result, &payload)
		}
	}
	if n != 1 {
		t.Fatalf("message recorded %d times, want 1 (at-most-once intake)", n)
	}
	if payload != "first" {
		t.Fatalf("payload = %q, want first (first delivery wins)", payload)
	}
}

// Empty channel pauses: Receive on an empty channel yields *SignalPending; after Send + re-run it
// resolves and the run completes.
func TestChannel_EmptyPausesThenResumes(t *testing.T) {
	store := memJournal()
	ctx := context.Background()

	var calls int
	var got []string
	tool := &drainTool{name: "drain", ch: "inbox", calls: &calls, got: &got}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "drain", `{}`), textTurn("done")}}
	a := mustNew(m, store, WithTools(tool))

	_, err := a.Run(ctx, "r", UserText("hi")) // channel empty: pauses immediately
	var awt *SignalPending
	if !errors.As(err, &awt) {
		t.Fatalf("err = %v, want *SignalPending on empty channel", err)
	}
	if awt.Name != "inbox" {
		t.Fatalf("awaiting = %+v, want channel inbox", awt)
	}
	if len(got) != 0 {
		t.Fatalf("consumed %v before any Send, want none", got)
	}

	if err := store.Enqueue(ctx, "r", "inbox", "k1", "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	res, err := a.Run(ctx, "r", UserText("hi"))
	var out Message
	if res != nil {
		out = res.Message
	} // re-run: Receive now resolves, drains, then pauses again? no:
	// after handling k1 the tool loops back, finds the channel drained, and pauses again. So the
	// resolved-and-drained run pauses once more rather than completing. Assert it consumed k1.
	var awt2 *SignalPending
	if !errors.As(err, &awt2) {
		t.Fatalf("resume err = %v, want *SignalPending (drained after consuming k1)", err)
	}
	_ = out
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("consumed %v, want [hello]", got)
	}
}

// Resume mid-stream: acking k1 (as a durable journal entry) makes a fresh Receive return k2,
// simulating replay where acks are journaled and Receive advances past handled messages.
func TestChannel_ResumeMidStream(t *testing.T) {
	store := memJournal()
	ctx := context.Background()
	for _, m := range []struct{ k, v string }{{"k1", "one"}, {"k2", "two"}, {"k3", "three"}} {
		if err := store.Enqueue(ctx, "r", "inbox", m.k, m.v); err != nil {
			t.Fatalf("Send %s: %v", m.k, err)
		}
	}

	// Drive Receive directly inside a run context (no full loop): first message is the oldest.
	rctx := withRunContext(ctx, store, "r", "", false)
	first, err := Receive[string](rctx, "inbox")
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if first.Key != "k1" || first.Payload != "one" {
		t.Fatalf("first = %+v, want {k1 one}", first)
	}

	// Before ack, Receive keeps returning the same message (replay-safe: it writes nothing).
	again, err := Receive[string](rctx, "inbox")
	if err != nil {
		t.Fatalf("Receive again: %v", err)
	}
	if again.Key != "k1" {
		t.Fatalf("re-Receive = %+v, want the same k1 (replay-safe)", again)
	}

	// Ack k1 (journaled), then a fresh Receive advances to k2.
	if err := Ack(ctx, store, "r", "inbox", "k1"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	next, err := Receive[string](rctx, "inbox")
	if err != nil {
		t.Fatalf("Receive after ack: %v", err)
	}
	if next.Key != "k2" || next.Payload != "two" {
		t.Fatalf("after ack k1, Receive = %+v, want {k2 two}", next)
	}

	// Ack is idempotent: a second ack of k1 is a no-op and Receive still points at k2.
	if err := Ack(ctx, store, "r", "inbox", "k1"); err != nil {
		t.Fatalf("Ack (idempotent): %v", err)
	}
	still, err := Receive[string](rctx, "inbox")
	if err != nil {
		t.Fatalf("Receive after re-ack: %v", err)
	}
	if still.Key != "k2" {
		t.Fatalf("after idempotent re-ack, Receive = %+v, want {k2 two}", still)
	}
}
