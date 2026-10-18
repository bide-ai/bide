package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// stateVersion is the "version" of the state file.
const stateVersion = 1

// stateFile is the JSON of a state file: the IDs of the events the relay saw cleared and the
// delivery IDs it forwarded with the sinks that took each, both oldest first so that loading
// into the bounded sets keeps the newest. It holds IDs and sink names only: no payload, no secret.
type stateFile struct {
	Version int         `json:"version"`
	Cleared []string    `json:"cleared"`
	Seen    []seenState `json:"seen"`
}

type seenState struct {
	ID   string   `json:"id"`
	Done []string `json:"done"`
}

// stateStore saves the relay's memory to one file, atomically (a temporary file in the same
// directory, then a rename), so a crash leaves the old state or the new, never a torn one.
type stateStore struct {
	path string
	mu   sync.Mutex
	lock *os.File // holds the OS lock on path+".lock" for the store's life
	// beforeRename, if set, runs after the temporary file is written and before it replaces the
	// state file; tests use it to stop a save at the torn-write point.
	beforeRename func() error
	afterRename  func() // test hook: runs after the rename, before the directory sync
	syncDir      func(dir string) error
	// renameFile, retryable and sleep stand in for os.Rename, retryableRename and time.Sleep in tests.
	renameFile func(from, to string) error
	retryable  func(error) bool
	sleep      func(time.Duration)
	saves      int
}

// saveCount is how many state files this store has written.
func (s *stateStore) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

// acquire takes the exclusive OS lock that makes this process the only relay on the state file. The
// lock is on a sibling file (path + ".lock"), since a save replaces the state file's inode, and it
// is the operating system's: a process that dies, however it dies, releases it, so there is no
// stale lock to clean up. A second relay on the file gets an error naming the file. It also removes
// temporary files a killed save left (it holds the lock, so none is in use).
func (s *stateStore) acquire() error {
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("relay: opening the state lock: %w", unwrapPath(err))
	}
	held, err := lockFile(f)
	if err != nil {
		f.Close()
		return fmt.Errorf("relay: locking the state file: %w", err)
	}
	if held {
		f.Close()
		return fmt.Errorf("relay: the state file %s is in use by another relay; one file belongs to one relay", s.path)
	}
	s.lock = f
	if stale, _ := filepath.Glob(s.path + ".tmp-*"); len(stale) > 0 {
		for _, p := range stale {
			os.Remove(p)
		}
	}
	return nil
}

// release gives up the lock.
func (s *stateStore) release() error {
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

// load reads the file into d and c. A missing file is a first start; a file that cannot be read
// or parsed is an error, since starting with the memory silently empty is what the file is for
// avoiding.
func (s *stateStore) load(d *dedupe, c *idSet) error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("relay: reading the state file: %w", unwrapPath(err))
	}
	var st stateFile
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("relay: the state file is not valid (%v); fix or remove it", err)
	}
	if st.Version != stateVersion {
		return fmt.Errorf("relay: the state file has version %d, this relay reads %d", st.Version, stateVersion)
	}
	for _, id := range st.Cleared {
		c.add(id)
	}
	for _, e := range st.Seen {
		d.restore(e.ID, e.Done)
	}
	return nil
}

// save writes the current sets. Snapshots are taken under s.mu, so the last save to run holds the
// newest state.
func (s *stateStore) save(d *dedupe, c *idSet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := stateFile{Version: stateVersion, Cleared: c.snapshot(), Seen: d.snapshot()}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("relay: writing the state file: %w", unwrapPath(err))
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil && s.beforeRename != nil {
		werr = s.beforeRename()
	}
	if werr == nil {
		werr = s.rename(tmp, s.path)
	}
	if werr != nil {
		os.Remove(tmp)
		return fmt.Errorf("relay: writing the state file: %w", unwrapPath(werr))
	}
	if s.afterRename != nil {
		s.afterRename()
	}
	sync := s.syncDir
	if sync == nil {
		sync = syncDir
	}
	if err := sync(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("relay: syncing the state directory: %w", unwrapPath(err))
	}
	s.saves++
	return nil
}

// rename replaces the state file with tmp, retrying briefly when the operating system refuses because another
// process has the file open (Windows; see retryableRename).
func (s *stateStore) rename(tmp, path string) error {
	rename, retryable, sleep := s.renameFile, s.retryable, s.sleep
	if rename == nil {
		rename = os.Rename
	}
	if retryable == nil {
		retryable = retryableRename
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	return renameWithRetry(rename, retryable, sleep, tmp, path)
}

func unwrapPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
