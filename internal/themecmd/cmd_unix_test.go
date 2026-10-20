//go:build unix

package themecmd

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCheckRefusesANamedPipe: bide theme check on a path that names a FIFO fails at once.
func TestCheckRefusesANamedPipe(t *testing.T) {
	p := filepath.Join(t.TempDir(), "theme.json")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	type res struct {
		code int
		errs string
	}
	done := make(chan res, 1)
	go func() {
		code, _, errs := run("check", p)
		done <- res{code, errs}
	}()
	select {
	case r := <-done:
		if r.code != 1 || !strings.Contains(r.errs, "not a regular file") {
			t.Errorf("exit %d, stderr %q; want 1 and a refusal as not a regular file", r.code, r.errs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bide theme check on a named pipe blocked")
	}
}
