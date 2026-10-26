//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package anchorobj_test

import (
	"os"
	"syscall"
	"testing"

	"github.com/bide-ai/bide/audit/anchorobj"
	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// A head file swapped for a named pipe between the check of its type and the open is refused, not
// waited on.
func TestLoadDirRefusesAFileSwappedForANamedPipe(t *testing.T) {
	k := newKeys(t, 1)
	dir := t.TempDir()
	p := write(t, dir, k.head(t, "r", 5, 1, "a"))
	anchorobj.SetAfterTypeCheck(func() {
		if err := os.Remove(p); err != nil {
			t.Error(err)
		}
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Error(err)
		}
	})
	defer anchorobj.SetAfterTypeCheck(nil)
	safefiletest.Deadline(t, "LoadDir", func() {
		if _, err := load(dir, k.v); err == nil {
			t.Error("LoadDir read a named pipe")
		}
	})
}

// A named pipe among the exported objects is refused by LoadDir, without being opened.
func TestLoadDirRefusesANamedPipe(t *testing.T) {
	k := newKeys(t, 1)
	dir := t.TempDir()
	p := write(t, dir, k.head(t, "r", 5, 1, "a"))
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	safefiletest.Deadline(t, "LoadDir", func() {
		if _, err := load(dir, k.v); err == nil {
			t.Error("LoadDir read a named pipe")
		}
	})
}
