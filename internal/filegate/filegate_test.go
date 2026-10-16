package filegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// A lease statement is admitted ahead of every ordinary writer already waiting, and after the one
// inside.
func TestEnterFirst_JumpsTheQueue(t *testing.T) {
	h := For(filepath.Join(t.TempDir(), "p.db"))
	defer h.Close()
	hold, err := h.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	enter := func(name string, first bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := h.Enter
			if first {
				f = h.EnterFirst
			}
			leave, err := f(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			leave()
		}()
		time.Sleep(10 * time.Millisecond) // let it park
	}
	enter("a", false)
	enter("b", false)
	enter("lease1", true)
	enter("lease2", true)
	hold()
	wg.Wait()
	want := []string{"lease1", "lease2", "a", "b"}
	if !slices.Equal(order, want) {
		t.Fatalf("admitted as %v, want %v", order, want)
	}
}

// Spellings of one file share a gate by identity: a relative path after a chdir, a symlink, and a
// file that did not exist when the first handle asked.
func TestFor_KeysByFileIdentity(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "new.db") // does not exist yet
	a := For(p)
	defer a.Close()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("For did not create the file: %v", err)
	}
	b := For(p)
	defer b.Close()
	if a.g != b.g {
		t.Fatal("handles on a file that did not exist got different gates")
	}
	link := filepath.Join(dir, "link.db")
	if os.Symlink(p, link) == nil {
		c := For(link)
		defer c.Close()
		if c.g != a.g {
			t.Fatal("a symlink got its own gate")
		}
	}
	old, _ := os.Getwd()
	defer os.Chdir(old)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	d := For("new.db")
	defer d.Close()
	if d.g != a.g {
		t.Fatal("a relative path got its own gate")
	}
	other := t.TempDir()
	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}
	e := For("new.db") // the same relative name, now a different file
	defer e.Close()
	if e.g == a.g {
		t.Fatal("a different file under the same relative name shared a gate")
	}
}

func TestWaiting_CountsTheQueue(t *testing.T) {
	g := For(filepath.Join(t.TempDir(), "w.db"))
	defer g.Close()
	if n := g.Waiting(); n != 0 {
		t.Fatalf("Waiting on a free gate = %d", n)
	}
	leave, err := g.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := g.Enter(context.Background())
			if err == nil {
				l()
			}
		}()
	}
	for i := 0; g.Waiting() != 3; i++ {
		if i > 5000 {
			t.Fatalf("Waiting = %d, want 3", g.Waiting())
		}
		time.Sleep(time.Millisecond)
	}
	leave()
	wg.Wait()
	if n := g.Waiting(); n != 0 {
		t.Errorf("Waiting after the queue drained = %d", n)
	}
}
