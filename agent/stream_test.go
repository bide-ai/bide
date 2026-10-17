package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var errCrash = errors.New("crash")

// ExampleAgent_Stream shows the streaming API: range Events for live progress, then
// call Final for the terminal answer. Run is the blocking equivalent of Final.
func ExampleAgent_Stream() {
	// A model that answers directly (a real adapter streams token deltas the same way).
	m := &scriptModel{turns: [][]Emit{textTurn("hello world")}}
	a := mustNew(m, memJournal())

	stream := a.Stream(context.Background(), "run-1", UserText("hi"))
	for ev := range stream.Events() {
		switch e := ev.(type) {
		case TurnStarted:
			fmt.Println("turn started")
		case ModelEvent:
			if d, ok := e.Event.(TextDelta); ok {
				fmt.Printf("delta: %q\n", d.Text)
			}
		case Finished:
			fmt.Println("finished")
		}
	}
	res, err := stream.Result()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	answer := res.Message
	fmt.Println("answer:", textOf(answer))
	// Output:
	// turn started
	// delta: "hello world"
	// finished
	// answer: hello world
}

// collect drains a stream's events and its final result together.
func collect(as *RunStream) ([]RunEvent, Message, error) {
	var evs []RunEvent
	for e := range as.Events() {
		evs = append(evs, e)
	}
	res, err := as.Result()
	var msg Message
	if res != nil {
		msg = res.Message
	}
	return evs, msg, err
}

func kinds(evs []RunEvent) []string {
	var out []string
	for _, e := range evs {
		switch ev := e.(type) {
		case TurnStarted:
			out = append(out, "turn")
		case ModelEvent:
			out = append(out, "model")
		case AssistantTurn:
			if ev.Replayed {
				out = append(out, "asst(replay)")
			} else {
				out = append(out, "asst")
			}
		case ToolStarted:
			out = append(out, "tool.start")
		case ToolCompleted:
			out = append(out, "tool.done")
		case ApprovalRequired:
			out = append(out, "approval")
		case Finished:
			out = append(out, "finished")
		}
	}
	return out
}

// Streaming surfaces the whole lifecycle: turn boundaries, live model deltas, tool
// start/finish, and a terminal Finished — and Final returns the same answer as Run.
func TestStream_EmitsLifecycle(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("final")}}
	a := mustNew(m, memJournal(), WithTools(tool))

	evs, out, err := collect(a.Stream(context.Background(), "run1", UserText("hi")))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := textOf(out); got != "final" {
		t.Fatalf("answer = %q, want %q", got, "final")
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times, want 1", calls)
	}

	// Turn 1: tool call. Turn 2: text answer. Deltas arrive as ModelEvents before the
	// assembled AssistantTurn; the tool runs between the turns; Finished is last.
	want := []string{
		"turn", "model", "model", "asst", // model asks for the tool
		"tool.start", "tool.done", // tool executes
		"turn", "model", "model", "asst", // model answers
		"finished",
	}
	got := kinds(evs)
	if len(got) != len(want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	// The last event carries the same terminal message Final reports.
	fin, ok := evs[len(evs)-1].(Finished)
	if !ok || textOf(fin.Final) != "final" {
		t.Fatalf("last event = %+v, want Finished{final}", evs[len(evs)-1])
	}
}

// Final without ranging Events behaves exactly like Run (drains and discards events).
func TestStream_FinalOnlyMatchesRun(t *testing.T) {
	tool := func() Tool {
		var c int
		return &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &c}
	}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("done")}}
	a := mustNew(m, memJournal(), WithTools(tool()))

	res, err := a.Stream(context.Background(), "r", UserText("hi")).Result()
	if err != nil {
		t.Fatalf("Final: %v", err)
	}
	out := res.Message
	if got := textOf(out); got != "done" {
		t.Fatalf("answer = %q, want %q", got, "done")
	}
}

// On resume, a fresh stream replays the journaled transcript (AssistantTurn{Replayed}
// + ToolCompleted) before live progress, so a UI reconstructs the full story.
func TestStream_ReplaysJournalOnResume(t *testing.T) {
	store := memJournal()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}

	// First attempt crashes after the tool result is journaled (model errors on turn 2).
	crashy := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), errTurn(errCrash)}}
	if _, err := mustNew(crashy, store, WithTools(tool)).Run(context.Background(), "run1", UserText("hi")); err == nil {
		t.Fatal("expected crash on first attempt")
	}

	// Resume with a healthy model, streaming this time.
	recovered := &scriptModel{turns: [][]Emit{textTurn("final")}}
	evs, out, err := collect(mustNew(recovered, store, WithTools(tool)).Stream(context.Background(), "run1", UserText("hi")))
	if err != nil {
		t.Fatalf("resume Stream: %v", err)
	}
	if got := textOf(out); got != "final" {
		t.Fatalf("answer = %q, want %q", got, "final")
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times across crash+resume, want 1", calls)
	}

	got := kinds(evs)
	// Replayed assistant turn + replayed tool result come first, then the live answer.
	want := []string{"asst(replay)", "tool.done", "turn", "model", "model", "asst", "finished"}
	if len(got) != len(want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// A producer that relies on send alone to learn the consumer is gone: it never checks ctx and
// would stream forever. Cancelling mid-stream must end the consumer's read with the context's
// error; a stream cut short by cancellation must never read as a complete message.
func TestNewStreamFunc_CancelledMidStreamIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewStreamFunc(ctx, func(send func(Emit) bool) {
		for {
			if !send(Emit{Event: TextDelta{Text: "x"}}) {
				return
			}
		}
	})
	var n int
	var gotErr error
	for _, err := range s.Events() {
		if err != nil {
			gotErr = err
			break
		}
		if n++; n == 3 {
			cancel()
		}
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("a stream cancelled mid-response ended with err = %v, want context.Canceled", gotErr)
	}
}

// Once ctx is cancelled, no further event is delivered, even to a consumer blocked waiting for
// one, and the stream ends with the context's error. Draining with Message therefore never
// returns a message completed after the caller cancelled.
func TestNewStreamFunc_NothingDeliveredAfterCancel(t *testing.T) {
	// The consumer is parked waiting for the next event when the late sends happen, so a send
	// and the cancellation are ready at once; a select alone would pick either. Repeated so
	// that ordering is exercised, not left to chance.
	for i := range 20 {
		ctx, cancel := context.WithCancel(context.Background())
		var delivered int
		s := NewStreamFunc(ctx, func(send func(Emit) bool) {
			send(Emit{Event: TextDelta{Text: "partial"}})
			cancel()
			time.Sleep(time.Millisecond) // let the consumer park on its next receive
			for range 10 {
				if send(Emit{Event: TextDelta{Text: "late"}}) {
					delivered++
				}
			}
			if send(Emit{Event: Finish{Reason: "stop"}}) {
				delivered++
			}
		})
		msg, _, err := s.Message()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("round %d: Message() = %+v, %v; want context.Canceled", i, msg, err)
		}
		if delivered != 0 {
			t.Fatalf("round %d: %d events were delivered after the context was cancelled, want 0", i, delivered)
		}
	}
}

// Close releases a producer the consumer never read from.
func TestNewStreamFunc_CloseReleasesProducer(t *testing.T) {
	done := make(chan struct{})
	s := NewStreamFunc(context.Background(), func(send func(Emit) bool) {
		defer close(done)
		for send(Emit{Event: TextDelta{Text: "x"}}) {
		}
	})
	s.Close()
	s.Close() // idempotent
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the producer is still blocked 2s after Close")
	}
}

// renderTurns ranges a stream the way a UI does: it appends each text delta and clears the
// turn's text on TurnRestarted. It returns the rendered text and the TurnRestarted events.
func renderTurns(t *testing.T, as *RunStream) (string, []TurnRestarted) {
	t.Helper()
	var text strings.Builder
	var restarts []TurnRestarted
	for ev := range as.Events() {
		switch e := ev.(type) {
		case ModelEvent:
			if d, ok := e.Event.(TextDelta); ok {
				text.WriteString(d.Text)
			}
		case TurnRestarted:
			restarts = append(restarts, e)
			text.Reset()
		}
	}
	return text.String(), restarts
}

// retryOnceMW calls next again when the first call fails, as middleware.Retry does.
func retryOnceMW(next ModelHandler) ModelHandler {
	return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
		resp, err := next(ctx, call)
		if err != nil {
			return next(ctx, call)
		}
		return resp, err
	}
}

// A model attempt that streams part of its answer and then fails, and is retried by
// middleware, must not leave its partial text in what the consumer renders: the consumer
// would show "partial complete" while the run records "complete".
func TestStream_RetriedTurnMarksDiscardedDeltas(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{
		{{Event: TextDelta{Text: "partial "}}, {Err: errors.New("connection reset")}},
		textTurn("complete"),
	}}
	as := mustNew(m, memJournal(), WithMiddleware(retryOnceMW)).Stream(context.Background(), "r", UserText("go"))
	rendered, restarts := renderTurns(t, as)
	res, err := as.Result()
	if err != nil {
		t.Fatalf("Final: %v", err)
	}
	final := res.Message
	if rendered != final.Text() {
		t.Fatalf("the consumer rendered %q but the run recorded %q", rendered, final.Text())
	}
	if len(restarts) != 1 || restarts[0].Seq != 0 {
		t.Fatalf("restarts = %+v, want one TurnRestarted{Seq: 0}", restarts)
	}
}

// The same holds when a middleware replaces a failed live attempt with a response from
// elsewhere: the agent replays the response, after a restart, with no sink code in the
// middleware.
func TestStream_FallbackResponseMarksDiscardedDeltas(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{
		{{Event: TextDelta{Text: "partial "}}, {Err: errors.New("connection reset")}},
	}}
	fallback := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			if resp, err := next(ctx, call); err == nil {
				return resp, nil
			}
			return ModelResponse{Message: Message{Role: RoleAssistant, Parts: []Part{Text{Text: "fallback"}}}}, nil
		}
	}
	as := mustNew(m, memJournal(), WithMiddleware(fallback)).Stream(context.Background(), "r", UserText("go"))
	rendered, restarts := renderTurns(t, as)
	res, err := as.Result()
	if err != nil {
		t.Fatalf("Final: %v", err)
	}
	final := res.Message
	if rendered != final.Text() {
		t.Fatalf("the consumer rendered %q but the run recorded %q", rendered, final.Text())
	}
	if len(restarts) != 1 {
		t.Fatalf("restarts = %+v, want one", restarts)
	}
}

// retryTwiceMW calls next up to three times in all while it fails.
func retryTwiceMW(next ModelHandler) ModelHandler {
	return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
		resp, err := next(ctx, call)
		for i := 0; i < 2 && err != nil; i++ {
			resp, err = next(ctx, call)
		}
		return resp, err
	}
}

// A retried attempt that streamed nothing leaves nothing to discard, so no TurnRestarted is
// emitted for it; and each turn's restart carries that turn's Seq.
func TestStream_RestartOnlyAfterStreamedDeltas(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		errTurn(errors.New("refused")), // turn 0, first attempt: fails before streaming
		toolTurn("c1", "lookup", `{}`),
		{{Event: TextDelta{Text: "half"}}, {Err: errors.New("reset")}}, // turn 1: streams, then fails
		errTurn(errors.New("refused")),                                 // turn 1, second attempt: fails before streaming
		textTurn("done"),
	}}
	as := mustNew(m, memJournal(), WithTools(tool), WithMiddleware(retryTwiceMW)).Stream(context.Background(), "r", UserText("go"))
	rendered, restarts := renderTurns(t, as)
	res, err := as.Result()
	if err != nil {
		t.Fatalf("Final: %v", err)
	}
	final := res.Message
	if len(restarts) != 1 || restarts[0].Seq != 1 {
		t.Fatalf("restarts = %+v, want one TurnRestarted{Seq: 1}", restarts)
	}
	if rendered != final.Text() {
		t.Fatalf("the consumer rendered %q but the run recorded %q", rendered, final.Text())
	}
}

// liveRetryModel fails its first request after streaming "partial", then streams "a", waits
// until the caller has rendered it, and finishes with "b".
type liveRetryModel struct {
	n       int
	sawA    chan struct{}
	timeout time.Duration
}

func (m *liveRetryModel) Stream(ctx context.Context, _ Request) (*Stream, error) {
	if m.n++; m.n == 1 {
		return NewStream(chanOf(Emit{Event: TextDelta{Text: "partial"}}, Emit{Err: errors.New("reset")})), nil
	}
	return NewStreamFunc(ctx, func(send func(Emit) bool) {
		if !send(Emit{Event: TextDelta{Text: "a"}}) {
			return
		}
		select {
		case <-m.sawA:
		case <-time.After(m.timeout):
			send(Emit{Err: errors.New("the caller did not see the retried attempt's delta live")})
			return
		}
		send(Emit{Event: TextDelta{Text: "b"}})
		send(Emit{Event: Finish{Reason: FinishStop}})
	}), nil
}

func chanOf(es ...Emit) <-chan Emit {
	ch := make(chan Emit, len(es))
	for _, e := range es {
		ch <- e
	}
	close(ch)
	return ch
}

// A failed attempt releases the stream: the attempt a retry makes next streams live, not replayed
// once it is over.
func TestStream_RetryStreamsLive(t *testing.T) {
	m := &liveRetryModel{sawA: make(chan struct{}), timeout: 5 * time.Second}
	as := mustNew(m, memJournal(), WithMiddleware(retryOnceMW)).Stream(context.Background(), "r", UserText("go"))
	var got []string
	for ev := range as.Events() {
		switch e := ev.(type) {
		case ModelEvent:
			if d, ok := e.Event.(TextDelta); ok {
				got = append(got, d.Text)
				if d.Text == "a" {
					close(m.sawA)
				}
			}
		case TurnRestarted:
			got = append(got, "restart")
		}
	}
	if _, err := as.Result(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"partial", "restart", "a", "b"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stream = %q, want %q", got, want)
	}
}

// slowDeltas streams one delta at once, then more after a delay, and does not stop when its
// context is cancelled.
type slowDeltas struct{}

func (slowDeltas) Stream(context.Context, Request) (*Stream, error) {
	ch := make(chan Emit)
	go func() {
		defer close(ch)
		ch <- Emit{Event: TextDelta{Text: "early "}}
		time.Sleep(40 * time.Millisecond)
		ch <- Emit{Event: TextDelta{Text: "late"}}
		ch <- Emit{Event: Finish{Reason: "stop"}}
	}()
	return NewStream(ch), nil
}

// leakyFanOut is a badly behaved middleware: it starts a second call to the model in a goroutine
// it never waits for, then returns its own answer at once. The leftover call claims the run's
// token sink and would stream deltas after the run has ended.
func leakyFanOut(next ModelHandler) ModelHandler {
	return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
		go func() { _, _ = next(ctx, call) }()
		return ModelResponse{Message: Message{Role: RoleAssistant, Parts: []Part{Text{Text: "answer"}}}}, nil
	}
}

// Middleware is a public extension point, so a stream must survive a middleware that leaves a
// model call running past the end of the run: its late events are dropped, never sent on the
// closed event channel (which panics and takes down the process).
func TestStream_SurvivesEventsAfterTheRunEnds(t *testing.T) {
	a := mustNew(slowDeltas{}, memJournal(), WithMiddleware(leakyFanOut))
	as := a.Stream(context.Background(), "r1", UserText("hi"))
	for range as.Events() {
	}
	res, err := as.Result()
	var out Message
	if res != nil {
		out = res.Message
	}
	if err != nil || textOf(out) != "answer" {
		t.Fatalf("Final = %q, %v; want the middleware's answer", textOf(out), err)
	}
	time.Sleep(150 * time.Millisecond) // the leftover call's late delta arrives now
}

// leftoverModel serves a run whose first turn a middleware answers itself while a request it
// left running keeps streaming. That leftover request streams "early", then, once the next turn
// has started, "late". The next turn waits until the leftover request has ended.
type leftoverModel struct {
	leftoverStarted, nextStarted, leftoverDone chan struct{}
}

func (m *leftoverModel) Stream(_ context.Context, req Request) (*Stream, error) {
	if req.Messages[len(req.Messages)-1].Role == RoleTool { // the next turn
		close(m.nextStarted)
		<-m.leftoverDone
		ch := make(chan Emit, 2)
		ch <- Emit{Event: TextDelta{Text: "done"}}
		ch <- Emit{Event: Finish{Reason: FinishStop}}
		close(ch)
		return NewStream(ch), nil
	}
	close(m.leftoverStarted) // the request has claimed the stream
	ch := make(chan Emit)
	go func() {
		defer close(ch)
		ch <- Emit{Event: TextDelta{Text: "early"}}
		<-m.nextStarted
		ch <- Emit{Event: TextDelta{Text: "late"}}
		ch <- Emit{Event: Finish{Reason: FinishStop}}
	}()
	return NewStream(ch), nil
}

// Once a turn has returned its response, nothing a request of that turn still sends reaches the
// caller, even while the run goes on: the leftover request's late delta must not appear in the
// next turn's feed.
func TestStream_LeftoverRequestCannotStreamIntoTheNextTurn(t *testing.T) {
	m := &leftoverModel{leftoverStarted: make(chan struct{}), nextStarted: make(chan struct{}), leftoverDone: make(chan struct{})}
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	first := true
	leave := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			if !first {
				return next(ctx, call)
			}
			first = false
			go func() {
				_, _ = next(context.WithoutCancel(ctx), call.AddHook(ModelCallHook{
					After: func(context.Context, ModelCall, ModelAttempt) { close(m.leftoverDone) },
				}))
			}()
			<-m.leftoverStarted
			return ModelResponse{Message: Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{}`)}}}}, nil
		}
	}
	as := mustNew(m, memJournal(), WithTools(tool), WithMiddleware(leave)).Stream(context.Background(), "r", UserText("go"))
	for ev := range as.Events() {
		if me, ok := ev.(ModelEvent); ok {
			if d, ok := me.Event.(TextDelta); ok && d.Text == "late" {
				t.Error("a request of the first turn streamed into the second")
			}
		}
	}
	if out, err := answerOf(as.Result()); err != nil || out.Text() != "done" {
		t.Fatalf("Final = %q, %v; want done", out.Text(), err)
	}
}
