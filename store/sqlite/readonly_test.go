package sqlite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// TestOpenReadOnly reads what Open wrote, refuses every write, and leaves the file unchanged.
func TestOpenReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "j.db")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Insert(ctx, "r1", "a", []byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	s, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer s.Close()
	e, ok, err := s.Get(ctx, "r1", "a")
	if err != nil || !ok || string(e.Data) != `{"x":1}` {
		t.Fatalf("Get = %+v, %v, %v", e, ok, err)
	}
	var runs []string
	for id, err := range s.Runs(ctx, agent.RunFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, id)
	}
	if len(runs) != 1 || runs[0] != "r1" {
		t.Fatalf("Runs = %v", runs)
	}
	if _, _, err := s.Insert(ctx, "r1", "b", []byte(`{}`)); !errors.Is(err, agent.ErrStorage) {
		t.Fatalf("Insert on a read-only store = %v, want an ErrStorage error", err)
	}
	if ok, err := s.AcquireLease(ctx, "r1", "h", 1e9); ok || err == nil {
		t.Fatalf("AcquireLease on a read-only store = %v, %v, want an error", ok, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("OpenReadOnly changed the file")
	}
}

// TestOpenReadOnlyRefusesMissing does not create a file that is not there.
func TestOpenReadOnlyRefusesMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "none.db")
	if s, err := OpenReadOnly(path); err == nil {
		s.Close()
		t.Fatal("OpenReadOnly opened a missing file")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenReadOnly created %s: %v", path, err)
	}
}

// TestOpenReadOnlyWhileWritten reads a journal a writer still has open, and sees its later writes.
func TestOpenReadOnlyWhileWritten(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "j.db")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, _, err := w.Insert(ctx, "r1", "a", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	s, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly beside a writer: %v", err)
	}
	defer s.Close()
	count := func() int {
		n := 0
		for _, err := range s.Load(ctx, "r1", 0) {
			if err != nil {
				t.Fatal(err)
			}
			n++
		}
		return n
	}
	if n := count(); n != 1 {
		t.Fatalf("read %d entries, want 1", n)
	}
	if _, _, err := w.Insert(ctx, "r1", "b", []byte(`{}`)); err != nil {
		t.Fatalf("the writer's insert beside a reader: %v", err)
	}
	if n := count(); n != 2 {
		t.Fatalf("read %d entries after the writer's second insert, want 2", n)
	}
}

// A path that is not a journal, or not there, fails with what to check, and keeps its category.
func TestOpenErrorsSayWhatToCheck(t *testing.T) {
	dir := t.TempDir()
	notDB := filepath.Join(dir, "plain.db")
	if err := os.WriteFile(notDB, []byte("this is not a database, it is just text, padded to be long enough for sqlite to look at"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := OpenReadOnly(notDB)
	if !errors.Is(err, agent.ErrStorage) || !strings.Contains(err.Error(), "not a SQLite database") {
		t.Errorf("OpenReadOnly(text file) = %v; want ErrStorage saying it is not a SQLite database", err)
	}
	_, err = OpenReadOnly(filepath.Join(dir, "missing.db"))
	if !errors.Is(err, agent.ErrStorage) || !strings.Contains(err.Error(), "file exists") {
		t.Errorf("OpenReadOnly(missing) = %v; want ErrStorage saying to check the file exists", err)
	}
	_, err = Open(filepath.Join(dir, "no", "such", "dir", "j.db"))
	if err == nil || !strings.Contains(err.Error(), "directory exists and is writable") {
		t.Errorf("Open(missing directory) = %v; want it to say to check the directory", err)
	}
}
