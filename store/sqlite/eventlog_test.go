package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// The journal projection's event salts derive from the record salts this store persists, so an
// event log built from the journal after a restart has the root it had before, and a proof taken
// before the restart still verifies against it.
func TestSQLite_EventLogSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		if _, err := s.Do(ctx, "run", id, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepToolResult, ToolUseID: id, Result: json.RawMessage(`{"ok":true}`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := audit.EventLogFromJournal(ctx, s, "run")
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := before.Prove(1)
	evs, _ := agent.ReplayEvents(ctx, s, "run")
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := audit.EventLogFromJournal(ctx, s, "run")
	if err != nil {
		t.Fatal(err)
	}
	if after.Len() != 3 || !bytes.Equal(after.Root(), before.Root()) {
		t.Fatalf("the event log's root changed across a restart (%d events)", after.Len())
	}
	if ok, err := audit.VerifyEventInclusion(after.Root(), evs[1], proof); !ok || err != nil {
		t.Fatalf("a proof from before the restart does not verify after it: %v, %v", ok, err)
	}
}
