package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// secret is the content the erasure must remove from every file of the database.
const secret = "ACCOUNT-4111-1111-SECRET-MARKER"

func filesHold(t *testing.T, path, marker string) (inDB, inWAL bool) {
	t.Helper()
	db, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wal, err := os.ReadFile(path + "-wal")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return bytes.Contains(db, []byte(marker)), bytes.Contains(wal, []byte(marker))
}

// seededStore opens a store on a file with one finished run whose tool result holds secret, and
// returns it with the bytes of that record. A checkpoint first puts the record in the database file,
// as it would be after a while, so the test sees both places it can be.
func seededStore(t *testing.T) (*Store, string, []byte) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Step(ctx, "r", "x", func(context.Context) (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	// Other records around it, so the page is not rewritten from scratch by the erasure.
	for i := 0; i < 12; i++ {
		fb, _ := agent.JournalEntry("tool:f"+string(rune('a'+i)), agent.Record{Kind: agent.StepToolResult, Result: []byte(`{"filler":"` + string(bytes.Repeat([]byte{byte('a' + i)}, 40)) + `"}`)})
		if _, _, err := s.Insert(ctx, "r", "tool:f"+string(rune('a'+i)), fb); err != nil {
			t.Fatal(err)
		}
	}
	b, err := agent.JournalEntry("tool:a", agent.Record{Kind: agent.StepToolResult, Result: []byte(`{"pii":"` + secret + `","pad":"` + string(bytes.Repeat([]byte("p"), 3000)) + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "r", "tool:a", b); err != nil {
		t.Fatal(err)
	}
	end, _ := agent.JournalEntry("run:complete", agent.Record{Kind: agent.StepValue, Result: []byte(`"ok"`)})
	if _, _, err := s.Insert(ctx, "r", "run:complete", end); err != nil {
		t.Fatal(err)
	}
	if rep, err := s.Compact(ctx); err != nil || !rep.Complete {
		t.Fatalf("setup checkpoint = %+v, %v", rep, err)
	}
	if inDB, _ := filesHold(t, path, secret); !inDB {
		t.Fatal("the record is not in the database file before the erasure: the test cannot tell")
	}
	return s, path, b
}

func erase(t *testing.T, s *Store, b []byte) {
	t.Helper()
	tomb, err := agent.NewTombstone("tool:a", b, time.UnixMilli(1_700_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if done, err := s.Redact(context.Background(), "r", "tool:a", b, tomb); err != nil || !done {
		t.Fatalf("Redact = %v, %v", done, err)
	}
}

// After an erasure and Compact the erased bytes are in neither the database file nor the WAL, even
// with another connection open on the file. Without Compact they are still in the file or the log,
// which is the point of the capability.
func TestCompact_ErasedBytesLeaveEveryFile(t *testing.T) {
	ctx := context.Background()
	s, path, b := seededStore(t)
	// Another connection holds the file open, idle: it does not stop the checkpoint.
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var n int
	if err := other.QueryRowContext(ctx, `SELECT count(*) FROM bide_steps`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	erase(t, s, b)
	if inDB, inWAL := filesHold(t, path, secret); !inDB && !inWAL {
		t.Fatal("the bytes are gone before Compact: the test does not show what Compact is for")
	}
	rep, err := s.Compact(ctx)
	if err != nil || !rep.Complete {
		t.Fatalf("Compact = %+v, %v", rep, err)
	}
	if inDB, inWAL := filesHold(t, path, secret); inDB || inWAL {
		t.Fatalf("after Compact the erased bytes are still in the database file (%v) or the WAL (%v)", inDB, inWAL)
	}
	// The tombstone is what is stored.
	if e, ok, _ := s.Get(ctx, "r", "tool:a"); !ok || !bytes.Contains(e.Data, []byte(`"redacted"`)) {
		t.Fatalf("the entry is %q", e.Data)
	}
}

// A reader that holds a transaction open keeps the checkpoint from finishing: Compact says so and
// does not claim success, and once the reader is closed a second Compact finishes and the bytes go.
func TestCompact_AReaderMakesItReportBusy(t *testing.T) {
	ctx := context.Background()
	s, path, b := seededStore(t)
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT data FROM bide_steps`)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() { // a read snapshot is held while the rows are open
		t.Fatal("no rows")
	}
	erase(t, s, b)
	rep, err := s.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Complete {
		t.Fatalf("Compact claimed success with a reader holding the file: %+v", rep)
	}
	rows.Close()
	tx.Rollback()
	if rep, err = s.Compact(ctx); err != nil || !rep.Complete {
		t.Fatalf("Compact after the reader closed = %+v, %v", rep, err)
	}
	if inDB, inWAL := filesHold(t, path, secret); inDB || inWAL {
		t.Fatalf("after the second Compact the erased bytes remain: db=%v wal=%v", inDB, inWAL)
	}
}
