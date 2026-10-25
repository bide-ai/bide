package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A wrapper's Safety is the one the agent acts on (taint gate, resume), so a wrapper may not declare a safer
// one than a tool inside it unless it says why (RaisesSafety).

// swNamed holds its tool in a named field (the Unwrap shape); swEmbed embeds it (the decorator shape).
type swNamed struct {
	in     Tool
	mutate func(*ToolSpec)
}

func (w swNamed) Spec() ToolSpec {
	s := w.in.Spec()
	w.mutate(&s)
	return s
}
func (w swNamed) Call(ctx context.Context, a json.RawMessage) (json.RawMessage, error) {
	return w.in.Call(ctx, a)
}
func (w swNamed) Unwrap() Tool { return w.in }

type swEmbed struct {
	Tool
	mutate func(*ToolSpec)
}

func (w swEmbed) Spec() ToolSpec {
	s := w.Tool.Spec()
	w.mutate(&s)
	return s
}

var swLevels = []struct {
	name string
	s    Safety
	rank int
}{
	{"side effect", Safety{}, 0},
	{"Idempotent", Safety{Idempotent: true}, 1},
	{"ReadOnly", Safety{ReadOnly: true}, 2},
	{"ReadOnly and Idempotent", Safety{ReadOnly: true, Idempotent: true}, 2},
}

// swShape builds a wrapper called "outer" around the tool "inner" at the given depth (1 or 2), with the
// outer spec edited by edit. At depth 2 a middle wrapper declares exactly what the inner tool does.
func swShape(shape string, depth int, inner Tool, edit func(*ToolSpec)) Tool {
	keep := func(s *ToolSpec) { s.Name = "middle" }
	outer := func(s *ToolSpec) { s.Name = "outer"; edit(s) }
	switch {
	case shape == "unwrap" && depth == 1:
		return swNamed{in: inner, mutate: outer}
	case shape == "unwrap":
		return swNamed{in: swNamed{in: inner, mutate: keep}, mutate: outer}
	case depth == 1:
		return swEmbed{Tool: inner, mutate: outer}
	}
	return swEmbed{Tool: swEmbed{Tool: inner, mutate: keep}, mutate: outer}
}

func swRegister(t Tool) error {
	_, err := New(NewScriptedModel(TextTurn("x")), memJournal(), WithTools(t), noKeys())
	return err
}

func swInner(level Safety) Tool {
	var c counter
	return c.tool("inner", WithSafety(level))
}

// Every pair of levels, in both shapes at depth 1 and 2: a wrapper safer than the tool inside it is
// ErrConfig naming the wrapper, the tool and both levels; the same or a stricter level is accepted.
func TestSafetyWrap_SaferWrapperIsRefused(t *testing.T) {
	for _, shape := range []string{"unwrap", "embed"} {
		for _, depth := range []int{1, 2} {
			for _, in := range swLevels {
				for _, out := range swLevels {
					t.Run(shape+"/depth"+string(rune('0'+depth))+"/inner "+in.name+"/outer "+out.name, func(t *testing.T) {
						err := swRegister(swShape(shape, depth, swInner(in.s), func(s *ToolSpec) { s.Safety = out.s }))
						if out.rank <= in.rank {
							if err != nil {
								t.Fatalf("a wrapper at the same or a stricter level was refused: %v", err)
							}
							return
						}
						if !errors.Is(err, ErrConfig) {
							t.Fatalf("err = %v, want ErrConfig", err)
						}
						inName := `"inner"`
						if depth == 2 {
							inName = `"middle"` // the first tool on the way down at that level, which declares what inner does
						}
						for _, want := range []string{`"outer"`, inName, "Safety " + safetyLevel(out.s) + " ", "wrapped tool " + safetyLevel(in.s), "RaisesSafety"} {
							if !strings.Contains(err.Error(), want) {
								t.Errorf("err = %v, want it to contain %q", err, want)
							}
						}
					})
				}
			}
		}
	}
}

// The least safe tool inside is the one the wrapper is held to, wherever it sits: a middle wrapper that
// raised the level with a reason does not let the outer one declare it without its own.
func TestSafetyWrap_HeldToTheLeastSafeToolInside(t *testing.T) {
	for _, shape := range []string{"unwrap", "embed"} {
		t.Run(shape, func(t *testing.T) {
			raise := func(reason string) func(*ToolSpec) {
				return func(s *ToolSpec) { s.Safety = Safety{ReadOnly: true}; s.RaisesSafety = reason }
			}
			mid := func(in Tool) Tool {
				if shape == "unwrap" {
					return swNamed{in: in, mutate: func(s *ToolSpec) { s.Name = "middle"; raise("it only reads a replica")(s) }}
				}
				return swEmbed{Tool: in, mutate: func(s *ToolSpec) { s.Name = "middle"; raise("it only reads a replica")(s) }}
			}
			wrap := func(in Tool, r string) Tool {
				if shape == "unwrap" {
					return swNamed{in: in, mutate: func(s *ToolSpec) { s.Name = "outer"; raise(r)(s) }}
				}
				return swEmbed{Tool: in, mutate: func(s *ToolSpec) { s.Name = "outer"; raise(r)(s) }}
			}
			if err := swRegister(wrap(mid(swInner(Safety{})), "")); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), `"inner"`) {
				t.Fatalf("outer without a reason over a raised middle = %v, want ErrConfig naming inner", err)
			}
			if err := swRegister(wrap(mid(swInner(Safety{})), "it only reads a replica")); err != nil {
				t.Fatalf("outer with its own reason: %v", err)
			}
		})
	}
}

// RaisesSafety with a valid reason lets the wrapper declare a safer level, in every shape and depth.
func TestSafetyWrap_RaisesSafetyAcceptsASaferWrapper(t *testing.T) {
	for _, shape := range []string{"unwrap", "embed"} {
		for _, depth := range []int{1, 2} {
			err := swRegister(swShape(shape, depth, swInner(Safety{}), func(s *ToolSpec) {
				s.Safety = Safety{ReadOnly: true}
				s.RaisesSafety = "it stops at a dry run and reads the quote only"
			}))
			if err != nil {
				t.Errorf("%s depth %d: %v", shape, depth, err)
			}
		}
	}
}

// The reason follows the rules of Trusted: required, printable, at most 128 bytes. As the option and as a
// field of a spec built by hand.
func TestSafetyWrap_RaisesSafetyReasonIsValidated(t *testing.T) {
	bad := map[string]string{"whitespace": "   ", "tab": "\t", "zero-width": "​", "control": "a\x01b", "over the cap": strings.Repeat("x", 129), "invalid UTF-8": "a\xffb"}
	for name, r := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := Func("f", "d", func(context.Context, struct{}) (string, error) { return "", nil }, RaisesSafety(r)); !errors.Is(err, ErrConfig) {
				t.Errorf("RaisesSafety(%q) = %v, want ErrConfig", r, err)
			}
			err := swRegister(swShape("unwrap", 1, swInner(Safety{}), func(s *ToolSpec) { s.Safety = Safety{ReadOnly: true}; s.RaisesSafety = r }))
			if !errors.Is(err, ErrConfig) {
				t.Errorf("a spec with RaisesSafety %q = %v, want ErrConfig", r, err)
			}
		})
	}
	if _, err := Func("f", "d", func(context.Context, struct{}) (string, error) { return "", nil }, RaisesSafety("")); !errors.Is(err, ErrConfig) {
		t.Errorf("RaisesSafety(\"\") = %v, want ErrConfig", err)
	}
	if _, err := Func("f", "d", func(context.Context, struct{}) (string, error) { return "", nil }, RaisesSafety(strings.Repeat("x", 128))); err != nil {
		t.Errorf("a reason of exactly the cap: %v", err)
	}
	// A bad reason is refused even on a wrapper that would not need it.
	if err := swRegister(swShape("embed", 1, swInner(Safety{}), func(s *ToolSpec) { s.RaisesSafety = " " })); !errors.Is(err, ErrConfig) {
		t.Errorf("a bad reason on a wrapper at the same level = %v, want ErrConfig", err)
	}
}

// RaisesSafety is a declaration: it is in the declarations document and in the digest, only when set.
func TestSafetyWrap_RaisesSafetyIsDeclaredAndDigested(t *testing.T) {
	var c counter
	build := func(opts ...ToolOption) *Agent {
		return mustNew(NewScriptedModel(TextTurn("x")), memJournal(), WithTools(c.tool("t", opts...)), WithTaintPolicy(TaintPolicy{Refuse: true}))
	}
	plain, raised, other := build(), build(RaisesSafety("dry run")), build(RaisesSafety("replica"))
	if plain.taintDigest() == raised.taintDigest() || raised.taintDigest() == other.taintDigest() {
		t.Error("RaisesSafety, or its reason, is not in the digest")
	}
	if got := plain.declarationsDoc().Tools[0]; got.RaisesSafety != "" || plain.declarationsDoc().Recompute() != plain.taintDigest() {
		t.Errorf("a tool without it: %+v", got)
	}
	d := raised.declarationsDoc()
	if d.Tools[0].RaisesSafety != "dry run" || d.Recompute() != d.Digest {
		t.Errorf("the document does not carry the reason, or its digest disagrees: %+v", d.Tools[0])
	}
	b, err := raised.Declarations()
	if err != nil || !strings.Contains(string(b), `"raises_safety": "dry run"`) {
		t.Errorf("the dump = %s, %v", b, err)
	}
	if b, _ := plain.Declarations(); strings.Contains(string(b), "raises_safety") || strings.Contains(string(b), "idempotent") {
		t.Errorf("a tool that sets neither has them in the dump: %s", b)
	}
	// An Idempotent tool is reported as such; ReadOnly is the effect flag.
	idem := mustNew(NewScriptedModel(TextTurn("x")), memJournal(), WithTools(c.tool("t", WithSafety(Safety{Idempotent: true}))))
	if tl := idem.declarationsDoc().Tools[0]; !tl.Idempotent || !tl.Effect {
		t.Errorf("an Idempotent tool = %+v", tl)
	}
	ro := mustNew(NewScriptedModel(TextTurn("x")), memJournal(), WithTools(c.tool("t", WithSafety(Safety{ReadOnly: true, Idempotent: true}))))
	if tl := ro.declarationsDoc().Tools[0]; tl.Idempotent || tl.Effect {
		t.Errorf("a ReadOnly tool = %+v", tl)
	}
}

// The opt-out is meaningful: a wrapper that raised its level over a side-effecting tool is treated by the
// level it declared. After a run read untrusted content, the side-effecting tool itself is refused by the
// taint policy; the same tool behind a ReadOnly RaisesSafety wrapper is not gated, as a ReadOnly tool is not.
// The wrapper's Safety is also the one a resume acts on.
func TestSafetyWrap_ARaisedWrapperIsTreatedByItsDeclaredLevel(t *testing.T) {
	run := func(raised bool) (writes int32, a *Agent) {
		var reads, w counter
		read := reads.tool("read", WithSafety(Safety{ReadOnly: true}), Untrusted("crm"))
		write := w.tool("inner")
		tool := write
		if raised {
			tool = swNamed{in: write, mutate: func(s *ToolSpec) {
				s.Name = "outer"
				s.Safety = Safety{ReadOnly: true}
				s.RaisesSafety = "it only reads the quote"
			}}
		}
		name := tool.Spec().Name
		a = mustNew(&scriptModel{turns: [][]Emit{toolTurn("c1", "read", `{}`), toolTurn("c2", name, `{}`), textTurn("done")}}, memJournal(),
			WithTools(read, tool), WithTaintPolicy(TaintPolicy{Refuse: true}))
		if _, err := a.Run(context.Background(), "r", UserText("go")); err != nil {
			t.Fatal(err)
		}
		return w.n.Load(), a
	}
	if n, _ := run(false); n != 0 {
		t.Fatalf("the side-effecting tool ran %d times after an untrusted read, want 0: the control does not gate", n)
	}
	n, a := run(true)
	if n != 1 {
		t.Fatalf("the raised wrapper ran the inner tool %d times, want 1: it is treated as ReadOnly", n)
	}
	if !a.specs["outer"].Safety.RetrySafe() {
		t.Error("a resume would not re-run the raised wrapper's call")
	}
}
