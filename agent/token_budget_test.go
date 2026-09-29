package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// meteredModel calls "lookup" until it has a tool result, then answers. Every call reports
// usage u, and calls counts them.
type meteredModel struct {
	u     Usage
	calls atomic.Int32
}

func (m *meteredModel) Stream(_ context.Context, req Request) (*Stream, error) {
	m.calls.Add(1)
	ch := make(chan Emit, 2)
	if req.Messages[len(req.Messages)-1].Role == RoleTool {
		ch <- Emit{Event: TextDelta{Text: "ok"}}
		ch <- Emit{Event: Finish{Reason: "stop", Usage: m.u}}
	} else {
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: []byte(`{}`)}}
		ch <- Emit{Event: Finish{Reason: "tool_use", Usage: m.u}}
	}
	close(ch)
	return NewStream(ch), nil
}

func lookupTool(onCall func()) Tool {
	return Func("lookup", "look up", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		onCall()
		return "found", ctx.Err()
	})
}

var turnUsage = Usage{InputTokens: 100, OutputTokens: 20} // 120 tokens per model call

// The budget is per run: one agent serving many runs gives each its own budget.
func TestTokenBudget_IsPerRun(t *testing.T) {
	a := New(&meteredModel{u: turnUsage}, NewMemStore(), lookupTool(func() {})).WithTokenBudget(300)
	for _, run := range []string{"r1", "r2", "r3"} {
		if _, err := a.Run(context.Background(), run, "q"); err != nil {
			t.Errorf("run %s (240 tokens, budget 300): %v", run, err)
		}
	}
}

// The call that crosses the budget completes, since its usage is known only after it returns;
// the next call is refused before it reaches the model.
func TestTokenBudget_RefusesTheNextCall(t *testing.T) {
	m := &meteredModel{u: turnUsage}
	_, err := New(m, NewMemStore(), lookupTool(func() {})).WithTokenBudget(100).Run(context.Background(), "r1", "q")
	if !errors.Is(err, ErrBudgetExceeded) || !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if n := m.calls.Load(); n != 1 {
		t.Fatalf("the model was called %d times, want 1 (the second call is over budget)", n)
	}
}

// Cached input counts: a call that reads most of its prompt from the cache still used it.
func TestTokenBudget_CountsCachedInput(t *testing.T) {
	m := &meteredModel{u: Usage{InputTokens: 10, CacheReadTokens: 200, OutputTokens: 5}}
	_, err := New(m, NewMemStore(), lookupTool(func() {})).WithTokenBudget(100).Run(context.Background(), "r1", "q")
	if !errors.Is(err, ErrBudgetExceeded) || m.calls.Load() != 1 {
		t.Fatalf("err = %v after %d calls; want ErrBudgetExceeded after 1 (215 tokens, budget 100)", err, m.calls.Load())
	}
}

// The budget survives a resume. A run stops after one model call (120 tokens); a new process
// resumes it with the same budget of 100. The run has already spent its budget, so the resumed
// run must be refused without calling the model.
func TestTokenBudget_HoldsAcrossResume(t *testing.T) {
	store := NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := New(&meteredModel{u: turnUsage}, store, lookupTool(cancel)).WithTokenBudget(1000).Run(ctx, "r1", "q"); !errors.Is(err, context.Canceled) {
		t.Fatalf("setup: %v", err)
	}
	m := &meteredModel{u: turnUsage}
	_, err := New(m, store, lookupTool(func() {})).WithTokenBudget(100).Run(context.Background(), "r1", "q")
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("resumed run: err = %v, want ErrBudgetExceeded (120 tokens already used, budget 100)", err)
	}
	if n := m.calls.Load(); n != 0 {
		t.Fatalf("the resumed run called the model %d times, want 0", n)
	}
}

// Concurrent runs on one agent share no budget state (run under -race).
func TestTokenBudget_ConcurrentRuns(t *testing.T) {
	a := New(&meteredModel{u: turnUsage}, NewMemStore(), lookupTool(func() {})).WithTokenBudget(300)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Run(context.Background(), string(rune('a'+i)), "q"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
