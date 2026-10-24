package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// subRig is a parent that may read untrusted text, delegates to a child, and then asks for a write. The
// child requires provenance and has one Trusted tool.
type subRig struct {
	writes  counter
	childJ  *Journal
	parentJ *Journal
}

// run drives the parent. parentPolicy: nil for none. parentReads: the parent reads an Untrusted source
// before it delegates. childReads: the child reads an Untrusted source. delegateOpts: options of the
// delegate tool.
func (r *subRig) run(t *testing.T, parentPolicy *TaintPolicy, parentReads, childReads bool, delegateOpts ...ToolOption) error {
	t.Helper()
	r.childJ, r.parentJ = memJournal(), memJournal()
	childTools := []Tool{provTool("own", Trusted("the host's table"))}
	childTurns := [][]Emit{textTurn("child answer")}
	if childReads {
		childTools = append(childTools, provTool("read_c", Untrusted("c-src")))
		childTurns = [][]Emit{toolTurn("r1", "read_c", `{}`), textTurn("child answer")}
	}
	child := mustNew(&scriptModel{turns: childTurns}, r.childJ, WithTools(childTools...),
		WithTaintPolicy(TaintPolicy{Refuse: true, RequireProvenance: true}))
	tools := []Tool{MustSubAgent("delegate", "delegate", child, delegateOpts...), r.writes.tool("write", Trusted("the host's own write"))}
	turns := [][]Emit{toolTurn("d1", "delegate", `{"task":"go"}`), toolTurn("w1", "write", `{}`), textTurn("done")}
	if parentReads {
		tools = append(tools, provTool("read_p", Untrusted("p-src")))
		turns = append([][]Emit{toolTurn("r0", "read_p", `{}`)}, turns...)
	}
	opts := []Option{WithTools(tools...)}
	if parentPolicy != nil {
		opts = append(opts, WithTaintPolicy(*parentPolicy))
	}
	_, err := mustNew(&scriptModel{turns: turns}, r.parentJ, opts...).Run(context.Background(), "r", UserText("go"), WithTrustedInput("the operator"))
	return err
}

func (r *subRig) childStarted(t *testing.T) bool {
	t.Helper()
	_, ok, err := RecordedStart(context.Background(), r.childJ, SubRunID("r", "d1"))
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// A sub-agent is never asked to state its input's provenance, whatever the parent's taint state: its input is
// its parent's call.
func TestRequireProvenance_ASubAgentIsNeverAsked(t *testing.T) {
	exempt := TaintExempt("delegation is gated inside the child")
	trustedDelegate := Trusted("a sub-agent: its own answer carries a mark when it read outside text")
	policy := &TaintPolicy{Refuse: true, RequireProvenance: true}
	for name, c := range map[string]struct {
		policy       *TaintPolicy
		parentReads  bool
		delegateOpts []ToolOption
	}{
		"parent has no policy":        {nil, false, nil},
		"parent has a policy":         {policy, false, []ToolOption{exempt, trustedDelegate}},
		"parent under taint":          {policy, true, []ToolOption{exempt, trustedDelegate}},
		"parent under taint, no req.": {&TaintPolicy{Refuse: true}, true, []ToolOption{exempt}},
	} {
		t.Run(name, func(t *testing.T) {
			var r subRig
			if err := r.run(t, c.policy, c.parentReads, false, c.delegateOpts...); err != nil {
				t.Fatal(err)
			}
			if !r.childStarted(t) {
				t.Fatal("the sub-agent's run did not start: it was asked to state a provenance")
			}
		})
	}
}

// A static Trusted declaration on a sub-agent tool never suppresses the mark computed from what the child
// read: the parent's next side effect is still refused.
func TestRequireProvenance_ATrustedSubAgentToolStillCarriesTheChildsMark(t *testing.T) {
	var r subRig
	policy := &TaintPolicy{Refuse: true, RequireProvenance: true}
	if err := r.run(t, policy, false, true, TaintExempt("delegation is gated inside the child"), Trusted("a sub-agent: its answer is marked by what it read")); err != nil {
		t.Fatal(err)
	}
	if got := r.writes.n.Load(); got != 0 {
		t.Fatalf("the parent's write ran %d times after a tainted child's answer: a Trusted declaration suppressed the mark", got)
	}
	recs, _ := r.parentJ.History(context.Background(), "r")
	var found bool
	for _, rec := range recs {
		if rec.Kind == StepToolResult && rec.ToolUseID == "d1" {
			found = rec.Safety != nil && rec.Safety.Untrusted == "subagent:delegate"
		}
	}
	if !found {
		t.Fatal("the delegate's tool_result does not carry the mark subagent:delegate")
	}
	// The same run with a child that read nothing: the write runs.
	var clean subRig
	if err := clean.run(t, policy, false, false, TaintExempt("delegation is gated inside the child"), Trusted("a sub-agent: its answer is marked by what it read")); err != nil {
		t.Fatal(err)
	}
	if got := clean.writes.n.Load(); got != 1 {
		t.Fatalf("with a clean child the write ran %d times, want 1", got)
	}
}

// The rule for sessions: every new turn (Send, or a SendOnce key's first drive) states its input; a replay of
// a SendOnce key whose turn finished runs nothing and is not asked.
func TestRequireProvenance_SessionRules(t *testing.T) {
	ctx := context.Background()
	a, err := New(&scriptModel{turns: [][]Emit{textTurn("one"), textTurn("two"), textTurn("three"), textTurn("four")}}, memJournal(),
		WithTools(provTool("a", Trusted("mine"))), WithTaintPolicy(TaintPolicy{Refuse: true, RequireProvenance: true}))
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.Session(ctx, "conv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendOnce(ctx, "k1", UserText("hello")); !errors.Is(err, ErrConfig) {
		t.Fatalf("a SendOnce that states nothing = %v, want ErrConfig", err)
	}
	if _, err := s.SendOnce(ctx, "k1", UserText("hello"), WithUntrustedInput("chat")); err != nil {
		t.Fatalf("a SendOnce that states its input, after a refused one with the same key = %v", err)
	}
	// Replay: the key's turn finished, so nothing runs and nothing is asked.
	if _, err := s.SendOnce(ctx, "k1", UserText("hello")); err != nil {
		t.Fatalf("a replay of a finished SendOnce key = %v, want its answer", err)
	}
	// A second turn is a new run: it states its input too.
	if _, err := s.Send(ctx, UserText("again")); !errors.Is(err, ErrConfig) {
		t.Fatalf("a second Send that states nothing = %v, want ErrConfig", err)
	}
	if _, err := s.Send(ctx, UserText("again"), WithTrustedInput("the operator")); err != nil {
		t.Fatalf("a second Send that states it = %v", err)
	}
}

// A retrieval layer is not a tool: RetrievalUntrusted marks its documents and needs no Trusted, and the run
// that uses it still states its input.
func TestRequireProvenance_ARetrievalLayerNeedsNoToolDeclaration(t *testing.T) {
	ctx := context.Background()
	ret := RetrieverFunc(func(context.Context, string, int) ([]Doc, error) { return []Doc{{ID: "d1", Text: "doc"}}, nil })
	a, err := New(&scriptModel{turns: [][]Emit{textTurn("done"), textTurn("done")}}, memJournal(), WithTools(provTool("a", Trusted("mine"))),
		WithRetrieval(ret, 1, RetrievalUntrusted("kb")), WithTaintPolicy(TaintPolicy{Refuse: true, RequireProvenance: true}))
	if err != nil {
		t.Fatalf("an agent with a retrieval layer under RequireProvenance: %v", err)
	}
	if _, err := a.Run(ctx, "r0", UserText("go")); !errors.Is(err, ErrConfig) {
		t.Fatalf("a run that states nothing = %v, want ErrConfig", err)
	}
	if _, err := a.Run(ctx, "r1", UserText("go"), WithTrustedInput("the operator")); err != nil {
		t.Fatal(err)
	}
	e, ok, err := a.store.getEntry(ctx, "r1", "@retrieval/0")
	if err != nil || !ok || !strings.Contains(string(e.Data), `"untrusted":"kb"`) {
		t.Fatalf("the retrieval record carries no mark: %v %v %s", ok, err, e.Data)
	}
}
