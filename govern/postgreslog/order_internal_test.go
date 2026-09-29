package postgreslog

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

// An append-only log must never show a reader an event that later has another event appear
// before it: a reader that remembers how many events it has applied, and applies only the ones
// after that on its next read, would otherwise skip the late one and apply another twice. Here a
// slow append (its transaction still open) and a fast one overlap on the same entity, as two
// nodes appending to one shared log can.
func TestAppend_VisibleOrderIsAppendOnly(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres event-log integration test")
	}
	ctx := context.Background()
	l, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	entity := fmt.Sprintf("pg-order-%s-%d", t.Name(), time.Now().UnixNano())

	// The slow append has inserted its event but not committed yet.
	slow, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendIn(ctx, slow, entity, "slow", "slow"); err != nil {
		t.Fatal(err)
	}
	fast := make(chan error, 1)
	go func() { _, err := l.Append(ctx, entity, "fast", "fast"); fast <- err }()
	time.Sleep(150 * time.Millisecond) // let the fast append commit, or block

	first, err := l.Events(ctx, entity, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := slow.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-fast; err != nil {
		t.Fatal(err)
	}
	second, err := l.Events(ctx, entity, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 {
		t.Fatalf("events = %v, want both appends", second)
	}
	if len(first) > len(second) || !slices.Equal(first, second[:len(first)]) {
		t.Fatalf("a reader saw %v, then later %v: an event appeared before one it had already seen", first, second)
	}
}
