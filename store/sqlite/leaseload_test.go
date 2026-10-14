package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Lease statements are not gated: under a sustained queue of journal writers, do they still get the
// write lock within the lease's TTL?
func TestReviewLeaseUnderWriteLoad(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var n atomic.Int64
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, err := s.Insert(ctx, fmt.Sprintf("r%d", g), fmt.Sprintf("s%d", i), []byte("x")); err != nil {
					t.Error(err)
					return
				}
				n.Add(1)
			}
		}()
	}
	if ok, err := s.AcquireLease(ctx, "L", "me", 3*time.Second); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var worst time.Duration
	fails := 0
	end := time.Now().Add(6 * time.Second)
	for time.Now().Before(end) {
		t0 := time.Now()
		ok, err := s.RenewLease(ctx, "L", "me", 3*time.Second)
		d := time.Since(t0)
		worst = max(worst, d)
		if err != nil || !ok {
			fails++
			t.Logf("renew failed after %v: ok=%v err=%v", d, ok, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	t.Logf("%d inserts, worst renew %v, %d failed renewals", n.Load(), worst, fails)
	if fails > 0 {
		t.Errorf("%d lease renewals failed under write load", fails)
	}
}
