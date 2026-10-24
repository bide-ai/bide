package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// RequireProvenance (TaintPolicy.RequireProvenance, the tool option
// Trusted, ToolSpec.Trusted): every tool declares a provenance and every run states its input's.

func provTool(name string, opts ...ToolOption) Tool {
	return MustFunc(name, name, func(context.Context, struct{}) (string, error) { return "ok", nil }, opts...)
}

func provAgent(t *testing.T, require bool, tools ...Tool) (*Agent, error) {
	t.Helper()
	return New(&scriptModel{turns: [][]Emit{textTurn("done"), textTurn("done"), textTurn("done"), textTurn("done"), textTurn("done"), textTurn("done")}}, memJournal(), WithTools(tools...),
		WithTaintPolicy(TaintPolicy{Refuse: true, RequireProvenance: require}))
}

func TestRequireProvenance_EveryToolDeclaresOne(t *testing.T) {
	declared := []Tool{provTool("a", Untrusted("crm")), provTool("b", Trusted("our own table"))}
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
	if _, err := provAgent(t, false, provTool("both", Untrusted("crm"), Trusted("mine"))); !errors.Is(err, ErrConfig) {
		t.Fatalf("a tool that is both untrusted and trusted = %v, want ErrConfig", err)
	}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("a trusted declaration with no reason was accepted")
			}
		}()
		provTool("empty", Trusted(""))
	}()
}

func TestRequireProvenance_EveryRunStatesItsInput(t *testing.T) {
	ctx := context.Background()
	run := func(a *Agent, id string, opts ...RunOption) error {
		_, err := a.Run(ctx, id, UserText("go"), opts...)
		return err
	}
	a, err := provAgent(t, true, provTool("a", Trusted("mine")))
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
	b, _ := provAgent(t, true, provTool("a", Trusted("mine")))
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
	a, err := New(&scriptModel{turns: [][]Emit{textTurn("one"), textTurn("two")}}, memJournal(), WithTools(provTool("a", Trusted("mine"))),
		WithTaintPolicy(TaintPolicy{Refuse: true, RequireProvenance: true}))
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
	childJ := memJournal()
	child, err := New(&scriptModel{turns: [][]Emit{textTurn("child answer")}}, childJ, WithTools(provTool("a", Trusted("mine"))),
		WithTaintPolicy(TaintPolicy{Refuse: true, RequireProvenance: true}))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := New(&scriptModel{turns: [][]Emit{toolTurn("d1", "delegate", `{"task":"go"}`), textTurn("done")}}, memJournal(),
		WithTools(MustSubAgent("delegate", "delegate", child, TaintExempt("test"), Trusted("a sub-agent: what it read marks its answer by itself")), provTool("a", Trusted("mine"))),
		WithTaintPolicy(TaintPolicy{Refuse: true, RequireProvenance: true}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parent.Run(ctx, "r", UserText("go"), WithTrustedInput("the operator")); err != nil {
		t.Fatalf("a parent that states its input, with a sub-agent: %v", err)
	}
	// The child's run started (a refused start is a tool error the parent reads and goes on from).
	if _, ok, err := RecordedStart(ctx, childJ, SubRunID("r", "d1")); err != nil || !ok {
		t.Fatalf("the sub-agent's run did not start (%v, %v): it was asked to state a provenance", ok, err)
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
	if mk(false, provTool("a", Trusted("mine"))).taintDigest() == mk(false, provTool("a")).taintDigest() {
		t.Error("a trusted declaration is not in the digest")
	}
	if mk(false, provTool("a", Trusted("mine"))).taintDigest() == mk(false, provTool("a", Trusted("other reason"))).taintDigest() {
		t.Error("the trusted reason is not in the digest")
	}
	// A policy that does not require it digests as before this existed: golden from before the field.
	if d := base.declarationsDoc(); d.Policy.RequireProvenance || d.Tools[0].Trusted != "" {
		t.Errorf("document = %+v", d)
	}
	doc := mk(true, provTool("a", Trusted("mine"))).declarationsDoc()
	if !doc.Policy.RequireProvenance || doc.Tools[0].Trusted != "mine" || doc.Recompute() != doc.Digest {
		t.Errorf("document = %+v, recompute %q", doc, doc.Recompute())
	}
}

// reclassify embeds a tool and reports its spec with the provenance replaced.
type reclassify struct {
	Tool
	untrusted, trusted string
}

func (r reclassify) Spec() ToolSpec {
	s := r.Tool.Spec()
	s.Untrusted, s.Safety.Untrusted, s.Trusted = r.untrusted, "", r.trusted
	return s
}

// unwrapReclassify is reclassify with a named field and Unwrap, so the Unwrap chain check sees it.
type unwrapReclassify struct {
	inner              Tool
	untrusted, trusted string
}

func (r unwrapReclassify) Spec() ToolSpec {
	s := r.inner.Spec()
	s.Untrusted, s.Safety.Untrusted, s.Trusted = r.untrusted, "", r.trusted
	return s
}
func (r unwrapReclassify) Call(ctx context.Context, a json.RawMessage) (json.RawMessage, error) {
	return r.inner.Call(ctx, a)
}
func (r unwrapReclassify) Unwrap() Tool { return r.inner }

// A wrapper that declares Trusted over a tool that declares Untrusted drops the taint: ErrConfig at New, in
// every wrapper shape, naming both declarations. The other direction only makes the wrapper unclassified,
// which RequireProvenance refuses.
func TestTrustedWrapperOverUntrustedToolIsRefused(t *testing.T) {
	ok := func(context.Context, struct{}) (string, error) { return "ok", nil }
	untrusted := MustFunc("t", "t", ok, Untrusted("crm"))
	trusted := MustFunc("t", "t", ok, Trusted("mine"))
	for name, w := range map[string]Tool{
		"embedded":         reclassify{untrusted, "", "launder"},
		"embedded depth 2": reclassify{reclassify{untrusted, "crm", ""}, "", "launder"},
		"unwrap":           unwrapReclassify{untrusted, "", "launder"},
		"unwrap depth 2":   unwrapReclassify{unwrapReclassify{untrusted, "crm", ""}, "", "launder"},
	} {
		_, err := New(&scriptModel{}, memJournal(), WithTools(w))
		if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "crm") && !strings.Contains(err.Error(), "launder") {
			t.Errorf("%s: Trusted wrapper over an Untrusted tool = %v, want ErrConfig naming the declarations", name, err)
			continue
		}
		if !strings.Contains(err.Error(), "Trusted(") {
			t.Errorf("%s: the error does not say the wrapper declares Trusted: %v", name, err)
		}
	}
	// An embedding decorator that declares nothing over an Untrusted tool drops the taint as well.
	for name, w := range map[string]Tool{
		"embedded, nothing":         reclassify{untrusted, "", ""},
		"embedded depth 2, nothing": reclassify{reclassify{untrusted, "crm", ""}, "", ""},
	} {
		if _, err := New(&scriptModel{}, memJournal(), WithTools(w)); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: a decorator that drops Untrusted = %v, want ErrConfig", name, err)
		}
	}
	// A wrapper that keeps Untrusted is accepted.
	if _, err := New(&scriptModel{}, memJournal(), WithTools(reclassify{untrusted, "crm", ""})); err != nil {
		t.Fatalf("a wrapper that keeps Untrusted: %v", err)
	}
	// The other direction: a wrapper that drops an inner Trusted is accepted without the requirement, and
	// refused with it (it is unclassified).
	w := unwrapReclassify{trusted, "", ""}
	if _, err := New(&scriptModel{}, memJournal(), WithTools(w)); err != nil {
		t.Fatalf("a wrapper that drops Trusted, no requirement: %v", err)
	}
	if _, err := New(&scriptModel{}, memJournal(), WithTools(w), WithTaintPolicy(TaintPolicy{Refuse: true, RequireProvenance: true})); !errors.Is(err, ErrConfig) {
		t.Fatalf("a wrapper that drops Trusted under RequireProvenance = %v, want ErrConfig", err)
	}
	if _, err := New(&scriptModel{}, memJournal(), WithTools(keepsCaps{trusted}), WithTaintPolicy(TaintPolicy{Refuse: true, RequireProvenance: true})); err != nil {
		t.Fatalf("an embedding decorator that keeps the inner spec: %v", err)
	}
}

// A blank reason is not a declaration.
func TestTrustedRefusesABlankReason(t *testing.T) {
	ok := func(context.Context, struct{}) (string, error) { return "ok", nil }
	for _, r := range []string{"", " ", "   ", "\t", " \t ", "\n", "\u00a0", "\u2003", "\u200b", "\u3000"} {
		if _, err := Func("t", "t", ok, Trusted(r)); !errors.Is(err, ErrConfig) {
			t.Errorf("Trusted(%q) = %v, want ErrConfig", r, err)
		}
	}
}
