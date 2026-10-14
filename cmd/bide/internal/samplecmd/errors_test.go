package samplecmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// notADir is a path below a regular file, which nothing can be created in.
func notADir(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(f, "out")
}

// TestWriteFailures: a directory that cannot be made is an error from WriteFiles, an unreplaceable
// journal is too, and nothing is claimed written.
func TestWriteFailures(t *testing.T) {
	if res, names, err := WriteFiles(notADir(t), false); err == nil || res != nil || names != nil {
		t.Errorf("WriteFiles below a file = %v, %v, %v", res, names, err)
	}
	// -force over a journal that is a directory: removing it fails, and the error is returned.
	dir := t.TempDir()
	db := filepath.Join(dir, journalFile)
	if err := os.MkdirAll(filepath.Join(db, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteFiles(dir, true); err == nil {
		t.Error("-force over a journal that cannot be removed succeeded")
	}
	if _, _, err := WriteFiles(dir, false); err == nil || !strings.Contains(err.Error(), "exists; remove it") {
		t.Errorf("without -force: %v, want the existing journal named", err)
	}
}

// TestMainFailures: a sample that cannot be written, or a viewer that cannot listen, exits 1 with
// the reason on stderr, after only the output it had earned.
func TestMainFailures(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"-out", notADir(t), "-no-browser"}, &out, &errb); code != 1 || !strings.HasPrefix(errb.String(), "bide sample: ") || out.Len() != 0 {
		t.Errorf("unwritable -out: exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
	out.Reset()
	errb.Reset()
	dir := t.TempDir()
	if code := Main([]string{"-out", dir, "-no-browser", "-addr", "0.0.0.0:0"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "bide sample: ") {
		t.Errorf("non-loopback -addr: exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "14 runs written by the real runtime") || strings.Contains(out.String(), "viewer") {
		t.Errorf("stdout = %q, want the written files and no viewer line", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, journalFile)); err != nil {
		t.Errorf("the sample was not left in place: %v", err)
	}
}
