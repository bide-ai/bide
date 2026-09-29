// Package eventlogtest checks that an implementation satisfies the govern.EventLog contract, so
// every adapter (in-memory, SQLite, Postgres, Redis, or your own) is held to the same guarantees a
// governor relies on: dense, unique positions under concurrent appends from separate processes, and
// a log that only ever grows at the end.
package eventlogtest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/govern"
)

// Run runs the conformance suite. open returns a handle on the log under test; the suite calls it
// once per concurrent writer, so for a shared backend each call should return an independent
// handle (its own connection or client), standing in for a separate process. Entity names are
// unique per call, so the suite can run repeatedly against a persistent backend.
func Run(t *testing.T, open func(t *testing.T) govern.EventLog) {
	t.Run("Positions", func(t *testing.T) { positions(t, open) })
	t.Run("ConcurrentAppends", func(t *testing.T) { concurrentAppends(t, open) })
}

func entityName(t *testing.T, tag string) string {
	return fmt.Sprintf("eventlogtest-%s-%s-%d", tag, t.Name(), time.Now().UnixNano())
}

// positions: sequential appends are numbered from 0, Events reads from any position, and
// entities are isolated.
func positions(t *testing.T, open func(t *testing.T) govern.EventLog) {
	ctx := context.Background()
	l := open(t)
	a, b := entityName(t, "a"), entityName(t, "b")
	want := []string{"e0", "e1", "e2"}
	for i, e := range want {
		pos, err := l.Append(ctx, a, e)
		if err != nil {
			t.Fatalf("Append(%s): %v", e, err)
		}
		if pos != int64(i) {
			t.Fatalf("Append(%s) returned position %d, want %d", e, pos, i)
		}
	}
	if pos, err := l.Append(ctx, b, "other"); err != nil || pos != 0 {
		t.Fatalf("first Append to another entity: position %d, err %v; want 0", pos, err)
	}
	for from := int64(0); from <= int64(len(want))+1; from++ {
		got, err := l.Events(ctx, a, from)
		if err != nil {
			t.Fatalf("Events(from=%d): %v", from, err)
		}
		exp := []string{}
		if from < int64(len(want)) {
			exp = want[from:]
		}
		if len(got) != len(exp) || (len(exp) > 0 && !slices.Equal(got, exp)) {
			t.Fatalf("Events(from=%d) = %v, want %v", from, got, exp)
		}
	}
	if got, err := l.Events(ctx, b, 0); err != nil || !slices.Equal(got, []string{"other"}) {
		t.Fatalf("other entity's events = %v (%v), want [other]", got, err)
	}
}

// concurrentAppends: writers on independent handles append to one entity at once. Every position
// from 0 to n-1 is returned exactly once, and the event read back at each position is the one that
// was appended there.
func concurrentAppends(t *testing.T, open func(t *testing.T) govern.EventLog) {
	const writers, each = 8, 25
	ctx := context.Background()
	entity := entityName(t, "concurrent")
	handles := make([]govern.EventLog, writers)
	for i := range handles {
		handles[i] = open(t)
	}

	var mu sync.Mutex
	at := map[int64]string{}
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				ev := fmt.Sprintf("w%d-%d", w, i)
				pos, err := handles[w].Append(ctx, entity, ev)
				if err != nil {
					errs <- fmt.Errorf("writer %d append %d: %w", w, i, err)
					return
				}
				mu.Lock()
				if prev, dup := at[pos]; dup {
					mu.Unlock()
					errs <- fmt.Errorf("position %d returned twice (%s and %s)", pos, prev, ev)
					return
				}
				at[pos] = ev
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	n := int64(writers * each)
	events, err := open(t).Events(ctx, entity, 0)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(events)) != n {
		t.Fatalf("log holds %d events, want %d", len(events), n)
	}
	for p := range n {
		ev, ok := at[p]
		if !ok {
			t.Fatalf("no append returned position %d (positions must be dense)", p)
		}
		if events[p] != ev {
			t.Fatalf("event at position %d is %q, but the append that returned %d wrote %q", p, events[p], p, ev)
		}
	}
}
