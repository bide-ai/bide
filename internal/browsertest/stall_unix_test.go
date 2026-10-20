//go:build unix

package browsertest

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stall starts a grandchild (as Node starts Chrome), writes its pid where the test can read it, and
// then waits for ever.
func stall() {
	child := exec.Command("sleep", "600")
	if err := child.Start(); err != nil {
		os.Exit(4)
	}
	os.WriteFile(os.Getenv("BROWSERTEST_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
	time.Sleep(time.Hour)
}

// The deadline kills the processes the script started, not only the script: a Chrome left running
// holds a port and memory for the rest of the test run.
func TestRunKillsTheScriptsChildren(t *testing.T) {
	pidfile := t.TempDir() + "/pid"
	t.Setenv("BROWSERTEST_PIDFILE", pidfile)
	if _, err, _ := run(t, "stall", 1500*time.Millisecond); err == nil {
		t.Fatal("no deadline error")
	}
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("the script did not start its child: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	gone := false
	for i := 0; i < 50 && !gone; i++ {
		if syscall.Kill(pid, 0) != nil {
			gone = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !gone {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("the child %d of the stalled script is still running", pid)
	}
}
