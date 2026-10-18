//go:build windows

package relay

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

func TestRetryableRenameClassifiesWindowsErrors(t *testing.T) {
	for _, e := range []syscall.Errno{32, 5} {
		if !retryableRename(&os.LinkError{Op: "rename", Err: e}) {
			t.Errorf("errno %d is not retried", e)
		}
	}
	for _, e := range []syscall.Errno{2, 3, 112} { // not found, path not found, disk full
		if retryableRename(&os.LinkError{Op: "rename", Err: e}) {
			t.Errorf("errno %d is retried", e)
		}
	}
}

// A reader that holds the state file open (an operator's tail, a virus scan) blocks the rename with a sharing
// violation; the save waits for the reader to let go instead of failing.
func TestSaveWaitsForAReaderToCloseTheStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	d, c := newDedupe(10), newIDSet(10)
	s := &stateStore{path: path}
	if err := s.save(d, c); err != nil {
		t.Fatal(err)
	}
	r, err := os.Open(path) // Go opens without FILE_SHARE_DELETE: the rename is refused while this is open
	if err != nil {
		t.Fatal(err)
	}
	d.restore(agent.NotifyID("e1"), []string{"s"})
	time.AfterFunc(60*time.Millisecond, func() { r.Close() })
	if err := s.save(d, c); err != nil {
		r.Close()
		t.Fatalf("the save failed while a reader held the file: %v", err)
	}
	n, err := readSeen(t, path)
	if err != nil || n != 1 {
		t.Fatalf("state file: %d entries, %v", n, err)
	}
}
