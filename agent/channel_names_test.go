package agent

import (
	"context"
	"errors"
	"testing"
)

// receiveOnce runs one retry-safe tool that calls Receive on channel and reports what it got.
func receiveOnce(t *testing.T, store *Journal, runID, channel string) (Received[string], error) {
	t.Helper()
	var got Received[string]
	var recvErr error
	recv := Func("recv", "", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		m, err := Receive[string](ctx, channel)
		if err != nil {
			recvErr = err
			return "", err
		}
		got = m
		return m.Payload, nil
	})
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
		t.Fatalf("Receive(\"orders\") = %+v, %v; want *Awaiting (the message is on \"orders:vip\")", got, err)
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
