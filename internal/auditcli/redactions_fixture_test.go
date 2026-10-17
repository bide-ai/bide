package auditcli

import (
	"context"
	"crypto/ed25519"
	"slices"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

// redactionFiles are the artifacts of one accountable redaction, for the exit table.
type redactionFiles struct {
	trail, trailCut, trailEmpty, sth string // the redaction trail export, with a record deleted, with none, and its signed head
	journal, journalWhole            string // the redacted journal's export, and the same journal before the redaction
}

func newRedactionFiles(t *testing.T, signer audit.Signer, file func(string, any) string) redactionFiles {
	t.Helper()
	ctx := context.Background()
	st := agent.NewMemStore()
	j := agenttest.MustJournal(st)
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("answer")), j)
	if _, err := a.Run(ctx, "red", agent.UserText("my account is 4111")); err != nil {
		t.Fatal(err)
	}
	var f redactionFiles
	f.journalWhole = file("red-journal-whole.json", exportJournal(t, j, "red"))
	th, err := audit.NewTreeHead(ctx, j, "red", time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	head := signHead(t, th, signer)
	plan, err := agent.PlanRedaction(ctx, j, "red", agent.RedactionSelector{Payloads: true})
	if err != nil {
		t.Fatal(err)
	}
	trail := audit.NewMemEventStore()
	if _, err := audit.RedactJournal(ctx, j, trail, plan, audit.RedactionOptions{Reason: "request", Ticket: "T-1", Executor: "op", Now: time.Now(),
		Signer: signer, Heads: []audit.SignedTreeHead{head}, Verifier: audit.Ed25519Verifier{Pub: ed25519.PublicKey(signer.PublicKey())}}); err != nil {
		t.Fatal(err)
	}
	x, err := audit.ExportNotifyTrail(ctx, trail, audit.RedactionTrailName)
	if err != nil {
		t.Fatal(err)
	}
	sth, err := audit.SignNotifyTrail(ctx, trail, audit.RedactionTrailName, signer, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.trail, f.sth = file("redact-trail.json", x), file("redact-sth.json", sth)
	cut := x
	cut.Leaves = slices.Delete(slices.Clone(x.Leaves), 1, 2)
	f.trailCut = file("redact-trail-cut.json", cut)
	f.trailEmpty = file("redact-trail-empty.json", audit.NotifyTrailExport{Format: x.Format, Trail: x.Trail, Leaves: [][]byte{}})
	f.journal = file("red-journal.json", exportJournal(t, j, "red"))
	return f
}
