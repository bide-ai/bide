package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// Under a sustained queue of 64 writers, a lease renewal is not starved. This is an end-to-end
// check; the priority itself is guarded by TestLease_StatementsGoAheadOfQueuedWrites, which fails
// when EnterFirst is replaced by Enter (this test also fails then: see the median check below).
//
// The disk's commit time varies by orders of magnitude between machines (a shared Windows runner
// stalled commits for seconds), so the test sizes the lease TTL from the commit time it measures
// (TTL = 100 commits, within 3s to 30s; a disk slower than that fails with "disk too slow"), and a
// lapsed lease fails the test unless the writers stalled too in the window just before it, which
// makes it the disk, not the queue. A lease cannot outlast a disk stall longer than its TTL (see
// docs/KNOWN-LIMITATIONS.md). The test runs for twice the TTL (6s to 60s) plus the in-flight writes.
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
	ttl := min(max(3*time.Second, 100*commit), 30*time.Second)
	if commit > ttl/10 {
		t.Fatalf("disk too slow for this test: a commit took %v, and the longest TTL it uses is %v", commit, ttl)
	}

	type done struct {
		at time.Time
		d  time.Duration
	}
	var mu sync.Mutex
	var inserts []done // every insert under load, with when it finished
	stop := make(chan struct{})
	var wg sync.WaitGroup
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
				mu.Lock()
				inserts = append(inserts, done{time.Now(), time.Since(t0)})
				mu.Unlock()
			}
		}()
	}
	if ok, err := s.AcquireLease(ctx, "L", "me", ttl); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var renews []time.Duration
	lastOK := time.Now()
	end := time.Now().Add(max(6*time.Second, 2*ttl))
	for time.Now().Before(end) {
		t0 := time.Now()
		ok, err := s.RenewLease(ctx, "L", "me", ttl)
		renews = append(renews, time.Since(t0))
		if err != nil {
			t.Errorf("renew: %v", err)
			break
		}
		if !ok {
			// The lease lapsed: no renewal landed for a whole TTL. It is the disk if the writers
			// stalled in that window too (an insert that finished in it took half a TTL or more);
			// otherwise a renewal was starved.
			var stall time.Duration
			mu.Lock()
			for _, in := range inserts {
				if !in.at.Before(lastOK) {
					stall = max(stall, in.d)
				}
			}
			mu.Unlock()
			if stall < ttl/2 {
				t.Errorf("the lease lapsed after %v without a renewal, but the slowest insert in that window took only %v (ttl %v): the renewal was starved by the writers", time.Since(lastOK), stall, ttl)
			} else {
				t.Logf("the lease lapsed after %v: the disk stalled (an insert took %v in that window, ttl %v)", time.Since(lastOK), stall, ttl)
			}
			break
		}
		lastOK = time.Now()
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	// A renewal waits for the one write inside and runs its own commit, about two commits however
	// many writers queue; an insert waits for the whole queue. If renewals go first, the median
	// renewal is far shorter than the median insert; the medians are robust to a stall on one commit.
	mu.Lock()
	ds := make([]time.Duration, len(inserts))
	for i, in := range inserts {
		ds[i] = in.d
	}
	mu.Unlock()
	if len(ds) == 0 || len(renews) == 0 {
		t.Fatalf("no load or no renewals ran (%d inserts, %d renewals)", len(ds), len(renews))
	}
	slices.Sort(ds)
	slices.Sort(renews)
	mi, mr := ds[len(ds)/2], renews[len(renews)/2]
	t.Logf("ttl %v, unloaded commit %v, %d inserts (median %v), %d renewals (median %v, worst %v)", ttl, commit, len(ds), mi, len(renews), mr, renews[len(renews)-1])
	if mr > mi/2 {
		t.Errorf("median renewal %v against median insert %v: renewals are not going ahead of the queue", mr, mi)
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
