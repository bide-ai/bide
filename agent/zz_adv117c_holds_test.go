package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// Angles that hold: reached beats a sentinel added by a middleware or returned by the tool
// itself, and a sub-agent whose inner call was not called does not make the outer call "not
// called". Each case cancels the run inside the call, so a wrong "not called" verdict would
// record the claim never started and the resume would fire the side effect a second time.
func TestAdv117c_ReachedBeatsSentinel(t *testing.T) {
	type setup struct {
		mw    ToolMiddleware
		inner bool // the tool is a sub-agent whose own tool is refused
	}
	cases := map[string]setup{
		"middleware wraps a reached error": {mw: func(next ToolHandler) ToolHandler {
			return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
				_, err := next(ctx, call)
				return nil, fmt.Errorf("%w (%w)", err, ErrToolNotCalled)
			}
		}},
		"tool's own error wraps the sentinel": {},
		"sub-agent inner not-called":          {inner: true},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			var tool Tool
			if s.inner {
				innerTool := Func("inner", "", Safety{}, func(context.Context, struct{}) (string, error) {
					calls.Add(1) // the sub-run's side effect
					cancel()
					return "", fmt.Errorf("declined (%w)", ErrToolNotCalled)
				})
				subStore := memJournal()
				sub := mustNew(
					NewScriptedModel(ToolTurn("i1", "inner", `{}`), TextTurn("done")),
					subStore,
					WithTools(innerTool),
				)
				tool = SubAgent("charge", "", sub)
			} else {
				tool = Func("charge", "", Safety{}, func(context.Context, struct{}) (string, error) {
					calls.Add(1)
					cancel()
					return "", fmt.Errorf("declined (%w)", ErrToolNotCalled)
				})
			}
			store := memJournal()
			args := `{}`
			if s.inner {
				args = `{"task":"x"}`
			}
			m := NewScriptedModel(ToolTurn("c1", "charge", args), TextTurn("done"))
			a := mustNew(m, store, WithTools(tool))
			if s.mw != nil {
				a = must(a.With(WithToolMiddleware(s.mw)))
			}
			_, err1 := a.Run(ctx, "r1", "go")
			_, err2 := mustNew(m, store, WithTools(tool)).Run(context.Background(), "r1", "go")
			var halt *OutcomeUnknown
			if calls.Load() != 1 || !errors.As(err2, &halt) {
				t.Fatalf("calls %d; first %v; resume %v", calls.Load(), err1, err2)
			}
			t.Logf("calls %d; first %v; resume %v", calls.Load(), err1, err2)
		})
	}
}
