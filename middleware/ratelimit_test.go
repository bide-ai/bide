package middleware_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// An interval of zero is a rate of one call per zero time, which is no limit. The limiter must
// not run out of tokens after the first burst and block every later call until its context ends.
func TestRateLimiter_ZeroIntervalDoesNotBlock(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		r := middleware.NewRateLimiter(interval, 1)
		h := callWith(&stubModel{text: "x"}, middleware.RateLimit(r))
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		for i := range 5 {
			if _, err := h(ctx); err != nil {
				t.Fatalf("interval %v: call %d: %v", interval, i, err)
			}
		}
		cancel()
	}
}

// A call whose context has already ended must not use up a token: it will not make the call it
// was waiting for, so the next live caller would wait for nothing.
func TestRateLimiter_CancelledCallKeepsToken(t *testing.T) {
	r := middleware.NewRateLimiter(time.Hour, 1)
	m := &countingModel{}
	h := callWith(m, middleware.RateLimit(r))
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h(done); err == nil {
		t.Fatal("a call with an ended context succeeded")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := h(ctx); err != nil {
		t.Fatalf("the live call after a cancelled one: %v (the cancelled call used up the token)", err)
	}
	if n := m.calls.Load(); n != 1 {
		t.Fatalf("the model was called %d times, want 1", n)
	}
}

// countingModel answers "ok" at once and counts its calls.
type countingModel struct{ calls atomic.Int32 }

func (m *countingModel) Stream(context.Context, agent.Request) (*agent.Stream, error) {
	m.calls.Add(1)
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: "ok"}}
	ch <- agent.Emit{Event: agent.Finish{Reason: agent.FinishStop}}
	close(ch)
	return agent.NewStream(ch), nil
}
