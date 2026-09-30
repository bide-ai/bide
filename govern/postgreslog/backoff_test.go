package postgreslog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The backoff's ceiling starts at backoffBase, doubles with each failure and stops at backoffCap.
func TestBackoffCeiling(t *testing.T) {
	want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond,
		16 * time.Millisecond, 32 * time.Millisecond, 64 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond,
		100 * time.Millisecond, 100 * time.Millisecond}
	var b backoff
	for i, w := range want {
		b.failed = i + 1
		if got := b.ceiling(); got != w {
			t.Errorf("ceiling after %d failures = %v, want %v", i+1, got, w)
		}
	}
	b.failed = 64 // a shift this large would overflow without the guard
	if got := b.ceiling(); got != backoffCap {
		t.Errorf("ceiling after many failures = %v, want the cap %v", got, backoffCap)
	}
}

// wait returns ctx's error at once when ctx is done, and as soon as ctx ends while it waits,
// rather than when the wait would have ended.
func TestBackoffWaitStopsWithTheContext(t *testing.T) {
	defer func(j func(time.Duration) time.Duration) { jitter = j }(jitter)
	waiting := make(chan struct{}, 2)
	jitter = func(time.Duration) time.Duration { // a wait only ctx can end
		waiting <- struct{}{}
		return time.Hour
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var b backoff
	if err := b.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait on a done context = %v, want context.Canceled", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.wait(ctx) }()
	<-waiting // past the check of ctx on entry
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait cancelled while waiting = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wait did not return when its context was cancelled")
	}
}

// jitter draws a wait below its ceiling.
func TestJitterStaysBelowTheCeiling(t *testing.T) {
	for range 1000 {
		if d := jitter(backoffBase); d < 0 || d >= backoffBase {
			t.Fatalf("jitter(%v) = %v, want a duration in [0, %v)", backoffBase, d, backoffBase)
		}
	}
}

// retry keeps running a statement that fails with a serialization failure until ctx is done, and
// then returns an error that wraps both ctx's error and the last attempt's. It stops at once on
// any other error, and on success.
func TestRetryUntilTheContextEnds(t *testing.T) {
	serialization := &pgconn.PgError{Code: serializationFailure}
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := retry(ctx, func() error {
		attempts++
		if attempts == 5 {
			cancel()
		}
		return serialization
	})
	if attempts != 5 || !errors.Is(err, context.Canceled) || !errors.Is(err, serialization) {
		t.Fatalf("retry = %v after %d attempts, want context.Canceled and the serialization failure after 5", err, attempts)
	}

	other := errors.New("other")
	attempts = 0
	if err := retry(context.Background(), func() error { attempts++; return other }); err != other || attempts != 1 {
		t.Fatalf("retry of a non-retryable error = %v after %d attempts, want it after 1", err, attempts)
	}
	attempts = 0
	err = retry(context.Background(), func() error {
		attempts++
		if attempts < 3 {
			return &pgconn.PgError{Code: deadlockDetected}
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("retry = %v after %d attempts, want nil after 3", err, attempts)
	}
}
