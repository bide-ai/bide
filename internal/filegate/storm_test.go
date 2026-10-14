package filegate

import (
	"context"
	"math/rand"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Cancelled waiters never leak the slot or break mutual exclusion; For and Close churn is race-free.
func TestReviewCancelStormAndChurn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.db")
	keep := For(p)
	defer keep.Close()
	var inside, maxInside atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := For(p)
			defer h.Close()
			for j := 0; j < 50; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(rand.Intn(300))*time.Microsecond)
				leave, err := h.Enter(ctx)
				cancel()
				if err != nil {
					continue
				}
				n := inside.Add(1)
				for {
					m := maxInside.Load()
					if n <= m || maxInside.CompareAndSwap(m, n) {
						break
					}
				}
				time.Sleep(time.Duration(rand.Intn(50)) * time.Microsecond)
				inside.Add(-1)
				leave()
			}
		}()
	}
	wg.Wait()
	if maxInside.Load() != 1 {
		t.Errorf("%d writers inside at once", maxInside.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	leave, err := keep.Enter(ctx)
	if err != nil {
		t.Fatalf("slot leaked: %v", err)
	}
	leave()
}
