//go:build unix

package hsmkeys

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// With no controlling terminal (a new session has none), the prompt is refused, and what is on
// standard input is not taken for the PIN.
func TestPromptRefusedWithoutATerminal(t *testing.T) {
	// Run the check in a child of this test binary that has no controlling terminal.
	c := exec.Command(testBinary(t), "-test.run=^TestPromptChild$")
	c.Env = append(c.Environ(), "HSMKEYS_PROMPT_CHILD=1")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Stdin = strings.NewReader("12345678\n")
	out, err := c.CombinedOutput()
	if ee, ok := errors.AsType[*exec.ExitError](err); err != nil && (!ok || ee.ExitCode() != 0) || !strings.Contains(string(out), "PASS") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// A PIN file that another user owns is refused, whatever its mode (see approvecmd.OpenPrivate).
func TestPINFileMustBeOwnedByYou(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	_, err := pinFromFile("/etc/hosts")()
	if err == nil || !strings.Contains(err.Error(), "owned by another user") || !strings.Contains(err.Error(), "PIN file") {
		t.Fatalf("err = %v", err)
	}
}
