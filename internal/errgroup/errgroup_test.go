package errgroup

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWaitReturnsFirstErrorAndCancels: the first function to fail sets Wait's error and cancels the
// group's context with that error as its cause; a later failure does not replace it.
func TestWaitReturnsFirstErrorAndCancels(t *testing.T) {
	first, second := errors.New("first"), errors.New("second")
	g, ctx := WithContext(context.Background())
	failed := make(chan struct{})
	g.Go(func() error { defer close(failed); return first })
	g.Go(func() error {
		<-failed
		<-ctx.Done() // the first failure cancels the context the other functions run under
		return second
	})
	if err := g.Wait(); err != first {
		t.Fatalf("Wait = %v, want %v", err, first)
	}
	if got := context.Cause(ctx); got != first {
		t.Fatalf("context.Cause = %v, want %v", got, first)
	}
}

// TestWaitCancelsOnSuccess: Wait cancels the context once every function has returned, also when
// none failed, so nothing derived from it outlives the group.
func TestWaitCancelsOnSuccess(t *testing.T) {
	g, ctx := WithContext(context.Background())
	var n atomic.Int32
	for range 5 {
		g.Go(func() error { n.Add(1); return nil })
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	if n.Load() != 5 {
		t.Fatalf("%d functions ran, want 5", n.Load())
	}
	if ctx.Err() == nil {
		t.Fatal("the context is not cancelled after Wait")
	}
	if got := context.Cause(ctx); got != context.Canceled {
		t.Fatalf("context.Cause = %v, want context.Canceled", got)
	}
}

// TestWaitWaitsForAll: Wait returns only after every function has returned, the failing one's
// siblings included.
func TestWaitWaitsForAll(t *testing.T) {
	g, _ := WithContext(context.Background())
	var done atomic.Bool
	g.Go(func() error { return errors.New("fail") })
	g.Go(func() error { time.Sleep(20 * time.Millisecond); done.Store(true); return nil })
	_ = g.Wait()
	if !done.Load() {
		t.Fatal("Wait returned before a sibling finished")
	}
}

// TestParentCancelReachesFunctions: cancelling the parent cancels the group's context.
func TestParentCancelReachesFunctions(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	g, ctx := WithContext(parent)
	g.Go(func() error { <-ctx.Done(); return ctx.Err() })
	cancel()
	if err := g.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
}

// TestSetLimit: with a limit of n, at most n functions run at once, and Go blocks the caller until
// one of them returns.
func TestSetLimit(t *testing.T) {
	const limit, total = 3, 20
	g, _ := WithContext(context.Background())
	g.SetLimit(limit)
	var mu sync.Mutex
	active, peak := 0, 0
	for range total {
		g.Go(func() error {
			mu.Lock()
			active++
			peak = max(peak, active)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	if peak > limit {
		t.Fatalf("%d functions ran at once, limit %d", peak, limit)
	}
	if peak < 2 {
		t.Fatalf("at most %d function ran at once; the limit should allow %d", peak, limit)
	}
}

// TestGoBlocksAtLimit: Go does not return while the group is at its limit.
func TestGoBlocksAtLimit(t *testing.T) {
	g, _ := WithContext(context.Background())
	g.SetLimit(1)
	release := make(chan struct{})
	g.Go(func() error { <-release; return nil })
	started := make(chan struct{})
	go func() {
		g.Go(func() error { return nil })
		close(started)
	}()
	select {
	case <-started:
		t.Fatal("Go returned while the group was at its limit")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-started
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
}

// TestNoLimit: SetLimit with a negative n removes the limit, and the zero Group's default is none.
func TestNoLimit(t *testing.T) {
	g, _ := WithContext(context.Background())
	g.SetLimit(-1)
	release := make(chan struct{})
	var running sync.WaitGroup
	running.Add(10)
	for range 10 {
		g.Go(func() error { running.Done(); <-release; return nil })
	}
	running.Wait() // all ten run at once
	close(release)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
}

// TestSetLimitWhileActivePanics: changing the limit while functions hold a slot panics, as
// x/sync/errgroup does, instead of silently replacing the semaphore those functions release into.
func TestSetLimitWhileActivePanics(t *testing.T) {
	g, _ := WithContext(context.Background())
	g.SetLimit(1)
	release := make(chan struct{})
	g.Go(func() error { <-release; return nil })
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		g.SetLimit(2)
	}()
	if !panicked {
		// The running function would now release its slot into the new semaphore and block, so
		// Wait would never return: stop here.
		t.Fatal("SetLimit with a function running did not panic")
	}
	close(release)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	g.SetLimit(2) // nothing running: allowed
}
