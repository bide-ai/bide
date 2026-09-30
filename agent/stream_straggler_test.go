package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

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
	a := New(slowDeltas{}, NewMemStore()).Use(leakyFanOut)
	as := a.Stream(context.Background(), "r1", "hi")
	for range as.Events() {
	}
	out, err := as.Final()
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
	as := New(m, NewMemStore(), tool).Use(leave).Stream(context.Background(), "r", "go")
	for ev := range as.Events() {
		if me, ok := ev.(ModelEvent); ok {
			if d, ok := me.Event.(TextDelta); ok && d.Text == "late" {
				t.Error("a request of the first turn streamed into the second")
			}
		}
	}
	if out, err := as.Final(); err != nil || out.Text() != "done" {
		t.Fatalf("Final = %q, %v; want done", out.Text(), err)
	}
}
