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

// Lease renewals go ahead of queued journal writers: under a sustained queue of 64 writers, every
// renewal still gets the write lock well within the lease TTL.
func TestLease_RenewalsSurviveWriteLoad(t *testing.T) {
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

// A lease statement is admitted ahead of journal writes already queued on the file.
func TestLease_StatementsGoAheadOfQueuedWrites(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if ok, err := s.AcquireLease(ctx, "L", "me", time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	hold, err := s.gate.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	run := func(name string, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil {
				t.Error(name, err)
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
		}()
		time.Sleep(20 * time.Millisecond) // let it park on the gate
	}
	run("insert", func() error { _, _, err := s.Insert(ctx, "r", "s", []byte("x")); return err })
	run("renew", func() error { _, err := s.RenewLease(ctx, "L", "me", time.Minute); return err })
	hold()
	wg.Wait()
	if len(order) != 2 || order[0] != "renew" {
		t.Fatalf("completed in order %v, want the renewal first", order)
	}
}
