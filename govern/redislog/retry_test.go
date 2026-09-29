package redislog_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern/redislog"
	"github.com/redis/go-redis/v9"
)

// loseFirstEvalReply starts a TCP proxy to target that forwards everything, except that the
// first time a client request contains EVAL or EVALSHA it forwards the request, waits for Redis
// to run it, and then drops the connection before relaying the reply. The client sees a network
// error for a script that did run, which is when go-redis retries the command.
func loseFirstEvalReply(t *testing.T, target string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var dropped atomic.Bool
	var wg sync.WaitGroup
	t.Cleanup(func() { ln.Close(); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				client.Close()
				continue
			}
			var cut atomic.Bool
			wg.Add(2)
			go func() { // client -> server
				defer wg.Done()
				defer server.Close()
				buf := make([]byte, 64<<10)
				for {
					n, err := client.Read(buf)
					if n > 0 {
						drop := bytes.Contains(bytes.ToUpper(buf[:n]), []byte("EVAL")) && dropped.CompareAndSwap(false, true)
						if drop {
							cut.Store(true)
						}
						if _, werr := server.Write(buf[:n]); werr != nil {
							return
						}
						if drop {
							time.Sleep(100 * time.Millisecond) // let Redis run the script
							client.Close()
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
			go func() { // server -> client, unless the reply is being lost
				defer wg.Done()
				defer client.Close()
				buf := make([]byte, 64<<10)
				for {
					n, err := server.Read(buf)
					if n > 0 && !cut.Load() {
						if _, werr := client.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// An append whose reply is lost after Redis ran it is retried by go-redis (MaxRetries defaults
// to 3). The retry must not record the event a second time: one Append call is one event, at
// the position the call returns.
func TestRedisLog_AppendRetriedAfterLostReplyRecordsOnce(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("set REDIS_ADDR to run the Redis Streams integration test")
	}
	ctx := context.Background()
	entity := fmt.Sprintf("retry-%s-%d", t.Name(), time.Now().UnixNano())
	direct, err := redislog.Open(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	// Load the script through a direct client first, so the proxied call is a single EVALSHA.
	if _, err := direct.Append(ctx, entity+"-warm", "warm", "warm"); err != nil {
		t.Fatal(err)
	}

	rc := redis.NewClient(&redis.Options{Addr: loseFirstEvalReply(t, addr)})
	defer rc.Close()
	l := redislog.NewWithClient(rc, "govern:")
	pos, err := l.Append(ctx, entity, "charge-1", "charge")
	if err != nil {
		t.Fatalf("Append through a lost reply: %v (go-redis should retry it)", err)
	}
	evs, err := direct.Events(ctx, entity, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || pos != 0 {
		t.Fatalf("one Append call recorded %d events %q and returned position %d; want one event at 0", len(evs), evs, pos)
	}
}

// An entity name may not contain a NUL byte, so no entity's stream can be another entity's
// append-id hash.
func TestRedisLog_EntityWithNULIsRejected(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("set REDIS_ADDR to run the Redis Streams integration test")
	}
	ctx := context.Background()
	l, err := redislog.Open(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	entity := fmt.Sprintf("nul-%s-%d", t.Name(), time.Now().UnixNano())
	if _, err := l.Append(ctx, entity, "a", "e0"); err != nil {
		t.Fatal(err)
	}
	shadow := entity + "\x00append-ids"
	if _, err := l.Append(ctx, shadow, "a", "e0"); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Append to an entity containing NUL: err = %v, want agent.ErrConfig", err)
	}
	if _, err := l.Events(ctx, shadow, 0); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Events of an entity containing NUL: err = %v, want agent.ErrConfig", err)
	}
}
