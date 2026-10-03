package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// logged is the usual decorator: it embeds a Tool and overrides Call. Tool's interface has no Spec
// method, so the struct's method set has none either, and SpecOf falls back to the old method set.
type logged struct {
	Tool
	seen *atomic.Int32
}

func (l logged) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	l.seen.Add(1)
	return l.Tool.Call(ctx, args)
}

// unwrapped is the same decorator with Unwrap, which SpecOf follows.
type unwrapped struct{ logged }

func (u unwrapped) Unwrap() Tool { return u.logged.Tool }

// nested hides the gated tool one level deeper, behind an unexported embedded pointer.
type nested struct{ *inner }
type inner struct{ Tool }

// Before P12 the approval gate was part of Safety (Safety{RequiresApproval: true}), which an
// embedding decorator forwards. Now it lives only in ToolSpec.Approval, read through a Spec method
// the decorator does not have, so the gate would be dropped without an error and the side effect
// would run without the approval the tool was built to require. New fails closed: a tool that
// embeds a gated tool (or one with a timeout) and whose own spec lacks the gate is ErrConfig; a
// decorator with Unwrap keeps the gate.
func TestRev117e_EmbeddingDecoratorDropsApprovalGate(t *testing.T) {
	var sent, seen atomic.Int32
	send := Func("send", "", Safety{}, func(context.Context, struct{}) (string, error) { sent.Add(1); return "sent", nil },
		WithApproval(SingleApproval()))
	timed := Func("send", "", Safety{}, func(context.Context, struct{}) (string, error) { sent.Add(1); return "sent", nil },
		WithTimeout(time.Minute))
	if SpecOf(send).Approval == nil {
		t.Fatal("setup: the inner tool is gated")
	}
	run := func(tool Tool) error {
		m := NewScriptedModel(ToolTurn("c1", "send", `{}`), TextTurn("done"))
		_, err := mustNew(m, memJournal(), WithTools(tool)).Run(context.Background(), "r", "go")
		return err
	}
	for name, tool := range map[string]Tool{
		"embedded":          logged{Tool: send, seen: &seen},
		"embedded pointer":  &logged{Tool: send, seen: &seen},
		"nested unexported": nested{&inner{send}},
		"timeout":           logged{Tool: timed, seen: &seen},
	} {
		if err := run(tool); !errors.Is(err, ErrConfig) || sent.Load() != 0 {
			t.Fatalf("%s: Run = %v, sent %d time(s); want ErrConfig before the tool runs", name, err, sent.Load())
		}
	}
	if s := SpecOf(unwrapped{logged{Tool: send, seen: &seen}}); s.Approval == nil {
		t.Fatalf("SpecOf does not follow Unwrap: %+v", s)
	}
	if s := SpecOf(unwrapped{logged{Tool: timed, seen: &seen}}); s.Timeout != time.Minute {
		t.Fatalf("SpecOf does not take the timeout through Unwrap: %+v", s)
	}
	var ap *ApprovalPending
	if err := run(unwrapped{logged{Tool: send, seen: &seen}}); !errors.As(err, &ap) || sent.Load() != 0 {
		t.Fatalf("Unwrap decorator: Run = %v, sent %d; want ApprovalPending", err, sent.Load())
	}
	if err := run(logged{Tool: Func("send", "", Safety{}, func(context.Context, struct{}) (string, error) { return "", nil }), seen: &seen}); err != nil {
		t.Fatalf("an ungated embedded tool: Run = %v", err)
	}
}

// ptrTool has pointer-receiver methods; specHider embeds it by value and declares its own Spec,
// which drops the embedded tool's approval gate.
type ptrTool struct{ spec ToolSpec }

func (t *ptrTool) Name() string                { return t.spec.Name }
func (t *ptrTool) Description() string         { return "" }
func (t *ptrTool) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *ptrTool) Safety() Safety              { return Safety{} }
func (t *ptrTool) Spec() ToolSpec              { return t.spec }
func (t *ptrTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`"sent"`), nil
}

type specHider struct{ ptrTool }

func (h *specHider) Spec() ToolSpec { return ToolSpec{Name: h.spec.Name} }

func TestRev117e_ValueEmbeddedPointerToolSpecHidesGate(t *testing.T) {
	h := &specHider{ptrTool{spec: ToolSpec{Name: "send", Approval: SingleApproval()}}}
	m := NewScriptedModel(ToolTurn("c1", "send", `{}`), TextTurn("done"))
	if _, err := mustNew(m, memJournal(), WithTools(h)).Run(context.Background(), "r", "go"); !errors.Is(err, ErrConfig) {
		t.Fatalf("Run = %v, want ErrConfig: the outer Spec hides the embedded tool's gate", err)
	}
}
