package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// logged is the usual decorator: it embeds a Tool and overrides Call. Tool's Spec is promoted, so
// the decorator describes itself with the wrapped tool's spec, approval gate and timeout included.
type logged struct {
	Tool
	seen *atomic.Int32
}

func (l logged) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	l.seen.Add(1)
	return l.Tool.Call(ctx, args)
}

// unwrapped is the same decorator with Unwrap.
type unwrapped struct{ logged }

func (u unwrapped) Unwrap() Tool { return u.logged.Tool }

// nested hides the gated tool one level deeper, behind an unexported embedded pointer.
type nested struct{ *inner }
type inner struct{ Tool }

// The approval gate lives in ToolSpec.Approval. A decorator that embeds a tool gets the tool's Spec
// method, so it keeps the gate and the timeout: the call waits for approval and the tool does not
// run first. (Before the Tool interface had Spec, such a decorator was described by the old method
// set, which has no gate, and New refused it; a decorator that declares a Spec of its own that drops
// the gate still is: see TestRev117e_ValueEmbeddedPointerToolSpecHidesGate.)
func TestRev117e_EmbeddingDecoratorDropsApprovalGate(t *testing.T) {
	var sent, seen atomic.Int32
	send := MustFunc("send", "", func(context.Context, struct{}) (string, error) { sent.Add(1); return "sent", nil },
		WithApproval(SingleApproval()))
	timed := MustFunc("send", "", func(context.Context, struct{}) (string, error) { sent.Add(1); return "sent", nil },
		WithTimeout(time.Minute))
	if send.Spec().Approval == nil {
		t.Fatal("setup: the inner tool is gated")
	}
	run := func(tool Tool) error {
		m := NewScriptedModel(ToolTurn("c1", "send", `{}`), TextTurn("done"))
		a, err := New(m, memJournal(), WithTools(tool))
		if err != nil {
			return err // refused when the agent is built
		}
		_, err = a.Run(context.Background(), "r", UserText("go"))
		return err
	}
	for name, tool := range map[string]Tool{
		"embedded":          logged{Tool: send, seen: &seen},
		"embedded pointer":  &logged{Tool: send, seen: &seen},
		"nested unexported": nested{&inner{send}},
	} {
		var ap *ApprovalPending
		if err := run(tool); !errors.As(err, &ap) || sent.Load() != 0 {
			t.Fatalf("%s: Run = %v, sent %d time(s); want ApprovalPending before the tool runs", name, err, sent.Load())
		}
	}
	if s := (logged{Tool: timed, seen: &seen}).Spec(); s.Timeout != time.Minute {
		t.Fatalf("an embedding decorator drops the timeout: %+v", s)
	}
	if s := (unwrapped{logged{Tool: send, seen: &seen}}).Spec(); s.Approval == nil {
		t.Fatalf("the Unwrap decorator drops the gate: %+v", s)
	}
	if s := (unwrapped{logged{Tool: timed, seen: &seen}}).Spec(); s.Timeout != time.Minute {
		t.Fatalf("the Unwrap decorator drops the timeout: %+v", s)
	}
	var ap *ApprovalPending
	if err := run(unwrapped{logged{Tool: send, seen: &seen}}); !errors.As(err, &ap) || sent.Load() != 0 {
		t.Fatalf("Unwrap decorator: Run = %v, sent %d; want ApprovalPending", err, sent.Load())
	}
	if err := run(logged{Tool: MustFunc("send", "", func(context.Context, struct{}) (string, error) { return "", nil }), seen: &seen}); err != nil {
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
	if _, err := New(m, memJournal(), WithTools(h)); !errors.Is(err, ErrConfig) {
		t.Fatalf("New = %v, want ErrConfig: the outer Spec hides the embedded tool's gate", err)
	}
}
