package flowedit

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// swapped runs readConfig on a config that is replaced, between the check of its name and the open,
// by what replace makes of the path. It fails if readConfig has not returned in 5 s.
func swapped(t *testing.T, replace func(path string) error) error {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	afterLstat = func() {
		p := filepath.Join(dir, "f.json")
		if err := os.Remove(p); err != nil {
			t.Error(err)
		}
		if err := replace(p); err != nil {
			t.Error(err)
		}
	}
	defer func() { afterLstat = nil }()
	done := make(chan error, 1)
	go func() { _, err := readConfig(root, "f.json", "f.json"); done <- err }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("readConfig blocked on the file swapped in before the open")
		return nil
	}
}

// A config replaced by a directory between the check and the open is refused, on every platform.
func TestReadConfig_SwappedForADirectory(t *testing.T) {
	err := swapped(t, func(p string) error { return os.Mkdir(p, 0o700) })
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("readConfig = %v, want a refusal", err)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readConfig = %v", err)
	}
}

// A config replaced by a named pipe between the check and the open does not stop the editor.
func TestReadConfig_SwappedForANamedPipe(t *testing.T) {
	if !haveFIFO {
		t.Skip("no named pipe can sit at a file path on " + runtime.GOOS)
	}
	err := swapped(t, mkfifo)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("readConfig = %v, want a refusal", err)
	}
}
