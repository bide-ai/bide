//go:build unix

package browsertest

import (
	"context"
	"os/exec"
	"regexp"
	"syscall"
	"time"
)

// group puts the command in its own process group. When the deadline comes the group gets SIGTERM (a
// puppeteer script closes its Chrome on it) and, if the script has not exited two seconds later, the
// leader is killed by the standard library (WaitDelay); killGroup then kills what is left of the group.
func group(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
}

// killGroup kills every process left in the script's process group.
func killGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }

// profileWait is how long killProfile keeps looking for processes of a profile directory.
var profileWait = 2 * time.Second

// runTool runs pgrep or pkill; a test replaces it to make them slow. It stops the tool when ctx ends,
// so one hung scan cannot outlast the budget.
var runTool = func(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

// killProfile kills every process whose command line names the profile directory: the Chrome puppeteer
// started detached, and its helpers, which are in no process group of ours. The directory must be a
// whole word of the command line (followed by a space, a slash or the end), so that the directory of
// one run does not match a longer path that merely begins with it. It looks again until none is left (a
// killed browser process can be followed by a helper it had just started), until profileWait has passed.
// The limit is a deadline, not a count of rounds: on a loaded machine a single pgrep scan can take
// seconds, and a count of rounds would then run for many times the budget.
func killProfile(dir string) {
	pattern := regexp.QuoteMeta(dir) + "([ /]|$)"
	ctx, cancel := context.WithTimeout(context.Background(), profileWait)
	defer cancel()
	for ctx.Err() == nil {
		if runTool(ctx, "pgrep", "-f", pattern) != nil { // none found (or the budget ended)
			return
		}
		_ = runTool(ctx, "pkill", "-KILL", "-f", pattern)
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}
