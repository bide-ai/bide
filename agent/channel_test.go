package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
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

// receiveOnce runs one retry-safe tool that calls Receive on channel and reports what it got.
func receiveOnce(t *testing.T, store *Journal, runID, channel string) (Received[string], error) {
	t.Helper()
	var got Received[string]
	var recvErr error
	recv := MustFunc("recv", "", func(ctx context.Context, _ struct{}) (string, error) {
		m, err := Receive[string](ctx, channel)
		if err != nil {
			recvErr = err
			return "", err
		}
		got = m
		return m.Payload, nil
	}, WithSafety(Safety{ReadOnly: true}))
	a := mustNew(NewScriptedModel(ToolTurn("c1", "recv", `{}`), TextTurn("done")), store, WithTools(recv))
	_, _ = a.Run(context.Background(), runID, UserText("go"))
	return got, recvErr
}

// A channel whose name extends another's with ':' is a different channel: Receive on
// "orders" does not see a message sent to "orders:vip".
func TestChannel_NameWithColonIsItsOwnChannel(t *testing.T) {
	store := memJournal()
	if err := store.Enqueue(context.Background(), "r", "orders:vip", "k1", "vip-order"); err != nil {
		t.Fatal(err)
	}
	got, err := receiveOnce(t, store, "r", "orders")
	var aw *SignalPending
	if !errors.As(err, &aw) {
		t.Fatalf("Receive(\"orders\") = %+v, %v; want *SignalPending (the message is on \"orders:vip\")", got, err)
	}
	store2 := memJournal()
	if err := store2.Enqueue(context.Background(), "r", "orders:vip", "k1", "vip-order"); err != nil {
		t.Fatal(err)
	}
	if got, err := receiveOnce(t, store2, "r", "orders:vip"); err != nil || got.Key != "k1" || got.Payload != "vip-order" {
		t.Fatalf("Receive(\"orders:vip\") = %+v, %v; want its own message", got, err)
	}
}

// No two (channel, key) pairs share a journal step, for messages or for acks: acking key
// "b:c" on channel "a" does not ack key "c" on channel "a:b", and the two sends are distinct.
func TestChannel_ColonsNeverCollide(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	if err := store.Enqueue(ctx, "r", "a:b", "c", "on a:b"); err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(ctx, "r", "a", "b:c", "on a"); err != nil {
		t.Fatal(err)
	}
	if err := Ack(ctx, store, "r", "a", "b:c"); err != nil {
		t.Fatal(err)
	}
	got, err := receiveOnce(t, store, "r", "a:b")
	if err != nil || got.Key != "c" || got.Payload != "on a:b" {
		t.Fatalf("Receive(\"a:b\") = %+v, %v; want key c with payload \"on a:b\" (it was never acked)", got, err)
	}
	pairs := [][2]string{{"a:b", "c"}, {"a", "b:c"}, {"a:", "b"}, {"a", ":b"}, {"", "a:b"}, {"a:b", ""}}
	msgs, acks := map[string][2]string{}, map[string][2]string{}
	for _, p := range pairs {
		if prev, dup := msgs[chanStep(p[0], p[1])]; dup {
			t.Errorf("messages %q and %q share step %q", prev, p, chanStep(p[0], p[1]))
		}
		msgs[chanStep(p[0], p[1])] = p
		if prev, dup := acks[chanAckStep(p[0], p[1])]; dup {
			t.Errorf("acks %q and %q share step %q", prev, p, chanAckStep(p[0], p[1]))
		}
		acks[chanAckStep(p[0], p[1])] = p
	}
}

// awaitTool blocks on an external signal, then returns the delivered payload.
type awaitTool struct {
	name   string
	safety Safety
	sig    string
	calls  *int
	got    *string
}

// Spec describes the tool to the agent (see Tool).
func (t *awaitTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Description: "", Input: json.RawMessage(`{"type":"object"}`), Safety: t.safety}
}

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

	_, err := a.Run(context.Background(), "r", UserText("hi"))
	var awt *SignalPending
	if !errors.As(err, &awt) {
		t.Fatalf("err = %v, want *SignalPending", err)
	}
	if awt.Name != "webhook" {
		t.Fatalf("awaiting = %+v", awt)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times before signal, want 1", calls)
	}

	if err := store.Signal(context.Background(), "r", "webhook", "payload-1"); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	res, err := a.Run(context.Background(), "r", UserText("hi")) // same agent, resumes
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	out := res.Message
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
	if err := store.Signal(ctx, "r", "webhook", "first"); err != nil {
		t.Fatal(err)
	}
	if err := store.Signal(ctx, "r", "webhook", "second"); err != nil { // redelivery of the same signal
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

	_, err := a.Run(context.Background(), "r", UserText("hi"))
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want errors.Is ErrConfig", err)
	}
	var awt *SignalPending
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

// End-to-end tests for durable signals: the signal pause/resume path composed with the
// at-most-once side-effect guarantee under crashes (DST), and the deliver-then-wake idiom
// driven by a real Waker. The per-stage tests (channel_test.go, awaitfor_test.go,
// channel_test.go) cover the units; these prove the whole loop.

// awaitThenChargeModel calls the retry-safe "await" tool first (which pauses until a signal
// is delivered), then the NON-idempotent "charge" tool once the await has resolved, then
// answers. It is a pure function of how many tool results are already in the conversation,
// so re-calling it after a crash returns the same turn (replayable model).
type awaitThenChargeModel struct{}

func (awaitThenChargeModel) Stream(_ context.Context, req Request) (*Stream, error) {
	results := 0
	for _, m := range req.Messages {
		if m.Role == RoleTool {
			for _, p := range m.Parts {
				if _, ok := p.(ToolResult); ok {
					results++
				}
			}
		}
	}
	var emits []Emit
	switch results {
	case 0:
		emits = toolTurn("c-await", "await", `{}`) // gate on the signal first
	case 1:
		emits = toolTurn("c-charge", "charge", `{}`) // then the non-idempotent action
	default:
		emits = textTurn("done")
	}
	ch := make(chan Emit, len(emits))
	for _, e := range emits {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

func runAwaitCharge(mem *Journal, awaitCalls, chargeCount *int, crashAt int) error {
	awaitT := &awaitTool{name: "await", safety: Safety{ReadOnly: true}, sig: "go", calls: awaitCalls}
	chargeT := chargeTool{count: chargeCount}
	a := mustNew(
		awaitThenChargeModel{},
		crashJournal(mem, crashAt),
		WithTools(awaitT, chargeT),
		WithMaxConcurrency(1),
	)
	_, err := a.Run(context.Background(), "dst-sig", UserText("start"))
	return err
}

// A signal-gated non-idempotent action fires at most once across every crash schedule. The
// run awaits a delivered signal, resolves it on resume, then charges; a crash swept across
// every write point must never double-charge, and the run always ends completed or halted.
// This proves durable signals compose with the at-most-once moat end to end.
func TestDST_Signal_NoDoubleFire_CrashSweep(t *testing.T) {
	haltSeen := false
	for crashAt := 1; crashAt <= 40; crashAt++ {
		var awaitCalls, count int
		mem := memJournal()
		// The signal is delivered (persisted) up front, so the await resolves on the first run;
		// the crash sweep then exercises the resume-and-charge path.
		if err := mem.Signal(context.Background(), "dst-sig", "go", "payload"); err != nil {
			t.Fatalf("Signal: %v", err)
		}

		err := runAwaitCharge(mem, &awaitCalls, &count, crashAt)
		crashed := errors.Is(err, errCrash)
		for errors.Is(err, errCrash) { // resume without further crashes
			err = runAwaitCharge(mem, &awaitCalls, &count, 0)
		}

		if count > 1 {
			t.Fatalf("crashAt=%d: charge fired %d times after signal-driven resume: DOUBLE FIRE", crashAt, count)
		}
		var halt *OutcomeUnknown
		switch {
		case err == nil:
			if count != 1 {
				t.Fatalf("crashAt=%d: completed run charged %d times, want 1", crashAt, count)
			}
		case errors.As(err, &halt):
			haltSeen = true // charge fired but its outcome was unknown at the crash: halted, not retried
		default:
			t.Fatalf("crashAt=%d: unexpected terminal error: %v", crashAt, err)
		}

		if !crashed { // crashAt exceeded the clean-run write count: sweep complete
			break
		}
	}
	if !haltSeen {
		t.Fatal("no crash point exercised OutcomeUnknown: the halt path was never tested")
	}
}

// Deliver-then-wake, end to end: a run awaits a signal and pauses; an external deliverer
// records the signal and schedules a wake at the current time; the Waker then resumes the run,
// which resolves the await and completes. This is the idiom stage 4 (a separate Notifier)
// would have added, shown to already work with the existing Waker.
func TestSignal_DeliverThenWake(t *testing.T) {
	mem := memJournal()
	var got string
	awaitT := &awaitTool{name: "await", safety: Safety{ReadOnly: true}, sig: "go", calls: new(int), got: &got}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "await", `{}`), textTurn("done")}}

	var a *Agent
	var completed bool
	waker := NewMemWaker(func(ctx context.Context, runID string) error {
		res, err := a.Run(ctx, runID, UserText("hi"))
		var out Message
		if res != nil {
			out = res.Message
		}
		var awt *SignalPending
		if errors.As(err, &awt) {
			return nil // still waiting is not an error
		}
		if err != nil {
			return err
		}
		if textOf(out) == "done" {
			completed = true
		}
		return nil
	})
	a = mustNew(m, mem, WithTools(awaitT))
	ctx := contextWithWaker(context.Background(), waker)

	// First run pauses on the await (plain Await does not self-schedule a wake).
	_, err := a.Run(ctx, "r", UserText("hi"))
	var awt *SignalPending
	if !errors.As(err, &awt) {
		t.Fatalf("first run err = %v, want *SignalPending", err)
	}

	// The deliverer records the signal, then schedules a wake now (the deliver-then-wake idiom).
	if err := mem.Signal(context.Background(), "r", "go", "payload"); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	waker.Schedule(context.Background(), Wake{RunID: "r", Name: "signal:go", FireAt: time.Time{}}) // zero time is before now, so the wake is due

	n, err := waker.Fire(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if n != 1 {
		t.Fatalf("Fire resumed %d runs, want 1", n)
	}
	if !completed {
		t.Fatal("run did not complete after deliver-then-wake")
	}
	if got != "payload" {
		t.Fatalf("await received %q, want payload", got)
	}
}
