//go:build unix

package browsertest

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stall starts a grandchild (as Node starts Chrome), writes its pid where the test can read it, and
// then waits for ever.
func stall() {
	// A child in the script's group that ignores SIGTERM: only killing the group removes it.
	child := exec.Command("sh", "-c", "trap '' TERM; while :; do sleep 1; done")
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

// detached is what puppeteer does with Chrome: a process in a session of its own (so no signal to the
// script's group reaches it) whose command line names the profile directory, started by a script that
// then ignores SIGTERM and spins, as a Node with a stuck event loop does.
func detached() {
	profile := os.Getenv(ProfileEnv)
	child := exec.Command("sh", "-c", "while :; do sleep 1; done", profile)
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		os.Exit(4)
	}
	os.WriteFile(os.Getenv("BROWSERTEST_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
	signal.Ignore(syscall.SIGTERM)
	for {
	}
}

// terminable closes "chrome" on SIGTERM and exits, as a puppeteer script does.
func terminable() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM)
	os.Stdout.WriteString("running\n")
	<-c
	os.Stdout.WriteString("closing chrome on SIGTERM\n")
	os.Exit(0)
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// A script that ignores SIGTERM and has started a Chrome-like process outside its group is stopped
// anyway, the process is killed by its profile directory, and the directory is removed.
func TestRunKillsADetachedChromeOfAScriptThatIgnoresSIGTERM(t *testing.T) {
	pidfile := t.TempDir() + "/pid"
	t.Setenv("BROWSERTEST_PIDFILE", pidfile)
	start := time.Now()
	_, err, _ := run(t, "detached", 1500*time.Millisecond)
	if !TimedOut(err) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Errorf("held the test for %v", took)
	}
	b, rerr := os.ReadFile(pidfile)
	if rerr != nil {
		t.Fatalf("the script did not start its child: %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if alive(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("the detached child %d outlived the run", pid)
	}
	var de *DeadlineError
	if errors.As(err, &de) {
		if _, serr := os.Stat(de.Profile); serr == nil {
			t.Errorf("the profile directory %s was not removed", de.Profile)
		}
	}
}

// spinner is a process that runs until killed; done closes when it has exited and been reaped.
type spinner struct{ done chan struct{} }

func (s spinner) exited(wait time.Duration) bool {
	select {
	case <-s.done:
		return true
	case <-time.After(wait):
		return false
	}
}

func spin(t *testing.T, arg string) spinner {
	t.Helper()
	c := exec.Command("sh", "-c", "while :; do sleep 1; done", arg)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	s := spinner{done: make(chan struct{})}
	go func() { c.Wait(); close(s.done) }()
	t.Cleanup(func() { c.Process.Kill() })
	return s
}

// killProfile matches the directory as a whole word of a command line: cleaning up after the run whose
// directory is a prefix of another's must not take the other run's Chrome with it.
func TestKillProfileLeavesALongerPathAlone(t *testing.T) {
	short := t.TempDir() + "/bide-chrome-123"
	mine := spin(t, "--user-data-dir="+short)
	inside := spin(t, short+"/Default/Cache") // a path inside the directory is the run's own
	other := spin(t, "--user-data-dir="+short+"456789")
	killProfile(short)
	for name, s := range map[string]spinner{"the run's own process": mine, "one of its files": inside} {
		if !s.exited(5 * time.Second) {
			t.Errorf("%s was not killed", name)
		}
	}
	if other.exited(500 * time.Millisecond) {
		t.Errorf("the process of the run whose directory begins with the other's was killed")
	}
}
