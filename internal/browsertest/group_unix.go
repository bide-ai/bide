//go:build unix

package browsertest

import (
	"os/exec"
	"syscall"
	"time"
)

// group puts the command in its own process group and, when its context ends, kills the whole group:
// Node starts Chrome, and a Chrome left behind holds a port and the machine's memory. WaitDelay bounds
// the wait for the output pipes, which a grandchild that survived would hold open.
func group(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
}
