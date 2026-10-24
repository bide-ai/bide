package agent

import (
	"context"
	"slices"
	"testing"
)

// sessModel is a conversation model: a user message "read" makes it call read_a (an untrusted source) and
// answer, "write" makes it call write and answer, anything else it answers at once. A turn is told by the
// last user message and whether a tool result follows it.
type sessModel struct{}

func (sessModel) Stream(_ context.Context, req Request) (*Stream, error) {
	var lastUser string
	toolDone := false
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			switch v := p.(type) {
			case Text:
				if m.Role == RoleUser {
					lastUser, toolDone = v.Text, false
				}
			case ToolResult:
				toolDone = true
			}
		}
	}
	ch := make(chan Emit, 2)
	switch {
	case toolDone || (lastUser != "read" && lastUser != "write"):
		ch <- Emit{Event: TextDelta{Text: "answer"}}
		ch <- Emit{Event: Finish{Reason: "stop"}}
	default:
		name := "read_a"
		if lastUser == "write" {
			name = "write"
		}
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "t-" + name, Name: name, ArgsFragment: []byte(`{}`)}}
		ch <- Emit{Event: Finish{Reason: "tool_use"}}
	}
	close(ch)
	return NewStream(ch), nil
}

type sessRig struct {
	t      *testing.T
	j      *Journal
	writes counter
}

func (r *sessRig) session(opts ...Option) *Session {
	r.t.Helper()
	read := MustFunc("read_a", "read", func(context.Context, struct{}) (string, error) { return "text", nil }, WithSafety(Safety{ReadOnly: true}), Untrusted("a-src"))
	s, err := mustNew(sessModel{}, r.j, append([]Option{WithTools(read, r.writes.tool("write"))}, opts...)...).Session(context.Background(), "conv")
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *sessRig) send(s *Session, text string) {
	r.t.Helper()
	if _, err := s.Send(context.Background(), UserText(text)); err != nil {
		r.t.Fatalf("send %q: %v", text, err)
	}
}

func (r *sessRig) taintSources() []string {
	r.t.Helper()
	recs, err := r.j.History(context.Background(), sessionJournalID("conv"))
	if err != nil {
		r.t.Fatal(err)
	}
	_, marks, err := latestSessionTaint(recs)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, m := range marks {
		out = append(out, m.Source)
	}
	return out
}

func TestTaintSession_ATaintedTurnTaintsTheTurnsAfterIt(t *testing.T) {
	r := &sessRig{t: t, j: memJournal()}
	s := r.session(WithTaintPolicy(TaintPolicy{Refuse: true}))
	r.send(s, "write") // untainted: the session has read nothing
	if r.writes.n.Load() != 1 || len(r.taintSources()) != 0 {
		t.Fatalf("turn 1: %d writes, marks %v; want 1 write and no marks", r.writes.n.Load(), r.taintSources())
	}
	r.send(s, "read")
	if got := r.taintSources(); !slices.Equal(got, []string{"a-src"}) {
		t.Fatalf("after a tainted turn the session's marks = %v, want [a-src]", got)
	}
	r.send(s, "write")
	if r.writes.n.Load() != 1 {
		t.Fatalf("a write in the turn after a tainted one ran (%d writes): the answer reached it laundered", r.writes.n.Load())
	}
	start := startOf(t, r.j, sessionTurnRunID("conv", 2))
	if l := start.inheritedMarks(); len(l) != 1 || l[0].Source != "a-src" {
		t.Fatalf("turn 3's inherited marks = %+v, want a-src", l)
	}
}

// Taint is sticky: a clean turn after a tainted one does not clear it.
func TestTaintSession_ACleanTurnDoesNotClearTaint(t *testing.T) {
	r := &sessRig{t: t, j: memJournal()}
	s := r.session(WithTaintPolicy(TaintPolicy{Refuse: true}))
	r.send(s, "read")
	r.send(s, "chat")
	if got := r.taintSources(); !slices.Equal(got, []string{"a-src"}) {
		t.Fatalf("after a clean turn the marks = %v, want [a-src] still", got)
	}
	r.send(s, "write")
	if r.writes.n.Load() != 0 {
		t.Fatalf("a write after a tainted turn and a clean turn ran (%d)", r.writes.n.Load())
	}
}

// A handle opened later, or an agent with no policy, reads the same marks: they are the session's.
func TestTaintSession_TheMarksAreTheSessionsOnAnyHandle(t *testing.T) {
	r := &sessRig{t: t, j: memJournal()}
	r.send(r.session(WithTaintPolicy(TaintPolicy{Refuse: true})), "read")
	later := r.session(WithTaintPolicy(TaintPolicy{Refuse: true}))
	r.send(later, "write")
	if r.writes.n.Load() != 0 {
		t.Fatalf("the write ran on a second handle of a tainted session (%d)", r.writes.n.Load())
	}
}

// A session that never met a policy records no marks and passes none.
func TestTaintSession_NoPolicyNoMarks(t *testing.T) {
	r := &sessRig{t: t, j: memJournal()}
	s := r.session()
	r.send(s, "read")
	r.send(s, "write")
	recs, _ := r.j.History(context.Background(), sessionJournalID("conv"))
	if n, _, _ := latestSessionTaint(recs); n != -1 {
		t.Fatalf("a taint record was written for a session with no policy (n=%d)", n)
	}
	if r.writes.n.Load() != 1 {
		t.Fatalf("writes = %d, want 1", r.writes.n.Load())
	}
}

// "Latest" is the highest number, parsed: taint/turn/10 is above taint/turn/9 whatever the order of the
// listing, and a name that is not a number is an error, not a guess.
func TestLatestSessionTaint_IsTheHighestNumber(t *testing.T) {
	rec := func(n string, srcs ...string) Record {
		var l []inheritedMark
		for _, s := range srcs {
			l = append(l, inheritedMark{Source: s, Ref: "r"})
		}
		b, _ := marshalJournal(sessionTaintRecord{Marks: l})
		return Record{Kind: StepValue, Name: sessionTaintTurnPrefix + n, Result: b}
	}
	for _, order := range [][]Record{{rec("9", "a"), rec("10", "a", "b")}, {rec("10", "a", "b"), rec("9", "a")}} {
		n, marks, err := latestSessionTaint(order)
		if err != nil || n != 10 || len(marks) != 2 {
			t.Fatalf("latest = %d %v %v, want 10 with two marks (a string order would pick 9)", n, marks, err)
		}
	}
	for _, bad := range []string{"x", "-1", "007", "1.5", ""} {
		if _, _, err := latestSessionTaint([]Record{rec(bad, "a")}); err == nil {
			t.Errorf("taint/turn/%s was accepted", bad)
		}
	}
	if n, marks, err := latestSessionTaint(nil); n != -1 || marks != nil || err != nil {
		t.Fatalf("no records: %d %v %v", n, marks, err)
	}
}

// Two turns that were seeded with the same number of turns both record: the second takes the next number
// and carries the union.
func TestTaintSession_ConcurrentTurnsKeepTheUnion(t *testing.T) {
	ctx := context.Background()
	r := &sessRig{t: t, j: memJournal()}
	s := r.session(WithTaintPolicy(TaintPolicy{Refuse: true}))
	r.send(s, "read") // writes taint/turn/0 with a-src
	// A second turn, seeded as the first was (from 0), that ended with b-src.
	put := func(name string, v any) {
		b, _ := marshalJournal(v)
		if _, _, err := r.j.putNew(ctx, sessionJournalID("conv"), name, Record{Kind: StepValue, Result: b}); err != nil {
			t.Fatal(err)
		}
	}
	put(sessionFromStep("other"), turnFrom{Turns: 0})
	other := mustNew(sessModel{}, r.j, WithTools(MustFunc("read_b", "read", func(context.Context, struct{}) (string, error) { return "t", nil }, WithSafety(Safety{ReadOnly: true}), Untrusted("b-src"))))
	if _, err := other.store.put(ctx, "other", "run:start", Record{Kind: StepValue, Result: []byte(`{"input":"x","ext":{"taint_policy_digest":"d","taint_inherited":[{"source":"b-src","ref":"parent:x"}]}}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.recordTurnTaint(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	recs, _ := r.j.History(ctx, sessionJournalID("conv"))
	n, marks, _ := latestSessionTaint(recs)
	var got []string
	for _, m := range marks {
		got = append(got, m.Source)
	}
	if n != 1 || !slices.Equal(got, []string{"a-src", "b-src"}) {
		t.Fatalf("latest = %d %v, want taint/turn/1 holding [a-src b-src] (the second turn took the next number and kept the union)", n, got)
	}
}
