//go:build unix

package identitypreflight

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// A CRL file swapped for a named pipe between the check of its type and the open is refused.
func TestLoadFlat_FileSwappedForANamedPipe(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.crl")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterListing = func() {
		os.Remove(p)
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Error(err)
		}
	}
	defer func() { afterListing = nil }()
	safefiletest.Deadline(t, "loadFlat", func() {
		_, _, err := loadFlat(dir, audit.NewStatusSnapshot())
		if err == nil {
			t.Error("loadFlat read a named pipe")
		}
	})
}

// A named pipe among the CRL files is reported as not a regular file and never opened.
func TestLoadFlat_NamedPipe(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "a.crl"), 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	safefiletest.Deadline(t, "loadFlat", func() {
		loaded, rejected, err := loadFlat(dir, audit.NewStatusSnapshot())
		if err != nil || loaded != 0 || len(rejected) != 1 {
			t.Errorf("loadFlat = %d loaded, %v rejected, %v", loaded, rejected, err)
		}
	})
}
