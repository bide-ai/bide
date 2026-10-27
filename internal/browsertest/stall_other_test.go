//go:build !unix

package browsertest

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// There are no process groups, sessions or SIGTERM here (group_other.go kills the script and nothing
// else), so the tests of those are in stall_unix_test.go. What is promised here is that a script that
// outlives its deadline is killed, and that the error says so.

// stall prints nothing more (TestMain wrote a line to each stream), records its pid where the test can
// read it, and waits for ever.
func stall() {
	if f := os.Getenv("BROWSERTEST_PIDFILE"); f != "" {
		os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	time.Sleep(time.Hour)
}

// detached, terminable: the unix tests that use them are not built here.
func detached()   { time.Sleep(time.Hour) }
func terminable() { time.Sleep(time.Hour) }

// gone reports whether no process has the id: opening a process that has exited fails on Windows.
func gone(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	p.Release()
	return false
}

func TestDeadlineKillsTheScript(t *testing.T) {
	pidfile := t.TempDir() + "/pid"
	t.Setenv("BROWSERTEST_PIDFILE", pidfile)
	out, err, _ := run(t, "stall", 5*time.Second)
	var de *DeadlineError
	if !errors.As(err, &de) || !TimedOut(err) {
		t.Fatalf("err = %v, want a *DeadlineError", err)
	}
	if !strings.Contains(err.Error(), "waiting for chrome") || !strings.Contains(string(out), "partial") {
		t.Errorf("the error or the output lost what the script printed: %v, %q", err, out)
	}
	if _, serr := os.Stat(de.Profile); serr == nil {
		t.Errorf("the profile directory %s was not removed", de.Profile)
	}
	b, rerr := os.ReadFile(pidfile)
	if rerr != nil {
		t.Fatalf("the script did not record its pid: %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for i := 0; i < 300 && !gone(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !gone(pid) {
		t.Errorf("the script (pid %d) was still running after the deadline", pid)
	}
}
