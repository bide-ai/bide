package redislog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/redis/go-redis/v9"
)

// An entity's positions are dense: the event at position p is stored at stream ID "<p+1>-0". A
// stream missing an entry (one deleted with XDEL, or added outside this adapter) cannot say what
// happened at the missing position, so Events refuses it with ErrProtocol rather than return a
// list whose indices are not the positions, which a governor would fold as if they were.
func TestEvents_RefusesAGapInPositions(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("set REDIS_ADDR to run the Redis event-log integration test")
	}
	ctx := context.Background()
	rc := redis.NewClient(&redis.Options{Addr: addr})
	defer rc.Close()
	l := NewWithClient(rc, fmt.Sprintf("gaptest-%d:", time.Now().UnixNano()))
	for i, e := range []string{"e0", "e1", "e2"} {
		if pos, err := l.Append(ctx, "x", "id"+e, e); err != nil || pos != int64(i) {
			t.Fatalf("Append %s = %d, %v", e, pos, err)
		}
	}
	t.Cleanup(func() { rc.Del(context.Background(), l.key("x"), l.idsKey("x")) })
	if err := rc.XDel(ctx, l.key("x"), "2-0").Err(); err != nil {
		t.Fatal(err)
	}
	for _, from := range []int64{0, 1} {
		evs, err := l.Events(ctx, "x", from)
		if !errors.Is(err, agent.ErrProtocol) {
			t.Errorf("Events(from %d) = %q, %v; want ErrProtocol for the missing position 1", from, evs, err)
		}
	}
	if evs, err := l.Events(ctx, "x", 2); err != nil || len(evs) != 1 || evs[0] != "e2" {
		t.Errorf("Events(from 2) = %q, %v; want [e2]", evs, err)
	}
}
