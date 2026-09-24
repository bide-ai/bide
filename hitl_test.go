package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// askTool interrupts on its first run to collect a typed value, then returns it.
type askTool struct {
	name   string
	safety Safety
	key    string
	calls  *int
	got    *string
}

func (t *askTool) Name() string                { return t.name }
func (t *askTool) Description() string         { return "" }
func (t *askTool) Safety() Safety              { return t.safety }
func (t *askTool) ArgsSchema() json.RawMessage { return nil }
func (t *askTool) Call(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	*t.calls++
	v, err := Interrupt[string](ctx, t.key, "what should I use?")
	if err != nil {
		return nil, err
	}
	if t.got != nil {
		*t.got = v
	}
	return json.Marshal(map[string]string{"used": v})
}

// A tool pauses via Interrupt; Resume supplies a typed value; re-running continues.
func TestInterrupt_PausesAndResumesTyped(t *testing.T) {
	store := NewMemStore()
	var calls int
	var got string
	tool := &askTool{name: "ask", safety: Safety{ReadOnly: true}, key: "q", calls: &calls, got: &got}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "ask", `{}`), textTurn("done")}}
	a := New(m, store, tool)

	_, err := a.Run(context.Background(), "r", "hi")
	var intr *Interrupted
	if !errors.As(err, &intr) {
		t.Fatalf("err = %v, want *Interrupted", err)
	}
	if intr.Key != "q" || intr.Prompt != "what should I use?" {
		t.Fatalf("interrupt = %+v", intr)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times before resume, want 1", calls)
	}

	if err := Resume(context.Background(), store, "r", "q", "hello-human"); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	out, err := a.Run(context.Background(), "r", "hi") // same agent, resumes
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if textOf(out) != "done" {
		t.Fatalf("answer = %q", textOf(out))
	}
	if got != "hello-human" {
		t.Fatalf("tool received %q, want hello-human", got)
	}
	if calls != 2 {
		t.Fatalf("tool ran %d times total, want 2 (re-run to resolve)", calls)
	}
}

// A struct resume value round-trips through the journal.
func TestInterrupt_StructValue(t *testing.T) {
	store := NewMemStore()
	type choice struct {
		Option string `json:"option"`
		Weight int    `json:"weight"`
	}
	var picked choice
	tool := Func("pick", "", Safety{ReadOnly: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			c, err := Interrupt[choice](ctx, "pick", nil)
			if err != nil {
				return "", err
			}
			picked = c
			return c.Option, nil
		})
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "pick", `{}`), textTurn("ok")}}
	a := New(m, store, tool)

	if _, err := a.Run(context.Background(), "r", "hi"); !errorsAsInterrupted(err) {
		t.Fatalf("want interrupt, got %v", err)
	}
	if err := Resume(context.Background(), store, "r", "pick", choice{Option: "b", Weight: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r", "hi"); err != nil {
		t.Fatal(err)
	}
	if picked.Option != "b" || picked.Weight != 3 {
		t.Fatalf("picked = %+v, want {b 3}", picked)
	}
}

// Interrupt from a non-retry-safe tool is a misuse and fails with ErrConfig (rather than
// silently halting on resume).
func TestInterrupt_RequiresRetrySafe(t *testing.T) {
	store := NewMemStore()
	var calls int
	tool := &askTool{name: "write", safety: Safety{}, key: "q", calls: &calls} // not retry-safe
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "write", `{}`), textTurn("done")}}
	a := New(m, store, tool)

	_, err := a.Run(context.Background(), "r", "hi")
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want errors.Is ErrConfig", err)
	}
	if errorsAsInterrupted(err) {
		t.Fatal("a non-retry-safe interrupt must not surface as a resumable pause")
	}
}

// Interrupt outside a running agent (no run context) is a config error.
func TestInterrupt_OutsideRun(t *testing.T) {
	_, err := Interrupt[string](context.Background(), "k", nil)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig", err)
	}
}

func errorsAsInterrupted(err error) bool {
	var i *Interrupted
	return errors.As(err, &i)
}
