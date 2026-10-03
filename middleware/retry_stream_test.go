package middleware_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/middleware"
)

// flakyStreamer streams part of an answer and then fails on its first call, and streams the
// whole answer on later calls.
type flakyStreamer struct{ calls atomic.Int32 }

func (m *flakyStreamer) Stream(context.Context, agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 3)
	if m.calls.Add(1) == 1 {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "The answer is "}}
		ch <- agent.Emit{Err: errors.New("stream reset by peer")}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "The answer is 42."}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// Retry re-calls the model after a streamed attempt fails part way. A streaming consumer must
// be able to tell the failed attempt's deltas from the answer the run records: rendering the
// deltas and clearing them on TurnRestarted shows exactly the recorded answer, not the
// partial text followed by the full text.
func TestRetry_StreamedPartialAttemptIsMarkedDiscarded(t *testing.T) {
	m := &flakyStreamer{}
	a := agenttest.MustNew(
		m,
		agenttest.MemJournal(),
		agent.WithMiddleware(middleware.Retry(2, middleware.WithBackoff(time.Millisecond, time.Millisecond))),
	)
	as := a.Stream(context.Background(), "r", "q")
	var rendered strings.Builder
	for ev := range as.Events() {
		switch e := ev.(type) {
		case agent.ModelEvent:
			if d, ok := e.Event.(agent.TextDelta); ok {
				rendered.WriteString(d.Text)
			}
		case agent.TurnRestarted:
			rendered.Reset()
		}
	}
	final, err := as.Final()
	if err != nil {
		t.Fatalf("Final: %v", err)
	}
	if m.calls.Load() != 2 {
		t.Fatalf("model called %d times, want 2", m.calls.Load())
	}
	if rendered.String() != final.Text() {
		t.Fatalf("the consumer rendered %q but the run recorded %q", rendered.String(), final.Text())
	}
}
