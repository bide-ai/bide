package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// The journal projection's event salts derive from the record salts this store persists, so an
// event log built from the journal on a new connection has the root it had before, and a proof
// taken earlier still verifies against it. Skips without PG_DSN.
func TestPostgres_EventLogSurvivesRestart(t *testing.T) {
	s, ctx := openTestStore(t)
	runID := fmt.Sprintf("evlog-%d", time.Now().UnixNano())
	for _, id := range []string{"t1", "t2", "t3"} {
		if _, err := s.Do(ctx, runID, id, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepToolResult, ToolUseID: id, Result: json.RawMessage(`{"ok":true}`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := audit.EventLogFromJournal(ctx, s, runID)
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := before.Prove(1)
	evs, _ := agent.ReplayEvents(ctx, s, runID)
	s.Close()

	s2, err := Open(context.Background(), os.Getenv("PG_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	after, err := audit.EventLogFromJournal(ctx, s2, runID)
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
