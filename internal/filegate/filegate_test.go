package filegate

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Handles on the same file share one gate, however the path is spelled; handles on different files,
// and in-memory databases, do not.
func TestFor_SharesAGatePerFile(t *testing.T) {
	dir := t.TempDir()
	a, b := For(filepath.Join(dir, "x.db")), For(filepath.Join(dir, ".", "sub", "..", "x.db"))
	defer a.Close()
	defer b.Close()
	if a.g != b.g {
		t.Fatal("two spellings of one file got different gates")
	}
	c := For(filepath.Join(dir, "y.db"))
	defer c.Close()
	if c.g == a.g {
		t.Fatal("two files share a gate")
	}
	m1, m2 := For(":memory:"), For(":memory:")
	if m1.g != nil || m2.g != nil {
		t.Fatal("an in-memory database got a gate")
	}
}

// Writers are let in in the order they asked.
func TestEnter_IsFirstComeFirstServed(t *testing.T) {
	h := For(filepath.Join(t.TempDir(), "q.db"))
	defer h.Close()
	hold, err := h.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const n = 20
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leave, err := h.Enter(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			leave()
		}()
		// Let writer i park on the gate before writer i+1 asks; a parked sender cannot be observed,
		// so the test waits long enough for the goroutine to reach it.
		time.Sleep(5 * time.Millisecond)
	}
	hold()
	wg.Wait()
	for i, got := range order {
		if got != i {
			t.Fatalf("writers were let in as %v, want 0..%d in order", order, n-1)
		}
	}
}

// A writer whose context ends while it waits leaves the queue without taking a turn.
func TestEnter_StopsWaitingWhenTheContextEnds(t *testing.T) {
	h := For(filepath.Join(t.TempDir(), "c.db"))
	defer h.Close()
	hold, err := h.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.Enter(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Enter = %v, want the context's deadline error", err)
	}
	hold()
	leave, err := h.Enter(context.Background())
	if err != nil {
		t.Fatalf("Enter after the holder left: %v", err)
	}
	leave()
}

// The registry forgets a file once its last handle closes.
func TestClose_ForgetsTheGate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.db")
	a, b := For(p), For(p)
	a.Close()
	mu.Lock()
	_, there := gates[key(p)]
	mu.Unlock()
	if !there {
		t.Fatal("the gate was forgotten while a handle was open")
	}
	b.Close()
	mu.Lock()
	_, there = gates[key(p)]
	mu.Unlock()
	if there {
		t.Fatal("the gate outlived its last handle")
	}
}
