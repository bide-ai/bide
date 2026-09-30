package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

var hundred = Usage{InputTokens: 80, OutputTokens: 20} // 100 tokens per model call

// stepper calls "step" until its conversation holds n tool results, then answers "done". Every
// call reports usage u, and calls counts them.
type stepper struct {
	n     int
	u     Usage
	calls atomic.Int32
}

func (m *stepper) Stream(_ context.Context, req Request) (*Stream, error) {
	m.calls.Add(1)
	results := 0
	for _, msg := range req.Messages {
		if msg.Role == RoleTool {
			results++
		}
	}
	ch := make(chan Emit, 2)
	if results >= m.n {
		ch <- Emit{Event: TextDelta{Text: "done"}}
		ch <- Emit{Event: Finish{Reason: "stop", Usage: m.u}}
	} else {
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: fmt.Sprintf("s%d", results), Name: "step", ArgsFragment: []byte(`{}`)}}
		ch <- Emit{Event: Finish{Reason: "tool_use", Usage: m.u}}
	}
	close(ch)
	return NewStream(ch), nil
}

func stepTool(fn func(ctx context.Context) error) Tool {
	return Func("step", "one step", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		if err := fn(ctx); err != nil {
			return "", err
		}
		return "stepped", nil
	})
}

func times(u Usage, n int) Usage {
	return Usage{InputTokens: u.InputTokens * n, OutputTokens: u.OutputTokens * n, CacheReadTokens: u.CacheReadTokens * n, CacheWriteTokens: u.CacheWriteTokens * n}
}

// Result.Usage and Result.Spend are the whole run's, however many invocations it took: a run
// cut off after its first model call and resumed reports both calls, and re-entering it once
// finished reports the same again.
func TestRunResult_WholeRunAcrossResume(t *testing.T) {
	store := NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	cut := stepTool(func(ctx context.Context) error { cancel(); return ctx.Err() })
	if _, err := New(&stepper{n: 1, u: hundred}, store, cut).RunResult(ctx, "r1", "q"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first invocation: err = %v, want context.Canceled", err)
	}
	a := New(&stepper{n: 1, u: hundred}, store, stepTool(func(context.Context) error { return nil }))
	for _, pass := range []string{"resumed", "re-entered"} {
		res, err := a.RunResult(context.Background(), "r1", "q")
		if err != nil {
			t.Fatalf("%s: %v", pass, err)
		}
		if want := times(hundred, 2); res.Usage != want || res.Spend != want {
			t.Fatalf("%s: Usage = %+v, Spend = %+v; want both %+v (the run's two model calls)", pass, res.Usage, res.Spend, want)
		}
	}
}

