package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
