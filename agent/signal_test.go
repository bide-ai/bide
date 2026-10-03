package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// awaitTool blocks on an external signal, then returns the delivered payload.
type awaitTool struct {
	name   string
	safety Safety
	sig    string
	calls  *int
	got    *string
}

func (t *awaitTool) Name() string                { return t.name }
func (t *awaitTool) Description() string         { return "" }
func (t *awaitTool) Safety() Safety              { return t.safety }
func (t *awaitTool) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *awaitTool) Call(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	*t.calls++
	v, err := Await[string](ctx, t.sig)
	if err != nil {
		return nil, err
	}
	if t.got != nil {
		*t.got = v
	}
	return json.Marshal(map[string]string{"event": v})
}

// A tool pauses on Await; Signal delivers an external event; re-running the same run resolves
// the await and continues, and the tool ran exactly twice (once to pause, once to resolve).
func TestAwait_PausesAndResumesOnSignal(t *testing.T) {
	store := memJournal()
	var calls int
	var got string
	tool := &awaitTool{name: "watch", safety: Safety{ReadOnly: true}, sig: "webhook", calls: &calls, got: &got}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "watch", `{}`), textTurn("done")}}
	a := mustNew(m, store, WithTools(tool))

	_, err := a.Run(context.Background(), "r", "hi")
	var awt *Awaiting
	if !errors.As(err, &awt) {
		t.Fatalf("err = %v, want *Awaiting", err)
	}
	if awt.Name != "webhook" {
		t.Fatalf("awaiting = %+v", awt)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times before signal, want 1", calls)
	}

	if err := Signal(context.Background(), store, "r", "webhook", "payload-1"); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	out, err := a.Run(context.Background(), "r", "hi") // same agent, resumes
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if textOf(out) != "done" {
		t.Fatalf("answer = %q", textOf(out))
	}
	if got != "payload-1" {
		t.Fatalf("tool received %q, want payload-1", got)
	}
	if calls != 2 {
		t.Fatalf("tool ran %d times total, want 2 (re-run to resolve)", calls)
	}
}

// A redelivered signal (at-least-once transport) is applied at most once: the first payload
// wins and later deliveries are no-ops. This is the exactly-once-intake guarantee.
func TestSignal_RedeliveryIsAtMostOnce(t *testing.T) {
	store := memJournal()
	ctx := context.Background()
	if err := Signal(ctx, store, "r", "webhook", "first"); err != nil {
		t.Fatal(err)
	}
	if err := Signal(ctx, store, "r", "webhook", "second"); err != nil { // redelivery of the same signal
		t.Fatal(err)
	}
	recs, err := store.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	var payload string
	for _, r := range recs {
		if r.Kind == StepSignal && r.Name == "signal:webhook" {
			n++
			_ = json.Unmarshal(r.Result, &payload)
		}
	}
	if n != 1 {
		t.Fatalf("signal recorded %d times, want 1 (at-most-once intake)", n)
	}
	if payload != "first" {
		t.Fatalf("payload = %q, want first (first delivery wins)", payload)
	}
}

// Await from a non-retry-safe tool is a misuse and fails with ErrConfig, not a resumable pause.
func TestAwait_RequiresRetrySafe(t *testing.T) {
	store := memJournal()
	var calls int
	tool := &awaitTool{name: "write", safety: Safety{}, sig: "e", calls: &calls} // not retry-safe
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "write", `{}`), textTurn("done")}}
	a := mustNew(m, store, WithTools(tool))

	_, err := a.Run(context.Background(), "r", "hi")
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want errors.Is ErrConfig", err)
	}
	var awt *Awaiting
	if errors.As(err, &awt) {
		t.Fatal("a non-retry-safe await must not surface as a resumable pause")
	}
}

// Await outside a running agent (no run context) is a config error.
func TestAwait_OutsideRun(t *testing.T) {
	_, err := Await[string](context.Background(), "e")
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig", err)
	}
}
