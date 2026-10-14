package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/dbpath"
)

// Two handles on one file under different spellings (relative path, symlinked directory, symlinked
// file) write concurrently: no deadlock, no BUSY, no lost row.
func TestOpen_AliasedPathsDoNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	os.Mkdir(real, 0o755)
	file := filepath.Join(real, "j.db")
	linkDir := filepath.Join(dir, "linkdir")
	linkFile := filepath.Join(dir, "linkfile.db")
	if os.Symlink(real, linkDir) != nil || os.Symlink(file, linkFile) != nil {
		t.Skip("no symlinks")
	}
	a, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var stores []*Store
	stores = append(stores, a)
	for _, p := range []string{filepath.Join(linkDir, "j.db"), linkFile, filepath.Join(real, "..", "real", "j.db")} {
		s, err := Open(p)
		if err != nil {
			t.Fatal(p, err)
		}
		defer s.Close()
		stores = append(stores, s)
	}
	var wg sync.WaitGroup
	for si, s := range stores {
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 100; i++ {
					if _, _, err := s.Insert(context.Background(), fmt.Sprintf("r%d-%d", si, g), fmt.Sprintf("s%d", i), []byte("x")); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
	}
	wg.Wait()
}

// A hard link cannot be canonicalised, and two handles on the two names corrupt the WAL
// (sqlite.org/howtocorrupt.html), so every way in refuses a file with a second link.
func TestOpen_RefusesAHardLinkedFile(t *testing.T) {
	dir := t.TempDir()
	f, h := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	s, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Link(f, h); err != nil {
		t.Skip("no hard links:", err)
	}
	for _, p := range []string{f, h} {
		if s, err := Open(p); err == nil {
			s.Close()
			t.Errorf("Open(%q) accepted a file with two links", p)
		} else if !errors.Is(err, agent.ErrConfig) || !errors.Is(err, dbpath.ErrHardLinked) {
			t.Errorf("Open(%q) = %v, want ErrConfig and ErrHardLinked", p, err)
		}
		if s, err := OpenReadOnly(p); err == nil {
			s.Close()
			t.Errorf("OpenReadOnly(%q) accepted a file with two links", p)
		} else if !errors.Is(err, agent.ErrConfig) {
			t.Errorf("OpenReadOnly(%q) = %v, want ErrConfig", p, err)
		}
		db, err := sql.Open("sqlite", p+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		if s, err := New(context.Background(), db); err == nil {
			s.Close()
			t.Errorf("New over %q accepted a file with two links", p)
		} else if !errors.Is(err, agent.ErrConfig) {
			t.Errorf("New over %q = %v, want ErrConfig", p, err)
		}
		db.Close()
	}
}
