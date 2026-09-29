package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain lets a test run the command's main in a child process (the test binary re-executed
// with BENCH_RUN_MAIN=1), so exit codes and output are checked as a user sees them.
func TestMain(m *testing.M) {
	if os.Getenv("BENCH_RUN_MAIN") == "1" {
		os.Args = append([]string{"bench"}, strings.Fields(os.Getenv("BENCH_ARGS"))...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runBench(t *testing.T, args string) (code int, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "BENCH_RUN_MAIN=1", "BENCH_ARGS="+args)
	var errb strings.Builder
	cmd.Stderr = &errb
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("bench %s did not exit within 10s", args)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), errb.String()
	}
	if err != nil {
		t.Fatalf("bench %s: %v", args, err)
	}
	return 0, errb.String()
}

// A run count or concurrency below 1 is a usage error: exit 2 with a message naming the flag,
// not a panic indexing an empty latency slice (-runs 0) or a hang on an unbuffered semaphore
// (-concurrency 0).
func TestBench_RejectsNonPositiveCounts(t *testing.T) {
	for _, tc := range []struct{ args, flag string }{
		{"-runs 0", "-runs"},
		{"-runs -3", "-runs"},
		{"-concurrency 0", "-concurrency"},
		{"-concurrency -1", "-concurrency"},
	} {
		code, stderr := runBench(t, tc.args)
		if code != 2 || strings.Contains(stderr, "panic: ") || !strings.Contains(stderr, tc.flag) {
			t.Errorf("bench %s: exit %d, stderr %q; want exit 2 and a usage error naming %s", tc.args, code, stderr, tc.flag)
		}
	}
}

// The smallest valid run still works.
func TestBench_OneRun(t *testing.T) {
	if code, stderr := runBench(t, "-runs 1 -concurrency 1"); code != 0 {
		t.Fatalf("bench -runs 1 -concurrency 1: exit %d, stderr %q", code, stderr)
	}
}
