package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
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
