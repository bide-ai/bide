package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
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
	if werr == nil {
		werr = os.Rename(tmp, s.path)
	}
	if werr != nil {
		os.Remove(tmp)
		return fmt.Errorf("relay: writing the state file: %w", unwrapPath(werr))
	}
	return nil
}

func unwrapPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
