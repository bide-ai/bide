package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Diagnostic (draft PR): where does a renewal's time go under write load?
func TestDiagLeaseLoad(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	var out strings.Builder
	// Baseline commit latency, no load.
	var base []time.Duration
	for i := range 30 {
		a := time.Now()
		s.Insert(ctx, "base", fmt.Sprintf("s%d", i), []byte("x"))
		base = append(base, time.Since(a))
	}
	slices.Sort(base)
	fmt.Fprintf(&out, "baseline insert p50 %v max %v\n", base[15], base[29])

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var n atomic.Int64
	var mu sync.Mutex
	var ins []time.Duration
	for g := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				a := time.Now()
				s.Insert(ctx, fmt.Sprintf("r%d", g), fmt.Sprintf("s%d", i), []byte("x"))
				d := time.Since(a)
				n.Add(1)
				mu.Lock()
				ins = append(ins, d)
				mu.Unlock()
			}
		}()
	}
	s.AcquireLease(ctx, "L", "me", 3*time.Second)
	begin := time.Now()
	lastOK := time.Now()
	for time.Since(begin) < 6*time.Second {
		a := time.Now()
		leave, _ := s.gate.EnterFirst(ctx)
		waited := time.Since(a)
		leave()
		b := time.Now()
		ok, err := s.RenewLease(ctx, "L", "me", 3*time.Second)
		d := time.Since(b)
		if ok {
			lastOK = time.Now()
		}
		fmt.Fprintf(&out, "t=%5.2fs gate wait %v; renew took %v ok=%v err=%v sinceLastOK=%v\n", time.Since(begin).Seconds(), waited.Round(time.Millisecond), d.Round(time.Millisecond), ok, err, time.Since(lastOK).Round(time.Millisecond))
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	mu.Lock()
	slices.Sort(ins)
	fmt.Fprintf(&out, "%d inserts under load: p50 %v p90 %v max %v\n", len(ins), ins[len(ins)/2], ins[len(ins)*9/10], ins[len(ins)-1])
	mu.Unlock()
	t.Fatalf("DIAG\n%s", out.String())
}
