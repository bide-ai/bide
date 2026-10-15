package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// An evidence report names when a proven result was written, if the record says (a halt a
// ResolveHalt resolved), as the writing process's claim; a result that records no time adds
// nothing, and so does one whose recorded_at is not in the canonical form.
func TestEvidence_NotesRecordedAt(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MustJournal(agent.NewMemStore())
	const runID = "run-times"
	put := func(name, id string, sig *agent.ApproverSignature) {
		t.Helper()
		if _, err := journaltest.Do(ctx, j, runID, name, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepToolResult, ToolUseID: id, Result: json.RawMessage(`{"ok":true}`), ApproverSignature: sig}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("tool:stamped", "stamped", &agent.ApproverSignature{RecordedAt: "2026-10-14T09:30:00.125Z"})
	put("tool:plain", "plain", nil)
	put("tool:garbled", "garbled", &agent.ApproverSignature{RecordedAt: "yesterday"})
	if _, err := journaltest.Do(ctx, j, runID, "run:complete", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, ApproverSignature: &agent.ApproverSignature{RecordedAt: "2026-10-14T10:00:00.000Z"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pkg, err := audit.Evidence(ctx, j, runID, edS(priv), 1700000000, audit.WithAllToolCalls(), audit.WithStep("run:complete"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := pkg.Verify(edV(pub), audit.WithVerifyTime(time.Unix(1700000001, 0)))
	if err != nil || !report.OK {
		t.Fatalf("Verify = %+v, %v", report, err)
	}
	notes := map[string]string{}
	for _, it := range report.Items {
		notes[it.Ref] = it.Note
	}
	if want := "tool call included in the signed log; recorded at 2026-10-14T09:30:00.125Z by the clock of the process that wrote it"; notes["stamped"] != want {
		t.Errorf("stamped note = %q, want %q", notes["stamped"], want)
	}
	if want := "step included in the signed log; recorded at 2026-10-14T10:00:00.000Z by the clock of the process that wrote it"; notes["run:complete"] != want {
		t.Errorf("step note = %q, want %q", notes["run:complete"], want)
	}
	for _, id := range []string{"plain", "garbled"} {
		if notes[id] != "tool call included in the signed log" || strings.Contains(notes[id], "recorded") {
			t.Errorf("%s note = %q, want no time", id, notes[id])
		}
	}
}
