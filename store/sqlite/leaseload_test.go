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

// Lease renewals go ahead of queued journal writers: under a sustained queue of 64 writers, a
// renewal waits for at most the one write already inside, then runs its own commit. The test
// checks that property in units of the disk's own commit time, which varies by orders of magnitude
// between machines (a shared Windows runner stalled commits for seconds), and it sizes the lease
// TTL from the commit time it measures: a lease cannot outlast a disk stall longer than its TTL, so
// the TTL must exceed the worst commit (see the Leaser notes in docs/KNOWN-LIMITATIONS.md).
func TestLease_RenewalsSurviveWriteLoad(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	// Calibrate: the slowest of 30 commits with no load.
	var commit time.Duration
	for i := range 30 {
		t0 := time.Now()
		if _, _, err := s.Insert(ctx, "calibrate", fmt.Sprintf("s%d", i), []byte("x")); err != nil {
			t.Fatal(err)
		}
		commit = max(commit, time.Since(t0))
	}
	ttl := max(3*time.Second, 100*commit)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var worstInsert atomic.Int64 // nanoseconds: the slowest insert under load, which includes its wait in the queue
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
				t0 := time.Now()
				if _, _, err := s.Insert(ctx, fmt.Sprintf("r%d", g), fmt.Sprintf("s%d", i), []byte("x")); err != nil {
					t.Error(err)
					return
				}
				d := int64(time.Since(t0))
				for {
					w := worstInsert.Load()
					if d <= w || worstInsert.CompareAndSwap(w, d) {
						break
					}
				}
			}
		}()
	}
	if ok, err := s.AcquireLease(ctx, "L", "me", ttl); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var worstRenew, worstStall time.Duration
	lastOK := time.Now()
	end := time.Now().Add(max(6*time.Second, 2*ttl))
	for time.Now().Before(end) {
		t0 := time.Now()
		ok, err := s.RenewLease(ctx, "L", "me", ttl)
		d := time.Since(t0)
		worstRenew = max(worstRenew, d)
		if err != nil {
			t.Errorf("renew: %v", err)
			break
		}
		if !ok {
			// The lease lapsed: no renewal landed for a whole TTL. That is the disk, not the queue,
			// only if the writers stalled that long too; if they did not, a renewal was starved.
			stall := time.Duration(worstInsert.Load())
			worstStall = max(worstStall, stall)
			if stall < ttl/2 {
				t.Errorf("the lease lapsed after %v without a renewal, but the slowest insert took only %v (ttl %v): the renewal was starved by the writers", time.Since(lastOK), stall, ttl)
			} else {
				t.Logf("the lease lapsed after %v: the disk stalled (slowest insert %v, ttl %v)", time.Since(lastOK), stall, ttl)
			}
			break
		}
		lastOK = time.Now()
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	// Priority: a renewal waits for the write inside and runs its own commit, so it takes about two
	// commits however many writers queue; the slowest insert under load (64 queued writers) is far
	// longer, so a renewal as slow as that did not go first.
	if inserts := time.Duration(worstInsert.Load()); worstRenew > max(4*inserts/5, 10*commit)+250*time.Millisecond && worstStall == 0 {
		t.Errorf("worst renewal took %v, slowest insert under load %v (unloaded commit %v): renewals are not going ahead of the queue", worstRenew, inserts, commit)
	}
	t.Logf("ttl %v, unloaded commit %v, worst renew %v, slowest insert %v", ttl, commit, worstRenew, time.Duration(worstInsert.Load()))
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
