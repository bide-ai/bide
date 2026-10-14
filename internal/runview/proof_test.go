package runview

import (
	"context"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

func logVerifier(seed string) audit.Verifier {
	return audit.Ed25519Verifier{Pub: key(seed).Public().(ed25519.PublicKey)}
}

func recordsOf(t *testing.T, j *agent.Journal, runID string) []agent.Record {
	t.Helper()
	recs, err := j.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func hasProblem(p *Proof, substr string) bool {
	for _, s := range p.Problems {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// TestHeadProverVerdicts: a signed head verifies a journal it commits to and says so; it fails,
// with the reason, under another key, over another journal, past the clock-skew rule, or when it
// covers no records; it is silent about a run it is not about.
func TestHeadProverVerdicts(t *testing.T) {
	ctx := context.Background()
	j, sth := gatedRun(t, map[string]bool{"ops": true, "finance": true})
	recs := recordsOf(t, j, "g1")
	good := logVerifier("log")

	p := HeadProver(sth, good, Approvals{})(ctx, "g1", recs)
	if !p.OK || p.Source != "signed tree head" || p.Covers != len(recs) || p.Merkle == nil || p.SignedAt.IsZero() ||
		p.Summary != "verifies: all "+itoa(int64(len(recs)))+" records are under the signed head" {
		t.Fatalf("good head: %+v", p)
	}
	if got := HeadProver(sth, good, Approvals{})(ctx, "other-run", recs); got != nil {
		t.Errorf("a head for another run produced %+v", got)
	}

	p = HeadProver(sth, logVerifier("mallory"), Approvals{})(ctx, "g1", recs)
	if p.OK || p.Summary != "does not verify" || p.Merkle != nil || len(p.Problems) == 0 {
		t.Errorf("wrong key: %+v", p)
	}

	other, _ := gatedRun(t, map[string]bool{"ops": true, "finance": false, "risk": false})
	p = HeadProver(sth, good, Approvals{})(ctx, "g1", recordsOf(t, other, "g1"))
	if p.OK || !hasProblem(p, "does not commit to this journal") {
		t.Errorf("head over another journal: %+v", p)
	}

	future := sth.TreeHead
	future.TimestampNanos = time.Now().Add(24 * time.Hour).UnixNano()
	fsth, err := audit.SignTreeHead(future, audit.Ed25519Signer{Priv: key("log")})
	if err != nil {
		t.Fatal(err)
	}
	if p = HeadProver(fsth, good, Approvals{})(ctx, "g1", recs); p.OK || len(p.Problems) == 0 {
		t.Errorf("a head dated a day ahead verified: %+v", p)
	}

	empty := sth.TreeHead
	empty.Size = 0
	esth, err := audit.SignTreeHead(empty, audit.Ed25519Signer{Priv: key("log")})
	if err != nil {
		t.Fatal(err)
	}
	if p = HeadProver(esth, good, Approvals{})(ctx, "g1", recs); p.OK || !hasProblem(p, "covers no records") {
		t.Errorf("a head over no records: %+v", p)
	}
}

// TestHeadProverPartial: records journaled after the head are not covered by it, and the verdict
// says how many.
func TestHeadProverPartial(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	m := agenttest.NewScriptedModel(agenttest.TextTurn("done"))
	if _, err := agenttest.MustNew(m, j).Run(ctx, "p1", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	recs := recordsOf(t, j, "p1")
	th, err := audit.NewTreeHead(ctx, j, "p1", time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: key("log")})
	if err != nil {
		t.Fatal(err)
	}
	// The viewer is shown two records more than the head covers.
	more := append(append([]agent.Record{}, recs...), recs[len(recs)-1], recs[len(recs)-1])
	p := HeadProver(sth, logVerifier("log"), Approvals{})(ctx, "p1", more)
	if p.OK {
		// Extra records beyond the head do not break the inclusion proof of the prefix.
		want := "verifies: records 1 to " + itoa(int64(sth.Size)) + " of " + itoa(int64(len(more))) + " are under the signed head; the 2 after it are not yet covered"
		if p.Summary != want {
			t.Errorf("summary = %q, want %q", p.Summary, want)
		}
	} else {
		t.Errorf("a head over a prefix of the journal does not verify: %+v", p.Problems)
	}
	if got := headTime(audit.SignedTreeHead{}); !got.IsZero() {
		t.Errorf("headTime of an unstamped head = %v, want zero", got)
	}
}

// TestEvidenceProverVerdicts: an evidence package verifies under its log key and over the journal
// it was made from; under another key it fails with the package's own words; for another run it is
// silent.
func TestEvidenceProverVerdicts(t *testing.T) {
	ctx := context.Background()
	j, _ := gatedRun(t, map[string]bool{"ops": true, "finance": true})
	recs := recordsOf(t, j, "g1")
	pkg, err := audit.Evidence(ctx, j, "g1", audit.Ed25519Signer{Priv: key("log")}, time.Now().UnixNano(), audit.WithToolCall("c1"))
	if err != nil {
		t.Fatal(err)
	}
	p := EvidenceProver(pkg, logVerifier("log"), Approvals{})(ctx, "g1", recs)
	if !p.OK || p.Source != "evidence package" || len(p.Items) == 0 {
		t.Fatalf("good package: %+v", p)
	}
	for _, it := range p.Items {
		if strings.Contains(it, "NOT verified") {
			t.Errorf("item %q", it)
		}
	}
	if EvidenceProver(pkg, logVerifier("log"), Approvals{})(ctx, "elsewhere", recs) != nil {
		t.Error("a package for another run produced a verdict")
	}
	p = EvidenceProver(pkg, logVerifier("mallory"), Approvals{})(ctx, "g1", recs)
	if p.OK || !hasProblem(p, "the package did not verify under this key") || p.Summary != "does not verify" {
		t.Errorf("wrong key: %+v", p)
	}
	other, _ := gatedRun(t, map[string]bool{"ops": true, "finance": false, "risk": false})
	p = EvidenceProver(pkg, logVerifier("log"), Approvals{})(ctx, "g1", recordsOf(t, other, "g1"))
	if p.OK || !hasProblem(p, "does not commit to this journal") {
		t.Errorf("package over another journal: %+v", p)
	}
}

// TestApprovalChecksNeedEvidence: a gate whose journal lacks the evidence under the head, or whose
// tally does not parse, is reported as not verified with that reason, never as verified.
func TestApprovalChecksNeedEvidence(t *testing.T) {
	ctx := context.Background()
	j, sth := gatedRun(t, map[string]bool{"ops": true, "finance": true})
	recs := recordsOf(t, j, "g1")

	// A head that covers fewer records than the gate's decisions: no evidence for the gate under it.
	short := HeadProver(sth, logVerifier("log"), Approvals{Keys: approverKeys(nil)})(ctx, "g1", recs[:3])
	c, ok := short.Approvals["c1"]
	if ok && c.OK {
		t.Errorf("a gate verified from a journal cut before its decisions: %+v", c)
	}

	// A tally record that is not a tally.
	bad := make([]agent.Record, len(recs))
	copy(bad, recs)
	got := checkApprovals(corruptTally(t, bad), Approvals{Keys: approverKeys(nil)}, logVerifier("log"), func(string) ([]audit.EvidenceAction, error) { return nil, nil })
	if c := got["c1"]; c.OK || !strings.HasPrefix(c.Text, "signatures not verified: the journaled tally does not read") {
		t.Errorf("corrupt tally: %+v", c)
	}
}

// corruptTally replaces the journaled approval tally's result with text that is not a tally.
func corruptTally(t *testing.T, recs []agent.Record) []agent.Record {
	t.Helper()
	byName := map[string]agent.Record{}
	for _, r := range recs {
		byName[r.Name] = r
	}
	tally, ok := agent.RecordedTally(byName, "c1")
	if !ok {
		t.Fatal("the run journaled no tally")
	}
	for i, r := range recs {
		if r.Name == tally.Name {
			r.Result = []byte(`{"need":"two"}`)
			recs[i] = r
			return recs
		}
	}
	t.Fatal("tally record not found")
	return nil
}
