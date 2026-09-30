package agent

import (
	"context"
	"errors"
	"testing"
)

// A session's turns run in journals of their own, and a root run ID may contain '/'. So a session
// turn's run ID must not be one a caller can pass to Run: on main, session "chat" ran its first
// Send turn as run "chat/t0" and its SendOnce turn for key "k1" as run "chat/e/k1", both valid
// root run IDs. Whichever finished first, the other was handed its recorded answer without a
// model call: a reply to a different message, from a different conversation.
func TestSessionTurn_RootRunCannotShareItsJournal(t *testing.T) {
	ctx := context.Background()
	send := func(s *Session) (Message, error) { return s.Send(ctx, "hello") }
	sendOnce := func(s *Session) (Message, error) { return s.SendOnce(ctx, "k1", "hello") }
	for _, tc := range []struct {
		name  string
		runID string // the root run ID that named the turn's journal on main
		turn  func(*Session) (Message, error)
	}{
		{"Send", "chat/t0", send},
		{"SendOnce", "chat/e/k1", sendOnce},
	} {
		t.Run(tc.name+"/run first", func(t *testing.T) {
			a := New(&replyModel{}, NewMemStore())
			if out, err := a.Run(ctx, tc.runID, "wire the money"); err != nil || out.Text() != "re: wire the money" {
				t.Fatalf("Run(%q) = %q, %v", tc.runID, out.Text(), err)
			}
			msg, err := tc.turn(openSession(t, a, "chat"))
			if err != nil || msg.Text() != "re: hello" {
				t.Fatalf("session turn = %q, %v; want %q, not the reply of root run %q", msg.Text(), err, "re: hello", tc.runID)
			}
		})
		t.Run(tc.name+"/session first", func(t *testing.T) {
			a := New(&replyModel{}, NewMemStore())
			if msg, err := tc.turn(openSession(t, a, "chat")); err != nil || msg.Text() != "re: hello" {
				t.Fatalf("session turn = %q, %v", msg.Text(), err)
			}
			out, err := a.Run(ctx, tc.runID, "wire the money")
			if err != nil || out.Text() != "re: wire the money" {
				t.Fatalf("Run(%q) = %q, %v; want %q, not the session's reply", tc.runID, out.Text(), err, "re: wire the money")
			}
		})
	}
}

// The session's own journal (its started and completed turns) was keyed by the bare session ID,
// which is also a valid root run ID. Run("chat") wrote into session "chat"'s journal, and Recover
// listed that journal as an unfinished run and handed it to the resume callback, which would drive
// it as an agent run.
func TestSessionJournal_RootRunCannotShareIt(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	a := New(&replyModel{}, store)
	if msg, err := openSession(t, a, "chat").Send(ctx, "hello"); err != nil || msg.Text() != "re: hello" {
		t.Fatalf("Send = %q, %v", msg.Text(), err)
	}
	if _, ok, err := RecordedStart(ctx, store, "chat"); err != nil || ok {
		t.Fatalf("RecordedStart(chat) = %v, %v before any run; want none", ok, err)
	}
	if out, err := a.Run(ctx, "chat", "wire the money"); err != nil || out.Text() != "re: wire the money" {
		t.Fatalf("Run(chat) = %q, %v", out.Text(), err)
	}
	recs, err := store.History(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Name == sessionTurnStep(0) || r.Name == sessionStartStep(0) {
			t.Fatalf("root run %q holds session record %q: the two share a journal", "chat", r.Name)
		}
	}
}

// Recover leaves a session's journal and its turn runs to the session. A turn run is seeded with
// the transcript before its message, which only the session holds, and its answer is recorded
// only by the session, so the next Send (or a redelivered SendOnce) of the same message resumes
// it. On main Recover handed both the session journal "chat" and the unfinished turn run
// "chat/t1" to the resume callback, which would drive them as root runs.
func TestRecover_SkipsSessionRuns(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	a := New(&replyModel{}, store)
	s := openSession(t, a, "chat")
	if _, err := s.Send(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	// The second turn's run starts and dies before it finishes.
	failing := New(failingModel{}, store)
	if _, err := openSession(t, failing, "chat").Send(ctx, "again"); err == nil {
		t.Fatal("second turn finished; want it left unfinished")
	}
	if _, err := openSession(t, failing, "chat").SendOnce(ctx, "k1", "keyed"); err == nil {
		t.Fatal("keyed turn finished; want it left unfinished")
	}
	var resumed []string
	n, err := Recover(ctx, store, func(_ context.Context, runID string) error {
		resumed = append(resumed, runID)
		return nil
	})
	if err != nil || n != 0 || len(resumed) != 0 {
		t.Fatalf("Recover drove %d runs %v, err %v; want none: the session drives its own turns", n, resumed, err)
	}
	// The session resumes its open turn when the message is sent again.
	s = openSession(t, a, "chat")
	if msg, err := s.Send(ctx, "again"); err != nil || msg.Text() != "re: again" || s.Turns() != 2 {
		t.Fatalf("resend = %q, %v, turns %d", msg.Text(), err, s.Turns())
	}
	if msg, err := s.SendOnce(ctx, "k1", "keyed"); err != nil || msg.Text() != "re: keyed" || s.Turns() != 3 {
		t.Fatalf("redeliver = %q, %v, turns %d", msg.Text(), err, s.Turns())
	}
}

// failingModel fails every call, leaving the run it serves unfinished.
type failingModel struct{}

var errModelDown = errors.New("model down")

func (failingModel) Stream(context.Context, Request) (*Stream, error) { return nil, errModelDown }
