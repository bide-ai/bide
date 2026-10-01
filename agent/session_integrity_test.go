package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
// to several handles at once is recorded once: one handle drives its turn, the others get
// ErrTurnContended, and their resend returns the recorded answer.
func TestSendOnce_ConcurrentHandles(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		a := New(&replyModel{}, NewMemStore())
		var wg sync.WaitGroup
		var mu sync.Mutex
		var contended []*Session
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
				msg, err := s.SendOnce(context.Background(), key, text)
				if errors.Is(err, ErrTurnContended) {
					// Another handle drives this key's turn now (S4's turn lease): send it again
					// once the others are done.
					mu.Lock()
					contended = append(contended, s)
					mu.Unlock()
					return
				}
				if err != nil || msg.Text() != "re: "+text {
					t.Errorf("%s = %q, %v", key, msg.Text(), err)
				}
			}()
		}
		wg.Wait()
		if !sameKey && len(contended) > 0 {
			t.Errorf("%d handles got ErrTurnContended for keys no other handle sent", len(contended))
		}
		if len(contended) == 8 {
			t.Error("every handle got ErrTurnContended: none drove the turn")
		}
		for _, s := range contended { // the resend returns the turn another handle recorded
			if msg, err := s.SendOnce(context.Background(), "k", "m"); err != nil || msg.Text() != "re: m" {
				t.Errorf("resend after ErrTurnContended = %q, %v", msg.Text(), err)
			}
		}
		want := 8
		if sameKey {
			want = 1
		}
		if n := openSession(t, a, "c1").Turns(); n != want {
			t.Errorf("same key=%v: transcript holds %d turns, want %d", sameKey, n, want)
		}
	}
}

// A SendOnce turn's run ID carries its key encoded (see sessionEventRunID), so any key is
// allowed, and keys that differ, even only in a character the encoding escapes, answer their own
// messages. On main a key with '/' or '>' was refused, since it named another run's journal.
func TestSendOnce_AnyKeyGetsItsOwnTurn(t *testing.T) {
	s := openSession(t, New(&replyModel{}, NewMemStore()), "c1")
	keys := []string{"a/b", "a>b", "a%2Fb", "a%3Eb", "a@b", "@turn/0", "session", strings.Repeat("k", 200), strings.Repeat("k", 201)}
	for _, k := range keys {
		if msg, err := s.SendOnce(context.Background(), k, "for "+k); err != nil || msg.Text() != "re: for "+k {
			t.Fatalf("SendOnce(%q) = %q, %v", k, msg.Text(), err)
		}
	}
	if s.Turns() != len(keys) {
		t.Fatalf("turns = %d, want %d: two keys shared a turn", s.Turns(), len(keys))
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

// A session's turns run under journals named from its id, so two sessions must never derive the
// same name. When turns ran under "<id>/t<n>" and "<id>/e/<key>", session "c1/e" sending its first
// message and session "c1" answering event "t0" both named run "c1/e/t0": the second was handed
// the first's recorded reply, to a different message in a different conversation, without a
// model call. Session IDs may now contain '/', since everything up to a session run ID's first
// '>' is its session's id.
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

// toolThenContext calls "lookup" first, then answers with every user message it was given,
// joined by "+", so a test can see what conversation the turn was answered in.
type toolThenContext struct{}

func (toolThenContext) Stream(ctx context.Context, req Request) (*Stream, error) {
	if req.Messages[len(req.Messages)-1].Role != RoleTool {
		ch := make(chan Emit, 2)
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: []byte(`{}`)}}
		ch <- Emit{Event: Finish{Reason: "tool_use"}}
		close(ch)
		return NewStream(ch), nil
	}
	return contextModel{}.Stream(ctx, req)
}

// A turn interrupted mid-run resumes in the conversation it started in. Message "a" starts a
// turn, makes its first model call and is cut off in its tool call; message "b" is answered
// meanwhile. When "a" resumes, its journaled first call was made without "b", so the rest of the
// turn must not see "b" either: the turn answers "a", not "b+a".
func TestSession_ResumedTurnKeepsItsTranscript(t *testing.T) {
	for _, send := range []string{"Send", "SendOnce"} {
		t.Run(send, func(t *testing.T) {
			var cut atomic.Bool
			ctx, cancel := context.WithCancel(context.Background())
			a := New(toolThenContext{}, NewMemStore(), Func("lookup", "look up", Safety{ReadOnly: true},
				func(ctx context.Context, _ struct{}) (string, error) {
					if !cut.Swap(true) {
						cancel() // the process dies inside turn "a"'s tool call
					}
					return "found", ctx.Err()
				}))
			do := func(ctx context.Context, key, text string) (Message, error) {
				s := openSession(t, a, "c1")
				if send == "Send" {
					return s.Send(ctx, text)
				}
				return s.SendOnce(ctx, key, text)
			}
			if _, err := do(ctx, "ka", "a"); !errors.Is(err, context.Canceled) {
				t.Fatalf(`turn "a": err = %v, want context.Canceled`, err)
			}
			// "b" is answered while "a" is unfinished (a Send turn is open, so "b" comes via SendOnce).
			if msg, err := openSession(t, a, "c1").SendOnce(context.Background(), "kb", "b"); err != nil || msg.Text() != "b" {
				t.Fatalf(`"b" = %q, %v; want "b"`, msg.Text(), err)
			}
			msg, err := do(context.Background(), "ka", "a")
			if err != nil {
				t.Fatal(err)
			}
			if msg.Text() != "a" {
				t.Fatalf(`resumed turn "a" answered %q, want "a": it saw a message that arrived after it started`, msg.Text())
			}
		})
	}
}

// A stale handle resuming a turn whose starting point another handle journaled catches up and
// seeds the turn from that starting point. Here the first handle dies before journaling it,
// "b" is answered, and a second handle runs turn "a" from the one-turn transcript.
func TestSession_StaleHandleUsesJournaledStart(t *testing.T) {
	a := New(contextModel{}, &crashOnce{Durable: NewMemStore(), crashName: sessionFromStep(sessionTurnRunID("c1", 0))})
	stale := openSession(t, a, "c1")
	if _, err := stale.Send(context.Background(), "a"); !errors.Is(err, errDied) {
		t.Fatalf(`first Send("a"): err = %v, want the crash`, err)
	}
	if msg, err := openSession(t, a, "c1").SendOnce(context.Background(), "kb", "b"); err != nil || msg.Text() != "b" {
		t.Fatalf(`"b" = %q, %v`, msg.Text(), err)
	}
	if msg, err := openSession(t, a, "c1").Send(context.Background(), "a"); err != nil || msg.Text() != "b+a" {
		t.Fatalf(`Send("a") on a fresh handle = %q, %v; want "b+a"`, msg.Text(), err)
	}
	if msg, err := stale.Send(context.Background(), "a"); err != nil || msg.Text() != "b+a" {
		t.Fatalf(`Send("a") on the stale handle = %q, %v; want the recorded "b+a"`, msg.Text(), err)
	}
}

// A journaled starting point that does not match the transcript is refused, not guessed at.
func TestSession_BadJournaledStartIsProtocolError(t *testing.T) {
	for _, from := range []turnFrom{{Turns: 0, Digest: "x"}, {Turns: -1}, {Turns: 5}, {Turns: 1, Digest: ""}} {
		store := NewMemStore()
		b, _ := json.Marshal(from)
		if _, err := store.Do(context.Background(), sessionJournalID("c1"), sessionFromStep(sessionTurnRunID("c1", 0)), func(context.Context) (Record, error) {
			return Record{Kind: StepValue, Result: b}, nil
		}); err != nil {
			t.Fatal(err)
		}
		a := New(contextModel{}, store)
		if _, err := openSession(t, a, "c1").SendOnce(context.Background(), "kb", "b"); err != nil {
			t.Fatal(err) // one recorded turn, so the transcript's digest after it is not empty
		}
		if _, err := openSession(t, a, "c1").Send(context.Background(), "a"); !errors.Is(err, ErrProtocol) {
			t.Errorf("start point %+v: err = %v, want ErrProtocol", from, err)
		}
	}
}

// The transcript digest depends on every turn, in order, and on each field's boundaries.
func TestSession_ChainTurnDistinguishes(t *testing.T) {
	d := func(prev, run, claim string) string { return chainTurn(prev, turnRecord{RunID: run, Claim: claim}) }
	seen := map[string]string{}
	for name, got := range map[string]string{
		"base":         d("", "c1/t0", "x"),
		"other prev":   d("p", "c1/t0", "x"),
		"other run":    d("", "c1/t1", "x"),
		"other claim":  d("", "c1/t0", "y"),
		"moved border": d("", "c1/t0x", ""),
		// Pairs that concatenate alike without the length prefix, or without its separator.
		"colon claim": d("", "", ":"),
		"colon run":   d("", ":", ""),
		"short run":   d("", "0", "abcdefgh1a"),
		"long run":    d("", "10abcdefgh", "a"),
	} {
		if other, dup := seen[got]; dup {
			t.Errorf("%s and %s have the same digest", name, other)
		}
		seen[got] = name
	}
}

// The digest covers every earlier turn, not just the latest: a starting point whose digest
// matches the last of two turns but not the first is refused.
func TestSession_JournaledStartCoversEveryTurn(t *testing.T) {
	store := NewMemStore()
	a := New(contextModel{}, store)
	for _, k := range []string{"k0", "k1"} {
		if _, err := openSession(t, a, "c1").SendOnce(context.Background(), k, k); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := store.History(context.Background(), sessionJournalID("c1"))
	if err != nil {
		t.Fatal(err)
	}
	var last turnRecord
	for _, r := range recs {
		if r.Name == sessionTurnStep(1) {
			if err := json.Unmarshal(r.Result, &last); err != nil {
				t.Fatal(err)
			}
		}
	}
	b, _ := json.Marshal(turnFrom{Turns: 2, Digest: chainTurn("", last)})
	if _, err := store.Do(context.Background(), sessionJournalID("c1"), sessionFromStep(sessionTurnRunID("c1", 0)), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: b}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := openSession(t, a, "c1").Send(context.Background(), "a"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}
