package approvals

import (
	"fmt"
	"testing"
	"time"
)

// The client map stays within its bound however many sources arrive, and a new source never costs a
// pass over every bucket: it looks at a few of the least recently used.
func TestLimiter_BoundedAndEvictsIdleClients(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	l := newLimiter(1, 2, 0, 0, 4, func() time.Time { return now })
	for i := range 4 {
		if ok, _ := l.take(fmt.Sprint("c", i)); !ok {
			t.Fatalf("client %d refused", i)
		}
	}
	// Every bucket has spent a token and not refilled: a fifth client shares the overflow bucket.
	for range 2 {
		l.take("flood-1")
	}
	if len(l.clients) != 4 || l.clients["flood-1"] != nil {
		t.Fatalf("%d clients, flood-1 tracked: %v", len(l.clients), l.clients["flood-1"] != nil)
	}
	if ok, _ := l.take("flood-2"); ok {
		t.Fatal("the overflow bucket let a third request through its burst of 2")
	}
	// Once the buckets have refilled, a new client takes a stale one's place.
	now = now.Add(5 * time.Second)
	if ok, _ := l.take("fresh"); !ok || l.clients["fresh"] == nil || len(l.clients) != 4 {
		t.Fatalf("fresh not tracked after refill: %d clients", len(l.clients))
	}
	if l.recent.Len() != len(l.clients) {
		t.Fatalf("recency list holds %d, map %d", l.recent.Len(), len(l.clients))
	}
	for i := range 20000 {
		l.take(fmt.Sprint("many-", i))
		if len(l.clients) > 4 || l.recent.Len() != len(l.clients) {
			t.Fatalf("after %d: %d clients, %d in the list", i, len(l.clients), l.recent.Len())
		}
	}
}

// The buckets of clients in use stay at the front of the recency list: a new source finds the idle
// ones at the back and takes one's place, instead of being pushed into the shared overflow bucket by
// the busy ones.
func TestLimiter_BusyClientsAreNotTheOnesProbed(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	l := newLimiter(1, 2, 0, 0, 20, func() time.Time { return now })
	for i := range 20 {
		l.take(fmt.Sprint("c", i))
		l.take(fmt.Sprint("c", i)) // drained
	}
	now = now.Add(5 * time.Second) // every bucket has refilled
	for i := range 12 {
		l.take(fmt.Sprint("c", i)) // c0..c11 are in use again
	}
	if ok, _ := l.take("newcomer"); !ok || l.clients["newcomer"] == nil {
		t.Fatalf("the newcomer was not given a bucket of its own (%d clients)", len(l.clients))
	}
	for i := range 12 {
		if l.clients[fmt.Sprint("c", i)] == nil {
			t.Fatalf("busy client c%d was evicted", i)
		}
	}
}
