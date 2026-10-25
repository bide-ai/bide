package relay

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// On a platform with no exclusive file lock the relay refuses a state file: lockFile there is
// unsupportedLock, which must never report the lock as taken (held=false with a nil error would run two
// relays on one file). The stub is checked here on every OS, since only some build it as lockFile.
func TestUnsupportedLockRefuses(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "state.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	held, err := unsupportedLock(f)
	if err == nil || held {
		t.Fatalf("unsupportedLock = (held %v, err %v); want an error and not held", held, err)
	}
	if !errors.Is(err, errNoFileLock) {
		t.Errorf("err = %v, want errNoFileLock", err)
	}
}
