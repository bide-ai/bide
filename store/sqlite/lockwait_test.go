package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Another process holds the write lock for longer than a few seconds. A step waits its turn
// instead of failing with "database is locked" after its side effect has run.
func TestDo_WaitsOutABusyWriter(t *testing.T) {
	p := filepath.Join(t.TempDir(), "j.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO bide_steps (run_id, seq, name, data) VALUES ('other', 0, 'x', '{}')`); err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(6 * time.Second); tx.Commit() }()
	if _, err := s.Do(context.Background(), "r", "step", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue}, nil
	}); err != nil {
		t.Fatalf("step while another writer held the lock for 6s: %v", err)
	}
}
