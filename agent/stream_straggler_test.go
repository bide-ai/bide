package agent

import (
	"context"
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
// it never waits for, then returns its own answer at once. The leftover call still holds the
// run's token sink, so it streams deltas after the run has ended.
func leakyFanOut(next ModelHandler) ModelHandler {
	return func(ctx context.Context, req Request) (Message, Usage, error) {
		go func() { _, _, _ = next(ctx, req) }()
		return Message{Role: RoleAssistant, Parts: []Part{Text{Text: "answer"}}}, Usage{}, nil
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
