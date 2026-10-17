package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A session carries the transcript across turns: turn 2's model call sees turn 1's Q&A.
func TestSession_MultiTurnCarriesHistory(t *testing.T) {
	var got Request
	inner := &scriptModel{turns: [][]Emit{textTurn("hello"), textTurn("as I said, hello")}}
	m := &captureModel{inner: inner, got: &got}
	a := mustNew(m, memJournal())

	s, err := a.Session(context.Background(), "conv")
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.Send(context.Background(), UserText("hi"))
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	a1 := res.Message
	if textOf(a1) != "hello" {
		t.Fatalf("answer 1 = %q", textOf(a1))
	}

	res2, err := s.Send(context.Background(), UserText("what did you say?"))
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	a2 := res2.Message
	if textOf(a2) != "as I said, hello" {
		t.Fatalf("answer 2 = %q", textOf(a2))
	}

	// Turn 2's model call must have been seeded with the full prior transcript + new input.
	msgs := got.Messages
	if len(msgs) != 3 {
		t.Fatalf("turn 2 seed had %d messages, want 3 (user, assistant, user): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != RoleUser || textOf(msgs[0]) != "hi" {
		t.Errorf("seed[0] = %+v, want user 'hi'", msgs[0])
	}
	if msgs[1].Role != RoleAssistant || textOf(msgs[1]) != "hello" {
		t.Errorf("seed[1] = %+v, want assistant 'hello'", msgs[1])
	}
	if msgs[2].Role != RoleUser || textOf(msgs[2]) != "what did you say?" {
		t.Errorf("seed[2] = %+v, want user question", msgs[2])
	}

	if s.Turns() != 2 || len(s.History()) != 4 {
		t.Fatalf("turns=%d history=%d, want 2 and 4", s.Turns(), len(s.History()))
	}
}

// A session reloaded from the same store rebuilds the transcript and continues.
func TestSession_DurableReloadContinues(t *testing.T) {
	store := memJournal()
	m := &scriptModel{turns: [][]Emit{textTurn("a1"), textTurn("a2"), textTurn("a3")}}
	a := mustNew(m, store)

	s1, err := a.Session(context.Background(), "conv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Send(context.Background(), UserText("q1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Send(context.Background(), UserText("q2")); err != nil {
		t.Fatal(err)
	}

	// Fresh Session over the SAME store (simulating a restart) rebuilds the transcript.
	s2, err := a.Session(context.Background(), "conv")
	if err != nil {
		t.Fatal(err)
	}
	if s2.Turns() != 2 || len(s2.History()) != 4 {
		t.Fatalf("reloaded turns=%d history=%d, want 2 and 4", s2.Turns(), len(s2.History()))
	}
	h := s2.History()
	if textOf(h[0]) != "q1" || textOf(h[1]) != "a1" || textOf(h[2]) != "q2" || textOf(h[3]) != "a2" {
		t.Fatalf("reconstructed transcript wrong: %q/%q/%q/%q",
			textOf(h[0]), textOf(h[1]), textOf(h[2]), textOf(h[3]))
	}

	res, err := s2.Send(context.Background(), UserText("q3"))
	if err != nil {
		t.Fatalf("turn 3 after reload: %v", err)
	}
	a3 := res.Message
	if textOf(a3) != "a3" || s2.Turns() != 3 {
		t.Fatalf("turn 3 answer=%q turns=%d", textOf(a3), s2.Turns())
	}
}

// A tool call in a turn does not leak into the next turn's transcript (memory is Q&A).
func TestSession_ToolTurnThenPlainHistory(t *testing.T) {
	var got Request
	inner := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("found it"), // turn 1: tool then answer
		textTurn("recalled"), // turn 2
	}}
	m := &captureModel{inner: inner, got: &got}
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	a := mustNew(m, memJournal(), WithTools(tool))

	s, _ := a.Session(context.Background(), "c")
	if _, err := s.Send(context.Background(), UserText("look")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), UserText("again")); err != nil {
		t.Fatal(err)
	}

	// Turn 2's seed is [user "look", assistant "found it", user "again"] — no tool_use /
	// tool_result from turn 1 leaks in.
	msgs := got.Messages
	if len(msgs) != 3 {
		t.Fatalf("turn 2 seed = %d messages, want 3 (no tool leakage): %+v", len(msgs), msgs)
	}
	if textOf(msgs[1]) != "found it" {
		t.Errorf("seed[1] should be the final answer, got %+v", msgs[1])
	}
}

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
	a := mustNew(&replyModel{}, mustJournal(&crashOnce{Store: NewMemStore(), crashName: "turn/0"}))
	_, _ = openSession(t, a, "c1").Send(context.Background(), UserText("first"))

	res, err := openSession(t, a, "c1").Send(context.Background(), UserText("second"))
	var msg Message
	if res != nil {
		msg = res.Message
	}
	if err == nil && msg.Text() != "re: second" {
		t.Fatalf(`Send("second") answered %q: the reply to another message`, msg.Text())
	}
	if !errors.Is(err, ErrConfig) {
		t.Fatalf(`Send("second") = %q, %v; want ErrConfig naming the open turn`, msg.Text(), err)
	}
	// Finishing the open turn with its own message works, and then the new one goes through.
	s := openSession(t, a, "c1")
	if msg, err := answerOf(s.Send(context.Background(), UserText("first"))); err != nil || msg.Text() != "re: first" {
		t.Fatalf(`resend "first" = %q, %v`, msg.Text(), err)
	}
	if msg, err := answerOf(s.Send(context.Background(), UserText("second"))); err != nil || msg.Text() != "re: second" {
		t.Fatalf(`then "second" = %q, %v`, msg.Text(), err)
	}
}

// Two handles on one session, each loaded before the other sent (two workers, or a stale
// handle). Neither message may be answered with the other's reply, and neither turn may vanish
// from the transcript.
func TestSession_TwoWritersLoseNoTurn(t *testing.T) {
	t.Run("SendOnce", func(t *testing.T) {
		a := mustNew(contextModel{}, memJournal())
		s1, s2 := openSession(t, a, "c1"), openSession(t, a, "c1")
		if _, err := s1.SendOnce(context.Background(), "k1", UserText("a")); err != nil {
			t.Fatal(err)
		}
		// The stale handle answers with the conversation as it now stands, "a" included.
		if msg, err := answerOf(s2.SendOnce(context.Background(), "k2", UserText("b"))); err != nil || msg.Text() != "a+b" {
			t.Fatalf("k2 = %q, %v; want a+b", msg.Text(), err)
		}
		if n := openSession(t, a, "c1").Turns(); n != 2 {
			t.Fatalf("transcript holds %d turns after two messages, want 2", n)
		}
	})
	t.Run("Send", func(t *testing.T) {
		a := mustNew(&replyModel{}, memJournal())
		s1, s2 := openSession(t, a, "c1"), openSession(t, a, "c1")
		if _, err := s1.Send(context.Background(), UserText("a")); err != nil {
			t.Fatal(err)
		}
		// The stale handle catches up from the journal and answers its own message.
		res, err := s2.Send(context.Background(), UserText("b"))
		var msg Message
		if res != nil {
			msg = res.Message
		}
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
		a := mustNew(&replyModel{}, memJournal())
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
				res, err := s.SendOnce(context.Background(), key, UserText(text))
				var msg Message
				if res != nil {
					msg = res.Message
				}
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
			if msg, err := answerOf(s.SendOnce(context.Background(), "k", UserText("m"))); err != nil || msg.Text() != "re: m" {
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
	s := openSession(t, mustNew(&replyModel{}, memJournal()), "c1")
	keys := []string{"a/b", "a>b", "a%2Fb", "a%3Eb", "a@b", "@turn/0", "session", strings.Repeat("k", 200), strings.Repeat("k", 201)}
	for _, k := range keys {
		if msg, err := answerOf(s.SendOnce(context.Background(), k, UserText("for "+k))); err != nil || msg.Text() != "re: for "+k {
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

// appendRendezvous holds every turn append (the Insert of a "turn/" record) until two have
// arrived, so two handles that each finished a turn write the transcript at the same moment, from
// the same view of it.
type appendRendezvous struct {
	Store
	mu      sync.Mutex
	arrived int
	both    chan struct{}
}

func (r *appendRendezvous) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
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
	return r.Store.Insert(ctx, runID, name, data)
}

// Two handles finish their turns and record them at the same moment. Two messages are two
// turns (the second takes the next slot), and one message is one turn.
func TestSendOnce_SimultaneousAppends(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		store := &appendRendezvous{Store: NewMemStore(), both: make(chan struct{})}
		a := mustNew(&replyModel{}, mustJournal(store))
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
				if _, err := s.SendOnce(context.Background(), key, UserText(text)); err != nil {
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
	a := mustNew(m, memJournal())
	if s, err := a.Session(context.Background(), "c1/e"); err == nil {
		if _, err := s.Send(context.Background(), UserText("from c1/e")); err != nil {
			t.Fatal(err)
		}
	} else if !errors.Is(err, ErrConfig) {
		t.Fatalf(`Session("c1/e") = %v, want ErrConfig or a session`, err)
	}
	res, err := openSession(t, a, "c1").SendOnce(context.Background(), "t0", UserText("from c1"))
	if err != nil {
		t.Fatal(err)
	}
	msg := res.Message
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
		a := mustNew(&replyModel{}, memJournal())
		s := openSession(t, a, "c1")
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				key := fmt.Sprintf("k%d", i)
				if msg, err := answerOf(s.SendOnce(context.Background(), key, UserText("m"+key))); err != nil || msg.Text() != "re: m"+key {
					t.Errorf("%s = %q, %v", key, msg.Text(), err)
				}
				_, _ = s.History(), s.Turns()
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if msg, err := answerOf(s.Send(context.Background(), UserText("plain"))); err != nil || msg.Text() != "re: plain" {
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
			a := mustNew(
				toolThenContext{},
				memJournal(),
				WithTools(MustFunc("lookup", "look up", func(ctx context.Context, _ struct{}) (string, error) {
					if !cut.Swap(true) {
						cancel() // the process dies inside turn "a"'s tool call
					}
					return "found", ctx.Err()
				}, WithSafety(Safety{ReadOnly: true}))),
			)
			do := func(ctx context.Context, key, text string) (Message, error) {
				s := openSession(t, a, "c1")
				if send == "Send" {
					res, err := s.Send(ctx, UserText(text))
					if err != nil {
						return Message{}, err
					}
					return res.Message, nil
				}
				res2, err := s.SendOnce(ctx, key, UserText(text))
				if err != nil {
					return Message{}, err
				}
				return res2.Message, nil
			}
			if _, err := do(ctx, "ka", "a"); !errors.Is(err, context.Canceled) {
				t.Fatalf(`turn "a": err = %v, want context.Canceled`, err)
			}
			// "b" is answered while "a" is unfinished (a Send turn is open, so "b" comes via SendOnce).
			if msg, err := answerOf(openSession(t, a, "c1").SendOnce(context.Background(), "kb", UserText("b"))); err != nil || msg.Text() != "b" {
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
	a := mustNew(
		contextModel{},
		mustJournal(&crashOnce{Store: NewMemStore(), crashName: sessionFromStep(sessionTurnRunID("c1", 0))}),
	)
	stale := openSession(t, a, "c1")
	if _, err := stale.Send(context.Background(), UserText("a")); !errors.Is(err, errDied) {
		t.Fatalf(`first Send("a"): err = %v, want the crash`, err)
	}
	if msg, err := answerOf(openSession(t, a, "c1").SendOnce(context.Background(), "kb", UserText("b"))); err != nil || msg.Text() != "b" {
		t.Fatalf(`"b" = %q, %v`, msg.Text(), err)
	}
	if msg, err := answerOf(openSession(t, a, "c1").Send(context.Background(), UserText("a"))); err != nil || msg.Text() != "b+a" {
		t.Fatalf(`Send("a") on a fresh handle = %q, %v; want "b+a"`, msg.Text(), err)
	}
	if msg, err := answerOf(stale.Send(context.Background(), UserText("a"))); err != nil || msg.Text() != "b+a" {
		t.Fatalf(`Send("a") on the stale handle = %q, %v; want the recorded "b+a"`, msg.Text(), err)
	}
}

// A journaled starting point that does not match the transcript is refused, not guessed at.
func TestSession_BadJournaledStartIsProtocolError(t *testing.T) {
	for _, from := range []turnFrom{{Turns: 0, Digest: "x"}, {Turns: -1}, {Turns: 5}, {Turns: 1, Digest: ""}} {
		store := memJournal()
		b, _ := json.Marshal(from)
		if _, err := store.do(context.Background(), sessionJournalID("c1"), sessionFromStep(sessionTurnRunID("c1", 0)), func(context.Context) (Record, error) {
			return Record{Kind: StepValue, Result: b}, nil
		}); err != nil {
			t.Fatal(err)
		}
		a := mustNew(contextModel{}, store)
		if _, err := openSession(t, a, "c1").SendOnce(context.Background(), "kb", UserText("b")); err != nil {
			t.Fatal(err) // one recorded turn, so the transcript's digest after it is not empty
		}
		if _, err := openSession(t, a, "c1").Send(context.Background(), UserText("a")); !errors.Is(err, ErrProtocol) {
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
	store := memJournal()
	a := mustNew(contextModel{}, store)
	for _, k := range []string{"k0", "k1"} {
		if _, err := openSession(t, a, "c1").SendOnce(context.Background(), k, UserText(k)); err != nil {
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
	if _, err := store.do(context.Background(), sessionJournalID("c1"), sessionFromStep(sessionTurnRunID("c1", 0)), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: b}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := openSession(t, a, "c1").Send(context.Background(), UserText("a")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}

// failFirstModel fails its first call and answers "re: <latest user message>" after that.
type failFirstModel struct{ calls atomic.Int32 }

func (m *failFirstModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	if m.calls.Add(1) == 1 {
		return nil, errors.New("model unavailable")
	}
	return (&replyModel{}).Stream(ctx, req)
}

// S1 (model 12): a handle whose Send failed keeps the turn open in its own view. Another handle
// finishes that turn; the first handle then refuses every new message as if the turn were still
// open, and never reloads. The godoc says a handle that finds the journal moved on reloads it.
func TestModel12_S1_StaleOpenTurnRefusesNextMessage(t *testing.T) {
	ctx := context.Background()
	a := mustNew(&failFirstModel{}, memJournal())
	h1 := openSession(t, a, "c1")
	if _, err := h1.Send(ctx, UserText("x")); err == nil {
		t.Fatal(`first Send("x") succeeded; the test needs it to fail`)
	}
	// A second worker (or a restarted process) gets "x" again and finishes its turn.
	h2 := openSession(t, a, "c1")
	if msg, err := answerOf(h2.Send(ctx, UserText("x"))); err != nil || msg.Text() != "re: x" {
		t.Fatalf(`h2 Send("x") = %q, %v`, msg.Text(), err)
	}
	// "x" is answered and recorded. The next message must go through on either handle.
	for i := 0; i < 3; i++ {
		res, err := h1.Send(ctx, UserText("y"))
		var msg Message
		if res != nil {
			msg = res.Message
		}
		if err == nil {
			if msg.Text() != "re: y" {
				t.Fatalf(`h1 Send("y") = %q`, msg.Text())
			}
			return
		}
		if i == 2 {
			t.Fatalf(`h1 Send("y") refused on every try, though turn "x" is recorded: %v`, err)
		}
	}
}

// loadGate blocks the n-th read of one run's whole history (a Load from its start) until
// released.
type loadGate struct {
	Store
	runID   string
	mu      sync.Mutex
	loads   int
	n       int
	arrived chan struct{}
	release chan struct{}
}

func (g *loadGate) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if runID == g.runID && after < 0 {
		g.mu.Lock()
		g.loads++
		block := g.loads == g.n
		g.mu.Unlock()
		if block {
			close(g.arrived)
			<-g.release
		}
	}
	return g.Store.Load(ctx, runID, after)
}

// blockFirstModel blocks its first call until released, then answers as replyModel does.
type blockFirstModel struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (m *blockFirstModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	if m.calls.Add(1) == 1 {
		close(m.entered)
		<-m.release
	}
	return (&replyModel{}).Stream(ctx, req)
}

// S2 (model 12): two callers share one handle and send the same message (a redelivery, or a
// retry while the first is still running). Both drive the turn's one run. The first records the
// turn and reloads the handle; the second's append then starts past the recorded slot, so the
// same run's turn is recorded twice. The godoc of appendTurn says no turn is recorded twice.
func TestModel12_S2_SharedHandleRecordsATurnTwice(t *testing.T) {
	for _, once := range []bool{false, true} {
		name := "Send"
		runID := sessionTurnRunID("c1", 0)
		if once {
			name, runID = "SendOnce", sessionEventRunID("c1", "k")
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			// The third Load of the run is the second caller's: the first caller's drive loads it
			// twice (its open, and again once it has written run:start).
			gate := &loadGate{Store: NewMemStore(), runID: runID, n: 3, arrived: make(chan struct{}), release: make(chan struct{})}
			model := &blockFirstModel{entered: make(chan struct{}), release: make(chan struct{})}
			a := mustNew(model, mustJournal(gate))
			h := openSession(t, a, "c1")
			send := func() (Message, error) {
				if once {
					res, err := h.SendOnce(ctx, "k", UserText("x"))
					if err != nil {
						return Message{}, err
					}
					return res.Message, nil
				}
				res2, err := h.Send(ctx, UserText("x"))
				if err != nil {
					return Message{}, err
				}
				return res2.Message, nil
			}
			wait := func(ch chan struct{}, what string) {
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatalf("timed out waiting for %s", what)
				}
			}
			aDone, bDone := make(chan error, 1), make(chan error, 1)
			go func() { _, err := send(); aDone <- err }()
			wait(model.entered, "the first caller's model call")
			go func() { _, err := send(); bDone <- err }()
			wait(gate.arrived, "the second caller's load of the turn run")
			close(model.release)
			if err := <-aDone; err != nil {
				t.Fatalf("first caller: %v", err)
			}
			close(gate.release)
			if err := <-bDone; err != nil {
				t.Fatalf("second caller: %v", err)
			}
			if n := openSession(t, a, "c1").Turns(); n != 1 {
				t.Fatalf("one message, one run: the transcript holds %d turns, want 1 (history %v)", n, texts(openSession(t, a, "c1").History()))
			}
		})
	}
}

func texts(ms []Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Text()
	}
	return out
}

type m12drv struct{}

// turnstileModel calls "lookup" (a new call id each turn) for ever, reporting one token per
// call, and lets two drivers ("A", "B") call it strictly in turn: B first loads the run, then A,
// B, A, B... A driver hands the turn over when it enters its next call (its previous record has
// landed by then) or when its Send returns (done).
type turnstileModel struct {
	mu     sync.Mutex
	cond   *sync.Cond
	turn   string
	done   map[string]bool
	calls  map[string]int
	bReady bool
}

func newTurnstile() *turnstileModel {
	m := &turnstileModel{done: map[string]bool{}, calls: map[string]int{}}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func other(d string) string {
	if d == "A" {
		return "B"
	}
	return "A"
}

func (m *turnstileModel) finish(d string) {
	m.mu.Lock()
	m.done[d] = true
	m.turn = other(d)
	m.cond.Broadcast()
	m.mu.Unlock()
}

func (m *turnstileModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	d, _ := ctx.Value(m12drv{}).(string)
	m.mu.Lock()
	if m.calls[d] > 0 && !m.done[other(d)] {
		m.turn = other(d) // this driver's previous record landed: the other goes next
		m.cond.Broadcast()
	}
	if d == "B" && !m.bReady {
		m.bReady = true // B has loaded the run: A may make its first call
		m.turn = "A"
		m.cond.Broadcast()
	}
	for m.turn != d {
		m.cond.Wait()
	}
	m.calls[d]++
	m.mu.Unlock()
	n := 0
	for _, msg := range req.Messages {
		if msg.Role == RoleTool {
			n++
		}
	}
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: fmt.Sprintf("c%d", n), Name: "lookup", ArgsFragment: []byte(`{}`)}}
	ch <- Emit{Event: Finish{Reason: "tool_use", Usage: Usage{InputTokens: 1}}}
	close(ch)
	return NewStream(ch), nil
}

// leasedProc is one process's handle on a shared MemStore that, unlike procStore, has the store's
// leases, as two processes of a store with a Leaser (store/sqlite, store/postgres). inserted, when
// set, is called after each Insert lands; acquired, after each AcquireLease that grants a lease.
type leasedProc struct {
	Store
	Leaser
	inserted func(runID, name string)
	acquired func(runID string)
}

func (p leasedProc) AcquireLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	ok, err := p.Leaser.AcquireLease(ctx, runID, holder, ttl)
	if ok && err == nil && p.acquired != nil {
		p.acquired(runID)
	}
	return ok, err
}

func (p leasedProc) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	e, ok, err := p.Store.Insert(ctx, runID, name, data)
	if err == nil && p.inserted != nil {
		p.inserted(runID, name)
	}
	return e, ok, err
}

// S4 (model 12): a session turn's run has no lease, so two workers given one message both drive
// it. Each counts the journal's spend when it loads and then its own calls and the records it
// adopts; neither sees the other's later calls. With a budget of 6 tokens (one per call) the
// turn makes 12 model calls, twice the budget, against the godoc's bound of max plus the calls in
// flight when it was reached. The two workers are two processes over a store with leases.
func TestModel12_S4_TwoWorkersOvershootATurnBudget(t *testing.T) {
	const budget = 6
	m := newTurnstile()
	shared := NewMemStore()
	tool := lookupTool(func() {})
	started := make(chan struct{})
	var once sync.Once
	journal := func(acquired func(runID string)) *Journal { // one process's journal over the shared store
		j, err := NewJournal(leasedProc{Store: shared, Leaser: shared, acquired: acquired})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	// B starts once A holds the lease on the turn's run: A drives the turn, and B, refused the
	// lease, does not. Waiting on the lease itself, not on A's start/0 record, which A writes before
	// it takes the lease, so B cannot take the lease first however the two are scheduled.
	aA := mustNew(m, journal(func(runID string) {
		if IsSessionRun(runID) && runID != sessionJournalID("c1") {
			once.Do(func() { close(started) })
		}
	}), WithTools(tool), WithTokenBudget(budget))
	aB := mustNew(m, journal(nil), WithTools(tool), WithTokenBudget(budget))
	hA, hB := openSession(t, aA, "c1"), openSession(t, aB, "c1")
	ctxA := context.WithValue(context.Background(), m12drv{}, "A")
	ctxB := context.WithValue(context.Background(), m12drv{}, "B")
	var wg sync.WaitGroup
	wg.Add(2)
	var errA, errB error
	go func() { defer wg.Done(); _, errA = hA.Send(ctxA, UserText("x")); m.finish("A") }()
	// A failsafe only: the test never depends on a wall-clock wait, and a deadlock fails with the
	// state below rather than hanging the suite.
	select {
	case <-started:
	case <-time.After(time.Minute):
		t.Fatal("timed out waiting for A to take the turn's lease")
	}
	go func() { defer wg.Done(); _, errB = hB.Send(ctxB, UserText("x")); m.finish("B") }()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Minute):
		m.mu.Lock()
		t.Fatalf("timed out: turn %q, calls %v, done %v, bReady %v", m.turn, m.calls, m.done, m.bReady)
	}
	t.Logf("A: %v; B: %v", errA, errB)
	total := m.calls["A"] + m.calls["B"]
	t.Logf("model calls: A %d, B %d", m.calls["A"], m.calls["B"])
	if total > budget+2 {
		t.Fatalf("one turn with a budget of %d tokens made %d model calls of 1 token (A %d, B %d): over max plus the 2 calls the two drivers can have in flight",
			budget, total, m.calls["A"], m.calls["B"])
	}
}

// S4's rule: a worker that cannot take the lease on a turn's run does not drive the turn. h1's
// Send holds the lease while its model call is held; h2's Send of the same message must come back
// at once without calling the model, and once h1's turn is recorded, h2's next Send of the message
// returns the recorded answer without driving it again.
func TestSession_TurnLeaseExcludesASecondDriver(t *testing.T) {
	ctx := context.Background()
	model := &blockFirstModel{entered: make(chan struct{}), release: make(chan struct{})}
	var released sync.Once
	release := func() { released.Do(func() { close(model.release) }) }
	t.Cleanup(release)
	shared := NewMemStore()
	proc := func() *Agent { // one process over the shared store, as two workers are
		j, err := NewJournal(leasedProc{Store: shared, Leaser: shared})
		if err != nil {
			t.Fatal(err)
		}
		return mustNew(model, j)
	}
	a := proc()
	h1, h2 := openSession(t, a, "c1"), openSession(t, proc(), "c1")
	first := make(chan error, 1)
	go func() { _, err := h1.Send(ctx, UserText("x")); first <- err }()
	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for h1's model call")
	}
	_, err := h2.Send(ctx, UserText("x"))
	if !errors.Is(err, ErrTurnContended) {
		t.Fatalf(`h2.Send("x") while h1 holds the turn's lease = %v; want ErrTurnContended`, err)
	}
	if errors.Is(err, ErrConfig) || IsPause(err) {
		t.Fatalf("ErrTurnContended must be in no category and not a pause: %v", err)
	}
	if n := model.calls.Load(); n != 1 {
		t.Fatalf("model calls = %d while h1 holds the lease, want 1 (h2 must not drive)", n)
	}
	release()
	if err := <-first; err != nil {
		t.Fatalf("h1: %v", err)
	}
	res, err := h2.Send(ctx, UserText("x"))
	var msg Message
	if res != nil {
		msg = res.Message
	}
	if err != nil || msg.Text() != "re: x" {
		t.Fatalf(`h2.Send("x") after h1 finished = %q, %v; want the recorded "re: x"`, msg.Text(), err)
	}
	if n := model.calls.Load(); n != 1 {
		t.Fatalf("model calls = %d, want 1: the recorded turn is replayed, not driven again", n)
	}
	if n := openSession(t, a, "c1").Turns(); n != 1 {
		t.Fatalf("turns = %d, want 1", n)
	}
}

// BenchmarkSession_Send measures a first Send turn (one model call, no tools) over a MemStore,
// on a session opened for it: the open, the start claim, the seed, the turn's run, the append and
// the reload, and, with S4's rule, the turn run's lease. Each turn is on a new session, so the
// transcript (which every later turn reloads and is seeded with) does not grow with b.N.
func BenchmarkSession_Send(b *testing.B) {
	ctx := context.Background()
	a := mustNew(&replyModel{}, memJournal())
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		s, err := a.Session(ctx, "bench"+strconv.Itoa(i))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := s.Send(ctx, UserText("m")); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

// Session's options set the turn runs' lease: the holder names the lease a turn is driven under,
// and a non-positive TTL is refused.
func TestSession_LeaseOptions(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	j := mustJournal(store)
	model := &blockFirstModel{entered: make(chan struct{}), release: make(chan struct{})}
	a := mustNew(model, j)
	if _, err := a.Session(ctx, "c1", WithLeaseTTL(0)); !errors.Is(err, ErrConfig) {
		t.Fatalf("Session with a zero lease TTL = %v; want ErrConfig", err)
	}
	s, err := a.Session(ctx, "c1", WithLeaseHolder("worker-7"), WithLeaseTTL(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.SendOnce(ctx, "k", UserText("x")); done <- err }()
	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the turn's model call")
	}
	store.mu.Lock()
	l, ok := store.leases[sessionEventRunID("c1", "k")]
	store.mu.Unlock()
	close(model.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !ok || !strings.HasPrefix(l.holder, "worker-7#") || time.Until(l.expiry) < 50*time.Minute {
		t.Fatalf("the turn run's lease while it ran = %+v (held %v); want holder worker-7#<token> and a TTL of an hour", l, ok)
	}
	store.mu.Lock()
	_, held := store.leases[sessionEventRunID("c1", "k")]
	store.mu.Unlock()
	if held {
		t.Fatal("the turn run's lease is still held after the turn returned")
	}
}

// Model 12's DLoad reads the run's completion before its lease: a finished turn run needs no
// lease. A run that completed while its holder still holds the lease (it has not yet released it,
// or it died before its release and the lease has not lapsed) returns its recorded answer to
// another worker, which records the turn, rather than ErrTurnContended.
func TestSession_FinishedTurnRunNeedsNoLease(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	j := mustJournal(store)
	model := &replyModel{}
	a := mustNew(model, j)
	h1 := openSession(t, a, "c1")
	st, n, err := h1.startTurn(ctx, UserText("x"))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := h1.turnSeed(ctx, st.RunID)
	if err != nil {
		t.Fatal(err)
	}
	// h1 drives the run to completion and stops before it records the turn, still leasing it.
	if _, _, _, err := h1.driveRun(ctx, st.RunID, turnDrive("c1", &n, "", UserText("x"), seed)); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.AcquireLease(ctx, st.RunID, "h1-still-holding", time.Hour); err != nil || !ok {
		t.Fatalf("AcquireLease = %v, %v", ok, err)
	}
	res, err := openSession(t, a, "c1").Send(ctx, UserText("x"))
	var msg Message
	if res != nil {
		msg = res.Message
	}
	if err != nil || msg.Text() != "re: x" {
		t.Fatalf(`Send("x") of a finished turn run another holder leases = %q, %v; want its recorded answer`, msg.Text(), err)
	}
	if n := model.calls.Load(); n != 1 {
		t.Fatalf("model calls = %d, want 1", n)
	}
	if n := openSession(t, a, "c1").Turns(); n != 1 {
		t.Fatalf("turns = %d, want 1", n)
	}
}

// turnDrive is the drive a session's turn makes of its run: session id's turn n (Send) or key
// (SendOnce), answering input after seed.
func turnDrive(id string, n *int, key string, input Message, seed []Message) *driveSpec {
	return &driveSpec{input: &input, seed: seed, kind: RunKindSessionTurn, session: &SessionRef{ID: id, Turn: n, Key: key}, strictSaga: true}
}

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

// A session's turns run in journals of their own, and a root run ID may contain '/'. So a session
// turn's run ID must not be one a caller can pass to Run: on main, session "chat" ran its first
// Send turn as run "chat/t0" and its SendOnce turn for key "k1" as run "chat/e/k1", both valid
// root run IDs. Whichever finished first, the other was handed its recorded answer without a
// model call: a reply to a different message, from a different conversation.
func TestSessionTurn_RootRunCannotShareItsJournal(t *testing.T) {
	ctx := context.Background()
	send := func(s *Session) (Message, error) {
		res, err := s.Send(ctx, UserText("hello"))
		if err != nil {
			return Message{}, err
		}
		return res.Message, nil
	}
	sendOnce := func(s *Session) (Message, error) {
		res, err := s.SendOnce(ctx, "k1", UserText("hello"))
		if err != nil {
			return Message{}, err
		}
		return res.Message, nil
	}
	for _, tc := range []struct {
		name  string
		runID string // the root run ID that named the turn's journal on main
		turn  func(*Session) (Message, error)
	}{
		{"Send", "chat/t0", send},
		{"SendOnce", "chat/e/k1", sendOnce},
	} {
		t.Run(tc.name+"/run first", func(t *testing.T) {
			a := mustNew(&replyModel{}, memJournal())
			if out, err := answerOf(a.Run(ctx, tc.runID, UserText("wire the money"))); err != nil || out.Text() != "re: wire the money" {
				t.Fatalf("Run(%q) = %q, %v", tc.runID, out.Text(), err)
			}
			msg, err := tc.turn(openSession(t, a, "chat"))
			if err != nil || msg.Text() != "re: hello" {
				t.Fatalf("session turn = %q, %v; want %q, not the reply of root run %q", msg.Text(), err, "re: hello", tc.runID)
			}
		})
		t.Run(tc.name+"/session first", func(t *testing.T) {
			a := mustNew(&replyModel{}, memJournal())
			if msg, err := tc.turn(openSession(t, a, "chat")); err != nil || msg.Text() != "re: hello" {
				t.Fatalf("session turn = %q, %v", msg.Text(), err)
			}
			res, err := a.Run(ctx, tc.runID, UserText("wire the money"))
			var out Message
			if res != nil {
				out = res.Message
			}
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
	store := memJournal()
	a := mustNew(&replyModel{}, store)
	if msg, err := answerOf(openSession(t, a, "chat").Send(ctx, UserText("hello"))); err != nil || msg.Text() != "re: hello" {
		t.Fatalf("Send = %q, %v", msg.Text(), err)
	}
	if _, ok, err := RecordedStart(ctx, store, "chat"); err != nil || ok {
		t.Fatalf("RecordedStart(chat) = %v, %v before any run; want none", ok, err)
	}
	if out, err := answerOf(a.Run(ctx, "chat", UserText("wire the money"))); err != nil || out.Text() != "re: wire the money" {
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
	store := memJournal()
	a := mustNew(&replyModel{}, store)
	s := openSession(t, a, "chat")
	if _, err := s.Send(ctx, UserText("hello")); err != nil {
		t.Fatal(err)
	}
	// The second turn's run starts and dies before it finishes.
	failing := mustNew(failingModel{}, store)
	if _, err := openSession(t, failing, "chat").Send(ctx, UserText("again")); err == nil {
		t.Fatal("second turn finished; want it left unfinished")
	}
	if _, err := openSession(t, failing, "chat").SendOnce(ctx, "k1", UserText("keyed")); err == nil {
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
	if msg, err := answerOf(s.Send(ctx, UserText("again"))); err != nil || msg.Text() != "re: again" || s.Turns() != 2 {
		t.Fatalf("resend = %q, %v, turns %d", msg.Text(), err, s.Turns())
	}
	if msg, err := answerOf(s.SendOnce(ctx, "k1", UserText("keyed"))); err != nil || msg.Text() != "re: keyed" || s.Turns() != 3 {
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
	a := mustNew(&replyModel{}, memJournal())
	if _, err := a.Run(ctx, sessionTurnRunID("chat", 0), UserText("hi")); !errors.Is(err, ErrConfig) {
		t.Fatalf("Run(session turn run) = %v, want ErrConfig", err)
	}
}

// A session whose journal holds a turn or turn start that does not decode is not opened (ErrProtocol,
// naming the session): rebuilding a transcript from a corrupt record could answer the wrong turn.
func TestSession_CorruptJournalRefusesToOpen(t *testing.T) {
	ctx := context.Background()
	for _, step := range []string{sessionTurnStep(0), sessionStartStep(0)} {
		t.Run(step, func(t *testing.T) {
			j := memJournal()
			writeRaw(t, j, sessionJournalID("c1"), step, Record{Kind: StepValue, Result: []byte(`[1]`)})
			a := mustNew(&replyModel{}, j)
			if _, err := a.Session(ctx, "c1"); !errors.Is(err, ErrProtocol) {
				t.Fatalf("Session over a corrupt %s = %v; want ErrProtocol", step, err)
			}
		})
	}
}

// A session whose transcript cannot be loaded is a storage error, not an empty session.
func TestSession_LoadFailureIsStorageError(t *testing.T) {
	a := mustNew(&replyModel{}, mustJournal(loadFails{NewMemStore()}))
	if _, err := a.Session(context.Background(), "c1"); !errors.Is(err, ErrStorage) {
		t.Fatalf("Session = %v; want ErrStorage", err)
	}
}

// Send and SendOnce refuse a nil run option, and SendOnce an empty key, before the model is called
// or anything is journaled for the turn.
func TestSession_SendRefusals(t *testing.T) {
	ctx := context.Background()
	m := &replyModel{}
	j := memJournal()
	s := openSession(t, mustNew(m, j), "c1")
	if _, err := s.Send(ctx, UserText("hi"), nil); !errors.Is(err, ErrConfig) {
		t.Errorf("Send with a nil option = %v; want ErrConfig", err)
	}
	if _, err := s.SendOnce(ctx, "k", UserText("hi"), nil); !errors.Is(err, ErrConfig) {
		t.Errorf("SendOnce with a nil option = %v; want ErrConfig", err)
	}
	if _, err := s.SendOnce(ctx, "", UserText("hi")); !errors.Is(err, ErrConfig) {
		t.Errorf("SendOnce with an empty key = %v; want ErrConfig", err)
	}
	if n := m.calls.Load(); n != 0 {
		t.Errorf("the model was called %d times by refused sends", n)
	}
	if _, ok, _ := j.Get(ctx, sessionJournalID("c1"), sessionStartStep(0)); ok {
		t.Error("a refused send recorded a turn start")
	}
}

// Redelivering a SendOnce key answers from the recorded turn and does not call the model again,
// and the session reopened by another handle sees the same keyed turn.
func TestSession_SendOnceRedeliveryAnswersFromTheJournal(t *testing.T) {
	ctx := context.Background()
	m := &replyModel{}
	a := mustNew(m, memJournal())
	s := openSession(t, a, "c1")
	first, err := s.SendOnce(ctx, "k1", UserText("hi"))
	if err != nil || first.Message.Text() != "re: hi" {
		t.Fatalf("SendOnce = %v, %v", first, err)
	}
	for _, h := range []*Session{s, openSession(t, a, "c1")} {
		again, err := h.SendOnce(ctx, "k1", UserText("hi"))
		if err != nil || again.Message.Text() != "re: hi" {
			t.Fatalf("redelivery = %v, %v", again, err)
		}
	}
	if n := m.calls.Load(); n != 1 {
		t.Fatalf("model called %d times for one key; want 1", n)
	}
}

// covToggleLoad fails Load while armed.
type covToggleLoad struct {
	Store
	armed atomic.Bool
}

func (s *covToggleLoad) Unwrap() Store { return s.Store }
func (s *covToggleLoad) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if s.armed.Load() {
		return func(yield func(Entry, error) bool) { yield(Entry{}, errCovBlip) }
	}
	return s.Store.Load(ctx, runID, after)
}

// A SendOnce whose key is not in the handle's view reloads the transcript first; when that fails the
// send is a storage error that names the turn's run, the model is not called, and the same key
// succeeds once the store is back.
func TestSession_SendOnceReloadFailure(t *testing.T) {
	ctx := context.Background()
	st := &covToggleLoad{Store: NewMemStore()}
	m := &replyModel{}
	s := openSession(t, mustNew(m, mustJournal(st)), "c1")
	st.armed.Store(true)
	res, err := s.SendOnce(ctx, "k1", UserText("hi"))
	if !errors.Is(err, ErrStorage) || res == nil || res.RunID != sessionEventRunID("c1", "k1") {
		t.Fatalf("SendOnce = %+v, %v; want ErrStorage and the turn's run ID", res, err)
	}
	if m.calls.Load() != 0 {
		t.Fatal("the model was called on a session whose transcript could not be loaded")
	}
	st.armed.Store(false)
	if res, err := s.SendOnce(ctx, "k1", UserText("hi")); err != nil || res.Message.Text() != "re: hi" {
		t.Fatalf("SendOnce after recovery = %+v, %v", res, err)
	}
}

// A Send on a handle that fell behind another writer catches up from the journal and still gets its
// own turn: the other writer's turn is kept, the model sees it, and the stale handle's message is
// answered with its own reply.
func TestSession_StaleHandleCatchesUp(t *testing.T) {
	ctx := context.Background()
	a := mustNew(&replyModel{}, memJournal())
	s1, s2 := openSession(t, a, "c1"), openSession(t, a, "c1")
	if res, err := s1.Send(ctx, UserText("one")); err != nil || res.Message.Text() != "re: one" {
		t.Fatalf("s1 = %+v, %v", res, err)
	}
	res, err := s2.Send(ctx, UserText("two"))
	if err != nil || res.Message.Text() != "re: two" {
		t.Fatalf("stale s2 = %+v, %v; want its own reply", res, err)
	}
	if got := s2.Turns(); got != 2 {
		t.Fatalf("s2 has %d turns; want 2 (the other writer's and its own)", got)
	}
	if got := openSession(t, a, "c1").Turns(); got != 2 {
		t.Fatalf("a fresh handle sees %d turns; want 2", got)
	}
}
