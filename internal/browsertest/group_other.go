//go:build !unix

package browsertest

import (
	"os/exec"
	"time"
)

// group kills the command when its context ends. Without process groups the Chrome it started is
// closed by the script's own browser.close() on a normal exit, and is left to the CI machine on a kill.
func group(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = 2 * time.Second
}

func killGroup(int)      {}
func killProfile(string) {}
