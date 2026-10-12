package sqlite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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
