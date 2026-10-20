//go:build unix

package browsertest

import (
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

// killProfile kills every process whose command line names the profile directory: the Chrome puppeteer
// started detached, and its helpers, which are in no process group of ours. The directory must be a
// whole word of the command line (followed by a space, a slash or the end), so that the directory of
// one run does not match a longer path that merely begins with it. It looks again until none is left (a
// killed browser process can be followed by a helper it had just started), up to two seconds.
func killProfile(dir string) {
	pattern := regexp.QuoteMeta(dir) + "([ /]|$)"
	for i := 0; i < 20; i++ {
		if exec.Command("pgrep", "-f", pattern).Run() != nil { // none found
			return
		}
		_ = exec.Command("pkill", "-KILL", "-f", pattern).Run()
		time.Sleep(100 * time.Millisecond)
	}
}
