//go:build unix

package browsertest

import (
	"context"
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
	for i := 0; i < 300 && !gone; i++ {
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
	_, err, _ := run(t, "detached", 1500*time.Millisecond)
	if !TimedOut(err) {
		t.Fatalf("err = %v, want a deadline error", err)
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
		if !s.exited(30 * time.Second) {
			t.Errorf("%s was not killed", name)
		}
	}
	if other.exited(500 * time.Millisecond) {
		t.Errorf("the process of the run whose directory begins with the other's was killed")
	}
}

// The directory is matched as text, not as a pattern: a temporary directory with a regexp
// metacharacter in its name (a plus, a bracket) is still found and its process killed.
func TestKillProfileMeta(t *testing.T) {
	dir := t.TempDir() + "/bide-chrome-a+b[c]"
	p := spin(t, "--user-data-dir="+dir)
	killProfile(dir)
	if !p.exited(30 * time.Second) {
		t.Errorf("the process of a directory with regexp metacharacters in its name was not killed")
	}
}

// A script that handles SIGTERM, as puppeteer's handler does, gets to run it before it is killed.
func TestRunAsksTheScriptToStopFirst(t *testing.T) {
	out, err, _ := run(t, "sigterm", 700*time.Millisecond)
	if !TimedOut(err) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if !strings.Contains(string(out), "closing chrome on SIGTERM") || !strings.Contains(err.Error(), "closing chrome on SIGTERM") {
		t.Errorf("the script was not sent SIGTERM before it was killed: stdout %q", out)
	}
}

// killProfile stops looking when its time is up, however slow each scan is: with the process always
// "found" and every pgrep and pkill taking 200ms, a limit counted in rounds would make 20 rounds and
// run for many times the budget. The check counts calls, not seconds, so a slow machine cannot fail
// it: a slower machine only fits fewer rounds in the budget.
func TestKillProfileStopsAtItsDeadline(t *testing.T) {
	oldWait, oldRun := profileWait, runTool
	defer func() { profileWait, runTool = oldWait, oldRun }()
	profileWait = 500 * time.Millisecond
	calls := 0
	runTool = func(ctx context.Context, name string, args ...string) error {
		calls++
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
		}
		return nil // pgrep: still found; pkill: killed
	}
	killProfile("/tmp/bide-chrome-0123456789abcdef")
	// 500ms of budget at 200ms a call is at most 3 or 4 calls; 20 rounds would be 40.
	if calls > 8 {
		t.Fatalf("%d pgrep/pkill calls inside a 500ms budget", calls)
	}
}

// A scan that hangs is cut off with the rest of the budget: the context given to pgrep and pkill ends
// when the budget does.
func TestKillProfileCutsOffAHungScan(t *testing.T) {
	oldWait, oldRun := profileWait, runTool
	defer func() { profileWait, runTool = oldWait, oldRun }()
	profileWait = 300 * time.Millisecond
	cutOff := false
	runTool = func(ctx context.Context, name string, args ...string) error {
		if name != "pgrep" {
			return nil
		}
		select {
		case <-ctx.Done():
			cutOff = true
		case <-time.After(5 * time.Second): // a scan that never ends
		}
		return nil
	}
	killProfile("/tmp/bide-chrome-0123456789abcdef")
	if !cutOff {
		t.Errorf("a hung pgrep was not cut off when the budget ended")
	}
}

// A scan cut off by the budget is not "no process found": the processes it did not get to report are
// still killed, with a pkill that has time of its own. A first scan slower than the budget is what a
// loaded runner produces, and it must not leave the detached Chrome running.
func TestKillProfileStillKillsWhenTheScanOutlastsTheBudget(t *testing.T) {
	oldWait, oldRun := profileWait, runTool
	defer func() { profileWait, runTool = oldWait, oldRun }()
	profileWait = 200 * time.Millisecond
	killed := false
	runTool = func(ctx context.Context, name string, args ...string) error {
		if name == "pkill" {
			killed = ctx.Err() == nil // it must start with time left
			return nil
		}
		<-ctx.Done() // pgrep, as CommandContext ends it: killed, Run returns an error
		return ctx.Err()
	}
	killProfile("/tmp/bide-chrome-0123456789abcdef")
	if !killed {
		t.Errorf("pkill was not run, or ran with no time left, after a scan that outlasted the budget")
	}
}
