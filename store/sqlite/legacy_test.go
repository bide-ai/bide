package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}

// A journal file written by v0.7.0 (testdata/v070.db: run "order-1", finished, with one
// side-effect call) keeps its journal in the table "steps", which this version does not read. Open
// refuses the file, rather than open it as empty: a re-invoked finished run would otherwise run
// again from the start and fire its side effect a second time.
func TestOpen_RefusesAV070File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	copyFile(t, filepath.Join("testdata", "v070.db"), path)
	s, err := Open(path)
	if err == nil {
		defer s.Close()
		j := agenttest.MustJournal(s)
		fired := 0
		charge := agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { fired++; return "charged", nil })
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.TextTurn("done"))
		_, err = agenttest.MustNew(m, j, agent.WithTools(charge)).Run(context.Background(), "order-1", agent.UserText("charge me"))
		t.Fatalf("Open of a v0.7.0 file succeeded; a re-invoked finished run then returned %v and fired its effect %d time(s)", err, fired)
	}
	if !errors.Is(err, agent.ErrJournalVersion) {
		t.Fatalf("Open of a v0.7.0 file = %v, want ErrJournalVersion", err)
	}
}

// A file with an empty table "steps" (whatever made it) holds no journal to lose, and opens.
func TestOpen_AnEmptyLegacyTableIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE steps (run_id TEXT NOT NULL, seq INTEGER NOT NULL, name TEXT NOT NULL, data BLOB NOT NULL, PRIMARY KEY (run_id, name))`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open with an empty legacy table = %v", err)
	}
	s.Close()
}

// A table named "steps" that is not a journal (it lacks the journal's columns) is not bide's, and
// the file opens.
func TestOpen_AnUnrelatedStepsTableIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE steps (id INTEGER PRIMARY KEY, label TEXT); INSERT INTO steps (label) VALUES ('mine')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open with an unrelated steps table = %v", err)
	}
	s.Close()
}
