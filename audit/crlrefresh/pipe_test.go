//go:build unix

package crlrefresh_test

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/crlrefresh"
	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// A CRL file in the archive that is a named pipe (someone put one there) is refused, not waited on.
func TestDirArchiveRead_NamedPipe(t *testing.T) {
	_ = safefiletest.FIFO(t) // skips where no named pipe can exist
	root := t.TempDir()
	var issuer audit.SPKIHash
	dir := filepath.Join(root, "crl", hex.EncodeToString(issuer[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "x.crl"), 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	safefiletest.Deadline(t, "DirArchive.Read", func() {
		if _, err := crlrefresh.DirArchive(root).Read(context.Background(), issuer, crlrefresh.ArchivedCRL{Name: "x.crl"}); err == nil {
			t.Error("DirArchive.Read read a named pipe")
		}
	})
}
