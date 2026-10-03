package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// turnsModel plays a fixed script: each turn is a list of tool calls (id, name), or, when empty,
// a final text answer.
type turnsModel struct {
	turns [][][2]string
	n     atomic.Int32
}

func (m *turnsModel) Stream(context.Context, Request) (*Stream, error) {
	i := int(m.n.Add(1)) - 1
	ch := make(chan Emit, 8)
	if i < len(m.turns) && len(m.turns[i]) > 0 {
		for k, c := range m.turns[i] {
			ch <- Emit{Event: ToolCallDelta{Index: k, ID: c[0], Name: c[1], ArgsFragment: []byte(`{}`)}}
		}
		ch <- Emit{Event: Finish{Reason: "tool_use"}}
	} else {
		ch <- Emit{Event: TextDelta{Text: "done"}}
		ch <- Emit{Event: Finish{Reason: "stop"}}
	}
	close(ch)
	return NewStream(ch), nil
}

// One tool in a turn asks a human (Interrupt) while its sibling, a side effect, is mid-call.
// A pause is routine: it must not cut the sibling off. The sibling finishes and its outcome is
// recorded, so after the human answers the run completes, with no halt and one send.
func TestParallelTurn_PauseDoesNotCancelASibling(t *testing.T) {
	var sent atomic.Int32
	send := MustFunc("send", "send the email", func(ctx context.Context, _ struct{}) (string, error) {
		select {
		case <-time.After(50 * time.Millisecond): // the provider accepts the email
			sent.Add(1)
			return "sent", nil
		case <-ctx.Done():
			sent.Add(1) // the request already went out
			return "", ctx.Err()
		}
	})
	ask := MustFunc("ask", "ask the user", func(ctx context.Context, _ struct{}) (string, error) {
		return Interrupt[string](ctx, "confirm", "ok to proceed?")
	}, WithSafety(Safety{ReadOnly: true}))
	store := memJournal()
	m := &turnsModel{turns: [][][2]string{{{"a1", "ask"}, {"s1", "send"}}}}
	a := mustNew(m, store, WithTools(ask, send))
	_, err := a.Run(context.Background(), "r1", UserText("go"))
	var intr *InterruptPending
	if !errors.As(err, &intr) {
		t.Fatalf("first run: %v, want *InterruptPending", err)
	}
	if err := store.AnswerInterrupt(context.Background(), "r1", "confirm", "yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r1", UserText("go")); err != nil {
		t.Fatalf("after the human answered: %v (the sibling's send was cut off by the pause)", err)
	}
	if n := sent.Load(); n != 1 {
		t.Fatalf("sent %d emails, want 1", n)
	}
}

// In a saga, a failing tool aborts the transaction even if a sibling paused first: the run
// rolls back now, rather than returning the pause and discarding the human's answer later.
func TestParallelTurn_SagaFailureIsNotMaskedByAPause(t *testing.T) {
	ask := MustFunc("ask", "ask the user", func(ctx context.Context, _ struct{}) (string, error) {
		return Interrupt[string](ctx, "confirm", "ok?")
	}, WithSafety(Safety{ReadOnly: true}))
	fail := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		time.Sleep(20 * time.Millisecond) // the pause is returned first
		return "", errors.New("no seats")
	})
	m := &turnsModel{turns: [][][2]string{{{"a1", "ask"}, {"b1", "book"}}}}
	_, err := mustNew(m, memJournal(), WithTools(ask, fail)).Run(context.Background(), "r1", UserText("go"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
}
