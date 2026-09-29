package redislog_test

import (
	"context"
	"os"
	"testing"

	"github.com/bide-ai/bide/govern"
	"github.com/bide-ai/bide/govern/eventlogtest"
	"github.com/bide-ai/bide/govern/redislog"
)

// The Redis log meets the EventLog contract, with each writer on its own client, as separate
// processes would be.
func TestRedisLog_Conformance(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("set REDIS_ADDR to run the Redis Streams integration test")
	}
	eventlogtest.Run(t, func(t *testing.T) govern.EventLog {
		l, err := redislog.Open(context.Background(), addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return l
	})
}
