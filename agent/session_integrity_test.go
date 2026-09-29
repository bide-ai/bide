package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func openSession(t *testing.T, a *Agent, id string) *Session {
	t.Helper()
	s, err := a.Session(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A turn belongs to the message that started it. Here "first" completes its run but the process
// dies before the session records the turn. A different message sent next must not be answered
// with the reply to "first": that turn is still open, so Send reports it.
func TestSend_NewMessageDoesNotTakeAnOpenTurn(t *testing.T) {
	a := New(&replyModel{}, &crashOnce{Durable: NewMemStore(), crashName: "turn/0"})
	_, _ = openSession(t, a, "c1").Send(context.Background(), "first")

	msg, err := openSession(t, a, "c1").Send(context.Background(), "second")
	if err == nil && msg.Text() != "re: second" {
		t.Fatalf(`Send("second") answered %q: the reply to another message`, msg.Text())
	}
	if !errors.Is(err, ErrConfig) {
		t.Fatalf(`Send("second") = %q, %v; want ErrConfig naming the open turn`, msg.Text(), err)
	}
	// Finishing the open turn with its own message works, and then the new one goes through.
	s := openSession(t, a, "c1")
	if msg, err := s.Send(context.Background(), "first"); err != nil || msg.Text() != "re: first" {
		t.Fatalf(`resend "first" = %q, %v`, msg.Text(), err)
	}
	if msg, err := s.Send(context.Background(), "second"); err != nil || msg.Text() != "re: second" {
		t.Fatalf(`then "second" = %q, %v`, msg.Text(), err)
	}
}

// Two handles on one session, each loaded before the other sent (two workers, or a stale
// handle). Neither message may be answered with the other's reply, and neither turn may vanish
// from the transcript.
func TestSession_TwoWritersLoseNoTurn(t *testing.T) {
	t.Run("SendOnce", func(t *testing.T) {
		a := New(contextModel{}, NewMemStore())
		s1, s2 := openSession(t, a, "c1"), openSession(t, a, "c1")
		if _, err := s1.SendOnce(context.Background(), "k1", "a"); err != nil {
			t.Fatal(err)
		}
		// The stale handle answers with the conversation as it now stands, "a" included.
		if msg, err := s2.SendOnce(context.Background(), "k2", "b"); err != nil || msg.Text() != "a+b" {
			t.Fatalf("k2 = %q, %v; want a+b", msg.Text(), err)
		}
		if n := openSession(t, a, "c1").Turns(); n != 2 {
			t.Fatalf("transcript holds %d turns after two messages, want 2", n)
		}
	})
	t.Run("Send", func(t *testing.T) {
		a := New(&replyModel{}, NewMemStore())
		s1, s2 := openSession(t, a, "c1"), openSession(t, a, "c1")
		if _, err := s1.Send(context.Background(), "a"); err != nil {
			t.Fatal(err)
		}
		// The stale handle catches up from the journal and answers its own message.
		msg, err := s2.Send(context.Background(), "b")
		if err != nil || msg.Text() != "re: b" {
			t.Fatalf(`stale handle's Send("b") = %q, %v; want its own answer`, msg.Text(), err)
		}
		if n := openSession(t, a, "c1").Turns(); n != 2 {
			t.Fatalf("transcript holds %d turns, want 2", n)
		}
	})
}

// Handles racing on one session: distinct messages are all recorded, and one message delivered
// to several handles at once is recorded once.
func TestSendOnce_ConcurrentHandles(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		a := New(&replyModel{}, NewMemStore())
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				key, text := fmt.Sprintf("k%d", i), fmt.Sprintf("m%d", i)
				if sameKey {
					key, text = "k", "m"
				}
				s, err := a.Session(context.Background(), "c1")
				if err != nil {
					t.Error(err)
					return
				}
				if msg, err := s.SendOnce(context.Background(), key, text); err != nil || msg.Text() != "re: "+text {
					t.Errorf("%s = %q, %v", key, msg.Text(), err)
				}
			}()
		}
		wg.Wait()
		want := 8
		if sameKey {
			want = 1
		}
		if n := openSession(t, a, "c1").Turns(); n != want {
			t.Errorf("same key=%v: transcript holds %d turns, want %d", sameKey, n, want)
		}
	}
}

// A key with '/' could name another run's journal (a sub-agent of a turn runs under
// "<turn run>/<call>"), so it is refused.
func TestSendOnce_RejectsSlashInKey(t *testing.T) {
	if _, err := openSession(t, New(&replyModel{}, NewMemStore()), "c1").SendOnce(context.Background(), "a/b", "x"); !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig", err)
	}
}

// contextModel answers with every user message it was given, joined by "+", so a test can see
// what conversation a turn was answered in.
type contextModel struct{}

func (contextModel) Stream(_ context.Context, req Request) (*Stream, error) {
	var seen []string
	for _, m := range req.Messages {
		if m.Role == RoleUser {
			seen = append(seen, m.Text())
		}
	}
	ch := make(chan Emit, 2)
	ch <- Emit{Event: TextDelta{Text: strings.Join(seen, "+")}}
	ch <- Emit{Event: Finish{Reason: "stop"}}
	close(ch)
	return NewStream(ch), nil
}

// appendRendezvous holds every turn append until two have arrived, so two handles that each
// finished a turn write the transcript at the same moment, from the same view of it.
type appendRendezvous struct {
	Durable
	mu      sync.Mutex
	arrived int
	both    chan struct{}
}

func (r *appendRendezvous) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	if strings.HasPrefix(name, "turn/") {
		r.mu.Lock()
		if r.arrived++; r.arrived == 2 {
			close(r.both)
		}
		r.mu.Unlock()
		select {
		case <-r.both:
		case <-time.After(2 * time.Second):
		}
	}
	return r.Durable.Do(ctx, runID, name, fn)
}

// Two handles finish their turns and record them at the same moment. Two messages are two
// turns (the second takes the next slot), and one message is one turn.
func TestSendOnce_SimultaneousAppends(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		store := &appendRendezvous{Durable: NewMemStore(), both: make(chan struct{})}
		a := New(&replyModel{}, store)
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				key, text := fmt.Sprintf("k%d", i), fmt.Sprintf("m%d", i)
				if sameKey {
					key, text = "k", "m"
				}
				s, err := a.Session(context.Background(), "c1")
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := s.SendOnce(context.Background(), key, text); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		want := 2
		if sameKey {
			want = 1
		}
		if n := openSession(t, a, "c1").Turns(); n != want {
			t.Errorf("same key=%v: transcript holds %d turns, want %d", sameKey, n, want)
		}
	}
}

// A session's turns run under journals named from its id ("<id>/t<n>", "<id>/e/<key>"), so two
// sessions must never derive the same name. Session "c1/e" sending its first message and
// session "c1" answering event "t0" both named run "c1/e/t0": the second was handed the first's
// recorded reply, to a different message in a different conversation, without a model call.
func TestSession_IDsCannotCollide(t *testing.T) {
	m := &replyModel{}
	a := New(m, NewMemStore())
	if s, err := a.Session(context.Background(), "c1/e"); err == nil {
		if _, err := s.Send(context.Background(), "from c1/e"); err != nil {
			t.Fatal(err)
		}
	} else if !errors.Is(err, ErrConfig) {
		t.Fatalf(`Session("c1/e") = %v, want ErrConfig or a session`, err)
	}
	msg, err := openSession(t, a, "c1").SendOnce(context.Background(), "t0", "from c1")
	if err != nil {
		t.Fatal(err)
	}
	if msg.Text() != "re: from c1" {
		t.Fatalf(`session "c1" answered event "t0" with %q, another session's reply`, msg.Text())
	}
}

// One handle shared by concurrent callers (a server that caches the handle per conversation)
// behaves as several handles do: every message is answered with its own reply and recorded once.
// Before the handle guarded its state, this was a data race that could crash the process with
// "concurrent map writes".
func TestSession_OneHandleConcurrentCallers(t *testing.T) {
	for range 20 {
		a := New(&replyModel{}, NewMemStore())
		s := openSession(t, a, "c1")
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				key := fmt.Sprintf("k%d", i)
				if msg, err := s.SendOnce(context.Background(), key, "m"+key); err != nil || msg.Text() != "re: m"+key {
					t.Errorf("%s = %q, %v", key, msg.Text(), err)
				}
				_, _ = s.History(), s.Turns()
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if msg, err := s.Send(context.Background(), "plain"); err != nil || msg.Text() != "re: plain" {
				t.Errorf(`Send("plain") = %q, %v`, msg.Text(), err)
			}
		}()
		wg.Wait()
		if n := openSession(t, a, "c1").Turns(); n != 9 {
			t.Fatalf("transcript holds %d turns after 9 messages, want 9", n)
		}
		if n := s.Turns(); n != 9 {
			t.Fatalf("the shared handle reports %d turns, want 9", n)
		}
	}
}
