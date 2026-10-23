package redislog

import (
	"context"
	"crypto/fips140"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Importing this package must not panic under GODEBUG=fips140=only (redis.NewScript hashes a script
// with SHA-1, which that mode refuses; the script is made with NewScriptServerSHA instead). Before
// that, the test binary itself panicked in package init, and this test could not run.
func TestAppendScriptNeedsNoClientSideSHA1(t *testing.T) {
	if appendScript == nil {
		t.Fatal("appendScript is nil")
	}
	if h := appendScript.Hash(); h != "" {
		t.Fatalf("appendScript.Hash() = %q before any Redis round trip: the digest was computed in Go", h)
	}
	t.Logf("fips140 enabled=%v enforced=%v", fips140.Enabled(), fips140.Enforced())
}

// redisForTest returns a client on REDIS_ADDR, or on a throwaway redis-server this test starts when
// REDIS_ADDR is unset and the binary is on PATH.
func redisForTest(t *testing.T, hooks ...redis.Hook) *Log {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		bin, err := exec.LookPath("redis-server")
		if err != nil {
			t.Skip("set REDIS_ADDR, or put redis-server on PATH, to run the script round-trip tests")
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr = l.Addr().String()
		_ = l.Close()
		_, port, _ := net.SplitHostPort(addr)
		cmd := exec.Command(bin, "--port", port, "--bind", "127.0.0.1", "--save", "", "--appendonly", "no")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	}
	rc := redis.NewClient(&redis.Options{Addr: addr})
	for deadline := time.Now().Add(10 * time.Second); rc.Ping(context.Background()).Err() != nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no Redis at %s", addr)
		}
	}
	for _, h := range hooks {
		rc.AddHook(h)
	}
	t.Cleanup(func() { _ = rc.Close() })
	return NewWithClient(rc, fmt.Sprintf("fips-%d:", time.Now().UnixNano()))
}

// Append works after the script cache is flushed (a restart or failover answers EVALSHA with NOSCRIPT).
func TestAppendAfterScriptFlush(t *testing.T) {
	ctx := context.Background()
	l := redisForTest(t)
	if pos, err := l.Append(ctx, "e", "id1", "ev1"); err != nil || pos != 0 {
		t.Fatalf("first append = %d, %v", pos, err)
	}
	if err := l.rc.ScriptFlush(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	if pos, err := l.Append(ctx, "e", "id2", "ev2"); err != nil || pos != 1 {
		t.Fatalf("append after SCRIPT FLUSH = %d, %v, want position 1", pos, err)
	}
	if pos, err := l.Append(ctx, "e", "id1", "ev1"); err != nil || pos != 0 {
		t.Fatalf("a repeated append id = %d, %v, want its recorded position 0", pos, err)
	}
}

type refuseScript struct{}

func (refuseScript) DialHook(next redis.DialHook) redis.DialHook { return next }
func (refuseScript) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (refuseScript) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		switch cmd.Name() {
		case "script", "evalsha":
			err := fmt.Errorf("ERR unknown command '%s' (renamed or disabled)", cmd.Name())
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

// Append works on a Redis that refuses SCRIPT and EVALSHA (renamed or disabled): the script runs with EVAL.
func TestAppendWhenScriptCommandsAreRefused(t *testing.T) {
	ctx := context.Background()
	old := appendScript
	appendScript = redis.NewScriptServerSHA(appendScriptSource) // no digest cached by an earlier test
	t.Cleanup(func() { appendScript = old })
	l := redisForTest(t, refuseScript{})
	for i, want := range []int64{0, 1} {
		if pos, err := l.Append(ctx, "e", fmt.Sprintf("id%d", i), "ev"); err != nil || pos != want {
			t.Fatalf("append %d = %d, %v, want %d", i, pos, err, want)
		}
	}
}
