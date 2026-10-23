package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// gateAndTimeoutNoCaps wraps an embedded tool and declares an approval gate and a timeout, but not the
// embedded tool's caps.
type gateAndTimeoutNoCaps struct{ Tool }

func (g gateAndTimeoutNoCaps) Spec() ToolSpec {
	s := g.Tool.Spec()
	s.Caps = nil
	s.Approval = SingleApproval()
	s.Timeout = time.Second
	return s
}

// outerKeepsSpec embeds a tool and reports its spec as is, so it declares what the embedded one does.
type outerKeepsSpec struct{ Tool }

func (o outerKeepsSpec) Spec() ToolSpec { return o.Tool.Spec() }

// A decorator that embeds a tool which has an approval gate and a timeout, and which in turn embeds a
// tool with amount caps, must still be refused when the outer spec drops the caps. checkEmbedded once
// stopped walking as soon as it had seen a gate and a timeout, so the deeper caps were never collected
// and the decorator registered with the cap gone.
func TestCheckEmbedded_FindsCapsBelowAGateAndATimeout(t *testing.T) {
	type in struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	deep := MustFunc("refund", "refund", func(context.Context, in) (string, error) { return "ok", nil }, WithCaps(refuseUSD))
	if len(deep.Spec().Caps) == 0 {
		t.Fatal("the deep tool has no caps")
	}
	mid := gateAndTimeoutNoCaps{deep}
	if s := mid.Spec(); s.Approval == nil || s.Timeout == 0 || len(s.Caps) != 0 {
		t.Fatalf("the middle tool's spec = %+v, want a gate and a timeout and no caps", s)
	}
	_, err := New(&scriptModel{}, memJournal(), WithTools(outerKeepsSpec{mid}))
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("New with a decorator that hides the caps of a tool below a gate and a timeout = %v, want ErrConfig", err)
	}
}

// zzD1 and zzD2 are two embedding decorators, each declaring its own approval gate and timeout and
// dropping the caps of the tool below: D1 embeds D2, which embeds the capped tool.
type zzD1 struct{ Tool }

func (d zzD1) Spec() ToolSpec { return dropCapsKeepGate(d.Tool.Spec()) }

type zzD2 struct{ Tool }

func (d zzD2) Spec() ToolSpec { return dropCapsKeepGate(d.Tool.Spec()) }

func dropCapsKeepGate(s ToolSpec) ToolSpec {
	s.Caps = nil
	s.Approval = SingleApproval()
	s.Timeout = time.Second
	return s
}

// The same hole at depth two with each decorator carrying its own gate and timeout (gates-reviewer's
// probe): the capped tool sits below both.
func TestCheckEmbedded_FindsCapsTwoDecoratorsDeep(t *testing.T) {
	type in struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	capped := MustFunc("refund", "refund", func(context.Context, in) (string, error) { return "ok", nil }, WithCaps(refuseUSD))
	_, err := New(&scriptModel{}, memJournal(), WithTools(zzD1{zzD2{capped}}))
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("New with D1 embedding D2 embedding a capped tool, both dropping the caps = %v, want ErrConfig", err)
	}
	// A decorator that carries the caps is accepted.
	keeps := func(d Tool) Tool { return keepsCaps{d} }
	if _, err := New(&scriptModel{}, memJournal(), WithTools(keeps(capped))); err != nil {
		t.Fatalf("a decorator that carries the caps: %v", err)
	}
}

// keepsCaps embeds a tool and reports its spec unchanged.
type keepsCaps struct{ Tool }

func (k keepsCaps) Spec() ToolSpec { return k.Tool.Spec() }
