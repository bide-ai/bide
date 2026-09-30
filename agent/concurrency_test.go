package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Single-flight: many concurrent Do calls on the SAME (runID,name) run fn exactly once.
// Run with -race to also prove no data race. Prevents double side effects under
// concurrency (parallel tools / retries) — the Durable "at-most-once execution" contract.
func TestMemStore_SingleFlight(t *testing.T) {
	store := NewMemStore()
	var calls int32
	var wg sync.WaitGroup

	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = store.Do(context.Background(), "run", "step", func(context.Context) (Record, error) {
				atomic.AddInt32(&calls, 1)
				time.Sleep(2 * time.Millisecond) // widen the race window
				return Record{Kind: StepValue, Result: json.RawMessage(`1`)}, nil
			})
		}()
	}
	wg.Wait()

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("fn ran %d times under 64 concurrent same-key Do, want 1 (single-flight)", n)
	}
	h, _ := store.History(context.Background(), "run")
	if len(h) != 2 { // the journal header, then the step
		t.Fatalf("history len %d, want the header and 1", len(h))
	}
}

func multiToolTurn(calls ...[2]string) []Emit {
	var e []Emit
	for i, c := range calls {
		e = append(e, Emit{Event: ToolCallDelta{Index: i, ID: c[0], Name: c[1], ArgsFragment: json.RawMessage(`{}`)}})
	}
	return append(e, Emit{Event: Finish{Reason: "tool_use"}})
}

// A turn's tool calls run CONCURRENTLY: three tools rendezvous at a barrier that can only
// be crossed if all three are in flight at once. If execution were sequential, the first
// tool would block at the barrier and time out. Run with -race too.
func TestParallelTools_RunConcurrently(t *testing.T) {
	const n = 3
	var arrived int32
	barrier := make(chan struct{})
	var concurrent [n]bool

	tools := make([]Tool, n)
	for i := 0; i < n; i++ {
		i := i
		tools[i] = Func(fmt.Sprintf("t%d", i), "", Safety{ReadOnly: true},
			func(ctx context.Context, _ struct{}) (struct{}, error) {
				if atomic.AddInt32(&arrived, 1) == n {
					close(barrier) // last one in releases everyone
				}
				select {
				case <-barrier:
					concurrent[i] = true
				case <-time.After(3 * time.Second):
				}
				return struct{}{}, nil
			})
	}

	m := &scriptModel{turns: [][]Emit{
		multiToolTurn([2]string{"c0", "t0"}, [2]string{"c1", "t1"}, [2]string{"c2", "t2"}),
		textTurn("done"),
	}}
	out, err := New(m, NewMemStore(), tools...).Run(context.Background(), "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	if textOf(out) != "done" {
		t.Fatalf("answer = %q, want done", textOf(out))
	}
	for i := 0; i < n; i++ {
		if !concurrent[i] {
			t.Fatalf("tool %d never reached the barrier — tools did not run concurrently", i)
		}
	}
}

// Thread-safety: concurrent Do on DISTINCT keys (the parallel-tools shape) all record,
// with no race. Proves the store is safe for concurrent tool execution.
func TestMemStore_ConcurrentDistinctKeys(t *testing.T) {
	store := NewMemStore()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = store.Do(context.Background(), "run", fmt.Sprintf("s%d", i), func(context.Context) (Record, error) {
				return Record{Kind: StepValue}, nil
			})
		}()
	}
	wg.Wait()

	h, _ := store.History(context.Background(), "run")
	if len(h) != 101 { // the journal header, then the steps
		t.Fatalf("history len %d, want the header and 100 distinct steps", len(h))
	}
}
