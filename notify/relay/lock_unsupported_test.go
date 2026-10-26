package relay

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// requireFileLock skips a test that needs a state file where the platform has no file lock: a relay
// there refuses a StatePath (TestStatePathRefusedWithoutAFileLock).
func requireFileLock(t testing.TB) {
	t.Helper()
	if !haveFileLock {
		t.Skip("this platform has no exclusive file lock, so a relay refuses a state file")
	}
}

// Where there is no file lock, New refuses a StatePath rather than run on a file it cannot lock. The test
// runs only on such a platform (haveFileLock is false there); the stub's own behavior is checked above
// on every OS.
func TestStatePathRefusedWithoutAFileLock(t *testing.T) {
	if haveFileLock {
		t.Skip("this platform has a file lock")
	}
	_, err := New(Config{Secrets: []string{secret}, Sinks: []Sink{&funcSink{name: "s"}}, StatePath: filepath.Join(t.TempDir(), "relay.state")})
	if err == nil || !strings.Contains(err.Error(), "not supported on this platform") {
		t.Fatalf("New with a StatePath and no file lock = %v, want a refusal", err)
	}
}
