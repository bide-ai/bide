package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// RequireProvenance (unexported until the names are decided: TaintPolicy.requireProvenance, the tool option
// trustedOutput, ToolSpec.trusted): every tool declares a provenance and every run states its input's.

func provTool(name string, opts ...ToolOption) Tool {
	return MustFunc(name, name, func(context.Context, struct{}) (string, error) { return "ok", nil }, opts...)
}

func provAgent(t *testing.T, require bool, tools ...Tool) (*Agent, error) {
	t.Helper()
	return New(&scriptModel{turns: [][]Emit{textTurn("done"), textTurn("done"), textTurn("done"), textTurn("done"), textTurn("done"), textTurn("done")}}, memJournal(), WithTools(tools...),
		WithTaintPolicy(TaintPolicy{Refuse: true, requireProvenance: require}))
}

func TestRequireProvenance_EveryToolDeclaresOne(t *testing.T) {
	declared := []Tool{provTool("a", Untrusted("crm")), provTool("b", trustedOutput("our own table"))}
	if _, err := provAgent(t, true, declared...); err != nil {
		t.Fatalf("tools that declare a provenance were refused: %v", err)
	}
	_, err := provAgent(t, true, provTool("a", Untrusted("crm")), provTool("plain"))
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), `"plain"`) {
		t.Fatalf("a tool with no provenance = %v, want ErrConfig naming it", err)
	}
	// TaintExempt is about gating, not provenance: it does not stand in for a declaration.
	if _, err := provAgent(t, true, provTool("x", TaintExempt("reviewed"))); !errors.Is(err, ErrConfig) {
		t.Fatalf("an exempt tool with no provenance = %v, want ErrConfig", err)
	}
	// Without the requirement nothing changes.
	if _, err := provAgent(t, false, provTool("plain")); err != nil {
		t.Fatalf("without RequireProvenance: %v", err)
	}
	// Declared both ways is a contradiction, whatever the policy.
	if _, err := provAgent(t, false, provTool("both", Untrusted("crm"), trustedOutput("mine"))); !errors.Is(err, ErrConfig) {
		t.Fatalf("a tool that is both untrusted and trusted = %v, want ErrConfig", err)
	}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("a trusted declaration with no reason was accepted")
			}
		}()
		provTool("empty", trustedOutput(""))
	}()
}

func TestRequireProvenance_EveryRunStatesItsInput(t *testing.T) {
	ctx := context.Background()
	run := func(a *Agent, id string, opts ...RunOption) error {
		_, err := a.Run(ctx, id, UserText("go"), opts...)
		return err
	}
	a, err := provAgent(t, true, provTool("a", trustedOutput("mine")))
	if err != nil {
		t.Fatal(err)
	}
	if err := run(a, "r0"); !errors.Is(err, ErrConfig) {
		t.Fatalf("a run that states no input provenance = %v, want ErrConfig", err)
	}
	if _, ok, _ := RecordedStart(ctx, a.store, "r0"); ok {
		t.Fatal("a refused first drive wrote a run:start")
	}
	for name, opt := range map[string]RunOption{"untrusted": WithUntrustedInput("webhook"), "trusted": WithTrustedInput("the operator")} {
		if err := run(a, "r-"+name, opt); err != nil {
			t.Errorf("a run that states its input (%s) = %v", name, err)
		}
	}
	// Not re-checked on a later drive of a run that started with a statement (a resume carries none).
	b, _ := provAgent(t, true, provTool("a", trustedOutput("mine")))
	if err := run(b, "r1", WithTrustedInput("the operator")); err != nil {
		t.Fatal(err)
	}
	if err := run(b, "r1"); err != nil {
		t.Errorf("a later drive of a started run = %v, want it read back", err)
	}
	// The same agent without the requirement: no statement needed.
	c, _ := provAgent(t, false, provTool("a"))
	if err := run(c, "r2"); err != nil {
		t.Errorf("without RequireProvenance: %v", err)
	}
}

// A session's turns are runs: each Send states its input's provenance too.
func TestRequireProvenance_SessionTurnsStateTheirInput(t *testing.T) {
	ctx := context.Background()
	a, err := New(&scriptModel{turns: [][]Emit{textTurn("one"), textTurn("two")}}, memJournal(), WithTools(provTool("a", trustedOutput("mine"))),
		WithTaintPolicy(TaintPolicy{Refuse: true, requireProvenance: true}))
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.Session(ctx, "conv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, UserText("hello")); !errors.Is(err, ErrConfig) {
		t.Fatalf("a Send that states no provenance = %v, want ErrConfig", err)
	}
	if _, err := s.Send(ctx, UserText("hello"), WithUntrustedInput("chat")); err != nil {
		t.Fatalf("a Send that states it = %v", err)
	}
}

// A sub-agent's input is written by its parent's model, and its run inherits the parent's taint: it is not
// asked to state a provenance of its own.
func TestRequireProvenance_ASubAgentRunIsNotAskedToState(t *testing.T) {
	ctx := context.Background()
	child, err := New(&scriptModel{turns: [][]Emit{textTurn("child answer")}}, memJournal(), WithTools(provTool("a", trustedOutput("mine"))),
		WithTaintPolicy(TaintPolicy{Refuse: true, requireProvenance: true}))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := New(&scriptModel{turns: [][]Emit{toolTurn("d1", "delegate", `{"task":"go"}`), textTurn("done")}}, memJournal(),
		WithTools(MustSubAgent("delegate", "delegate", child, TaintExempt("test"), trustedOutput("a sub-agent: what it read marks its answer by itself")), provTool("a", trustedOutput("mine"))),
		WithTaintPolicy(TaintPolicy{Refuse: true, requireProvenance: true}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parent.Run(ctx, "r", UserText("go"), WithTrustedInput("the operator")); err != nil {
		t.Fatalf("a parent that states its input, with a sub-agent: %v", err)
	}
}

// The declarations digest covers the requirement and a tool's trusted declaration, and only when they are set.
func TestRequireProvenance_IsInTheDigestOnlyWhenSet(t *testing.T) {
	mk := func(require bool, tool Tool) *Agent {
		a, err := provAgent(t, require, tool)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	base := mk(false, provTool("a", Untrusted("crm")))
	if mk(true, provTool("a", Untrusted("crm"))).taintDigest() == base.taintDigest() {
		t.Error("RequireProvenance is not in the digest")
	}
	if mk(false, provTool("a", Untrusted("crm"))).taintDigest() != base.taintDigest() {
		t.Error("the same declarations give another digest")
	}
	if mk(false, provTool("a", trustedOutput("mine"))).taintDigest() == mk(false, provTool("a")).taintDigest() {
		t.Error("a trusted declaration is not in the digest")
	}
	if mk(false, provTool("a", trustedOutput("mine"))).taintDigest() == mk(false, provTool("a", trustedOutput("other reason"))).taintDigest() {
		t.Error("the trusted reason is not in the digest")
	}
	// A policy that does not require it digests as before this existed: golden from before the field.
	if d := base.declarationsDoc(); d.Policy.RequireProvenance || d.Tools[0].Trusted != "" {
		t.Errorf("document = %+v", d)
	}
	doc := mk(true, provTool("a", trustedOutput("mine"))).declarationsDoc()
	if !doc.Policy.RequireProvenance || doc.Tools[0].Trusted != "mine" || doc.Recompute() != doc.Digest {
		t.Errorf("document = %+v, recompute %q", doc, doc.Recompute())
	}
}
