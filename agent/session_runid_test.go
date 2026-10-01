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
	n, err := Recover(ctx, store, func(_ context.Context, runID string, _ RunStart) error {
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

// Every run ID a session derives is one no caller can pass to Run, and no two sessions, turns,
// keys or sub-agents derive the same one: the session ID ends at the first '>', and the segment
// after it is a session's mark, which no encoded call ID starts with.
func TestSessionRunIDs_Unambiguous(t *testing.T) {
	ctx := context.Background()
	seen := map[string]string{}
	add := func(id, what string) {
		t.Helper()
		if other, dup := seen[id]; dup {
			t.Fatalf("%s and %s share run ID %q", what, other, id)
		}
		seen[id] = what
	}
	for _, sess := range []string{"chat", "chat/t0", "c/e", "c", "a/b", "@x", "session"} {
		ids := []string{sessionJournalID(sess)}
		for n := range 3 {
			ids = append(ids, sessionTurnRunID(sess, n))
		}
		for _, key := range []string{"t0", "k1", "a/b", "a>b", "a%3Eb", "@turn/0", "session", "", "0"} {
			ids = append(ids, sessionEventRunID(sess, key))
		}
		for _, id := range ids {
			add(id, "session "+sess+" run")
			if !IsSessionRun(id) || IsSubRun(id) {
				t.Fatalf("%q: IsSessionRun %v, IsSubRun %v; want true, false", id, IsSessionRun(id), IsSubRun(id))
			}
			if err := checkRunID(ctx, id); !errors.Is(err, ErrConfig) {
				t.Fatalf("checkRunID(%q) = %v outside its session, want ErrConfig", id, err)
			}
			if err := checkRunID(withSessionRun(ctx, id), id); err != nil {
				t.Fatalf("checkRunID(%q) = %v in its session, want nil", id, err)
			}
			if err := checkRunID(withSessionRun(ctx, ids[0]+"x"), id); !errors.Is(err, ErrConfig) {
				t.Fatalf("checkRunID(%q) = %v under another session run, want ErrConfig", id, err)
			}
			for _, call := range []string{"c1", "@turn/0", "@session"} {
				sub := SubRunID(id, call)
				add(sub, "sub-run "+call+" of "+id)
				if !IsSubRun(sub) || IsSessionRun(sub) {
					t.Fatalf("%q: IsSubRun %v, IsSessionRun %v; want true, false", sub, IsSubRun(sub), IsSessionRun(sub))
				}
			}
		}
		add(sess, "root run "+sess)
		if IsSessionRun(sess) || IsSubRun(sess) {
			t.Fatalf("root run %q reported as derived", sess)
		}
	}
	// A context carrying a session run admits only session run IDs: a sub-run ID smuggled into it
	// is still refused.
	sub := SubRunID("r", "c1")
	if err := checkRunID(withSessionRun(ctx, sub), sub); !errors.Is(err, ErrConfig) {
		t.Fatalf("checkRunID(%q) = %v under withSessionRun, want ErrConfig", sub, err)
	}
	a := New(&replyModel{}, NewMemStore())
	if _, err := a.Run(ctx, sessionTurnRunID("chat", 0), "hi"); !errors.Is(err, ErrConfig) {
		t.Fatalf("Run(session turn run) = %v, want ErrConfig", err)
	}
}
