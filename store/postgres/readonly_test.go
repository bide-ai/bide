package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// TestOpenReadOnly reads what Open wrote, through a session the server holds read-only: every
// write is refused and the journal is unchanged.
func TestOpenReadOnly(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	ctx := context.Background()
	prefix := fmt.Sprintf("ro%d_", time.Now().UnixNano())
	w, err := Open(ctx, dsn, WithTablePrefix(prefix))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, _, err := w.Insert(ctx, "r1", "a", []byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}

	s, err := OpenReadOnly(ctx, dsn, WithTablePrefix(prefix))
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer s.Close()
	if e, ok, err := s.Get(ctx, "r1", "a"); err != nil || !ok || string(e.Data) != `{"x":1}` {
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
	if ok, err := s.AcquireLease(ctx, "r1", "h", time.Second); ok || err == nil {
		t.Fatalf("AcquireLease on a read-only store = %v, %v, want an error", ok, err)
	}
	n := 0
	for _, err := range w.Load(ctx, "r1", -1) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("the journal holds %d entries after the refused writes, want 1", n)
	}

	if s2, err := OpenReadOnly(ctx, dsn, WithTablePrefix(prefix+"none_")); err == nil {
		s2.Close()
		t.Fatal("OpenReadOnly opened a database with no journal tables under that prefix")
	}
}

// TestReadOnlyDSN sets default_transaction_read_only in both DSN forms, over any value given.
func TestReadOnlyDSN(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://u@h:5432/db?sslmode=disable":              "postgres://u@h:5432/db?default_transaction_read_only=on&sslmode=disable",
		"postgres://u@h/db?default_transaction_read_only=off": "postgres://u@h/db?default_transaction_read_only=on",
		"host=h dbname=db": "host=h dbname=db default_transaction_read_only=on",
		"host=h dbname=db default_transaction_read_only=off": "host=h dbname=db default_transaction_read_only=off default_transaction_read_only=on",
	} {
		if got, err := readOnlyDSN(in); err != nil || got != want {
			t.Errorf("readOnlyDSN(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
