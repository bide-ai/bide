package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// replyModel answers "re: <the latest user message>" and counts the calls.
type replyModel struct{ calls atomic.Int32 }

func (m *replyModel) Stream(_ context.Context, req Request) (*Stream, error) {
	m.calls.Add(1)
	var last string
	for _, msg := range req.Messages {
		if msg.Role == RoleUser {
			last = msg.Text()
		}
	}
	ch := make(chan Emit, 2)
	ch <- Emit{Event: TextDelta{Text: "re: " + last}}
	ch <- Emit{Event: Finish{Reason: "stop"}}
	close(ch)
	return NewStream(ch), nil
}

// deliver is the messaging guide's conversational handler: reopen the session and answer the
// inbound message once, keyed by its event id.
func deliver(a *Agent, conversation, eventID, text string) (Message, error) {
	sess, err := a.Session(context.Background(), conversation)
	if err != nil {
		return Message{}, err
	}
	res, err := sess.SendOnce(context.Background(), eventID, UserText(text))
	if err != nil {
		return Message{}, err
	}
	return res.Message, nil
}

func turnsOf(t *testing.T, a *Agent, conversation string) int {
	t.Helper()
	sess, err := a.Session(context.Background(), conversation)
	if err != nil {
		t.Fatal(err)
	}
	return sess.Turns()
}

// One inbound message is one turn, whenever the process dies and however often the message is
// redelivered. The old pattern (Send inside an event-keyed Step) opened a second turn when the
// process died after the session recorded the turn and before the Step recorded the reply.
func TestSendOnce_RedeliveryIsOneTurn(t *testing.T) {
	cases := map[string]string{
		"dies before the session records the turn": "turn/0",
		"dies after the turn is recorded":          "", // the caller never replied; the event is redelivered
	}
	for name, crashAt := range cases {
		t.Run(name, func(t *testing.T) {
			m := &replyModel{}
			var store Store = NewMemStore()
			if crashAt != "" {
				store = &crashOnce{Store: store, crashName: crashAt}
			}
			a := mustNew(m, mustJournal(store))
			_, _ = deliver(a, "c1", "evt-1", "hello")
			msg, err := deliver(a, "c1", "evt-1", "hello") // redelivery
			if err != nil || msg.Text() != "re: hello" {
				t.Fatalf("redelivery = %q, %v; want the turn's answer", msg.Text(), err)
			}
			if n, calls := turnsOf(t, a, "c1"), m.calls.Load(); n != 1 || calls != 1 {
				t.Fatalf("one inbound message produced %d turns and %d model calls, want 1 and 1", n, calls)
			}
		})
	}
}

// Distinct messages are distinct turns, in order, and the conversation carries across them.
func TestSendOnce_DistinctKeysAreDistinctTurns(t *testing.T) {
	a := mustNew(&replyModel{}, memJournal())
	for _, ev := range []string{"evt-1", "evt-2", "evt-1"} {
		if _, err := deliver(a, "c1", ev, "msg "+ev); err != nil {
			t.Fatal(err)
		}
	}
	if n := turnsOf(t, a, "c1"); n != 2 {
		t.Fatalf("turns = %d, want 2", n)
	}
}

// A key names one message: reusing it for different text is a caller error, not a new turn.
func TestSendOnce_KeyReuseWithDifferentInput(t *testing.T) {
	a := mustNew(&replyModel{}, memJournal())
	if _, err := deliver(a, "c1", "evt-1", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := deliver(a, "c1", "evt-1", "goodbye"); !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig", err)
	}
}

// A message that arrives while another's turn is unfinished gets its own turn, not the
// unfinished one: each key's turn has its own journal. Here evt-1's turn completes but the
// process dies before the session records it; evt-2 arrives next, then evt-1 is redelivered.
func TestSendOnce_InterleavedMessagesKeepTheirOwnTurns(t *testing.T) {
	m := &replyModel{}
	a := mustNew(m, mustJournal(&crashOnce{Store: NewMemStore(), crashName: "turn/0"}))
	_, _ = deliver(a, "c1", "evt-1", "first")
	second, err := deliver(a, "c1", "evt-2", "second")
	if err != nil || second.Text() != "re: second" {
		t.Fatalf("evt-2 = %q, %v; want its own answer", second.Text(), err)
	}
	first, err := deliver(a, "c1", "evt-1", "first")
	if err != nil || first.Text() != "re: first" {
		t.Fatalf("redelivered evt-1 = %q, %v; want its own answer", first.Text(), err)
	}
	if n, calls := turnsOf(t, a, "c1"), m.calls.Load(); n != 2 || calls != 2 {
		t.Fatalf("turns = %d, model calls = %d; want 2 and 2", n, calls)
	}
}
