//go:build windows

package relay

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

func TestRenameQuickRetriesClassifiesWindowsErrors(t *testing.T) {
	for e, want := range map[syscall.Errno]int{32: transientRetries, 5: transientRetries / 2, 2: 0, 3: 0, 112: 0} { // 112: disk full
		if got := renameQuickRetries(&os.LinkError{Op: "rename", Err: e}); got != want {
			t.Errorf("errno %d: %d quick retries, want %d", e, got, want)
		}
	}
}

// What MoveFileEx returns when the target is open in a reader that did not share delete (how Go opens a file):
// the error the relay must classify as a refusal that passes. The log names which it is.
func TestRenameOverAnOpenReaderReturnsAClassifiedError(t *testing.T) {
	dir := t.TempDir()
	target, tmp := filepath.Join(dir, "relay.state"), filepath.Join(dir, "relay.state.tmp-1")
	for _, p := range []string{target, tmp} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	err = os.Rename(tmp, target)
	if err == nil {
		t.Fatal("the rename over an open file succeeded")
	}
	t.Logf("rename over an open reader: %v (errno %v)", err, errors.Unwrap(errors.Unwrap(err)))
	if renameQuickRetries(err) == 0 {
		t.Fatalf("the error a reader's open handle causes is not retried: %v", err)
	}
}

// A reader that holds the state file open (an operator's tail, a virus scan) blocks the rename with a sharing
// violation; the save waits for the reader to let go instead of failing.
func TestSaveWaitsForAReaderToCloseTheStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	d, c := newDedupe(10), newIDSet(10)
	s := &stateStore{path: path}
	if err := s.save(d, c, true); err != nil {
		t.Fatal(err)
	}
	r, err := os.Open(path) // Go opens without FILE_SHARE_DELETE: the rename is refused while this is open
	if err != nil {
		t.Fatal(err)
	}
	d.restore(agent.NotifyID("e1"), []string{"s"})
	time.AfterFunc(60*time.Millisecond, func() { r.Close() })
	if err := s.save(d, c, true); err != nil {
		r.Close()
		t.Fatalf("the save failed while a reader held the file: %v", err)
	}
	n, err := readSeen(t, path)
	if err != nil || n != 1 {
		t.Fatalf("state file: %d entries, %v", n, err)
	}
}
