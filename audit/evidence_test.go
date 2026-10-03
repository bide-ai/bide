package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// buildEvidenceRun journals a run with two completed tool calls and one anchored, issuer-signed
// grant, then returns the store, run id, and the log key pair (distinct from the grant issuer's key).
func buildEvidenceRun(t *testing.T) (*agent.Journal, string, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	j := agenttest.MustJournal(store)
	const runID = "run-evidence"

	// Two completed tool calls, recorded as StepToolResult leaves under stable names.
	for _, tc := range []struct{ name, id, result string }{
		{"call:charge", "call-charge", `{"charged":true}`},
		{"call:notify", "call-notify", `{"sent":true}`},
	} {
		tc := tc
		if _, err := journaltest.Do(ctx, j, runID, tc.name, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepToolResult, ToolUseID: tc.id, Result: json.RawMessage(tc.result)}, nil
		}); err != nil {
			t.Fatalf("record tool call %q: %v", tc.id, err)
		}
	}

	// An issuer-signed grant, anchored as a durable leaf so it is provable in the signed log.
	_, issuerPriv, _ := ed25519.GenerateKey(rand.Reader)
	g := audit.Grant{ID: "g1", Issuer: "treasury", Subject: "charge-agent", Scope: map[string]string{"limit_usd": "500"}}
	sg, err := audit.SignGrant(g, audit.Ed25519Signer{Priv: issuerPriv})
	if err != nil {
		t.Fatalf("SignGrant: %v", err)
	}
	if _, err := audit.RecordGrant(ctx, j, runID, sg); err != nil {
		t.Fatalf("RecordGrant: %v", err)
	}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader) // log operator key, distinct from the issuer's
	return j, runID, pub, priv
}

// earlyHead signs, with priv, the journal head runID had after its first n records, standing in for
// an earlier head an auditor took from the anchor log.
func earlyHead(t *testing.T, store *agent.Journal, runID string, n int, priv ed25519.PrivateKey) audit.SignedTreeHead {
	t.Helper()
	ctx := context.Background()
	recs, err := store.History(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	th, err := audit.NewTreeHead(ctx, fixedHistory(recs[:n]).journal(), runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	return signTH(t, th, priv)
}

// TestEvidence_VerifyReportsEachAction: Evidence packages the run's proofs, Verify passes under the
// log key, and the report names each proven action (both tool calls and the anchored grant).
func TestEvidence_VerifyReportsEachAction(t *testing.T) {
	ctx := context.Background()
	store, runID, pub, priv := buildEvidenceRun(t)

	pkg, err := audit.Evidence(ctx, store, runID, edS(priv), 1700000000,
		audit.WithLabel("charge run"),
		audit.WithAllToolCalls(),
		audit.WithGrants(),
		audit.WithConsistencyFrom(earlyHead(t, store, runID, 1, priv)),
	)
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}

	if pkg.Format != audit.EvidenceFormat {
		t.Fatalf("format = %q, want %q", pkg.Format, audit.EvidenceFormat)
	}
	if len(pkg.Actions) != 2 {
		t.Fatalf("got %d actions, want 2 (the two tool calls)", len(pkg.Actions))
	}
	if pkg.Grants == nil || len(pkg.Grants.Chain) != 1 {
		t.Fatalf("grants = %+v, want one grant in the chain", pkg.Grants)
	}
	if pkg.Alg != audit.AlgEd25519 || !bytes.Equal(pkg.PublicKey, pub) {
		t.Fatalf("package names key %s %x, want the signer's ed25519 key", pkg.Alg, pkg.PublicKey)
	}

	report, err := pkg.Verify(edV(pub))
	if err := reportErr(report.OK, err); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.OK {
		t.Fatalf("package did not verify: %+v", report.Items)
	}
	if !report.STHVerified {
		t.Fatal("STH not reported verified")
	}

	// Every proven item must appear in the report, verified, with a note.
	byRef := map[string]audit.EvidenceItem{}
	for _, it := range report.Items {
		if !it.Verified {
			t.Fatalf("item %q not verified: %s", it.Label, it.Note)
		}
		if it.Note == "" {
			t.Fatalf("item %q has no note", it.Label)
		}
		byRef[it.Ref] = it
	}
	for _, id := range []string{"call-charge", "call-notify"} {
		if _, ok := byRef[id]; !ok {
			t.Fatalf("report is missing tool call %q", id)
		}
	}
	// The consistency item is reported and verified between the two signed heads.
	sawConsistency := false
	for _, it := range report.Items {
		if it.Kind == "consistency" {
			sawConsistency = true
		}
	}
	if !sawConsistency {
		t.Fatal("consistency item missing from report")
	}

	// Wrong key: the whole package fails.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if wrong, _ := pkg.Verify(edV(otherPub)); wrong.OK {
		t.Fatal("package verified under the wrong key")
	}
}

// TestEvidence_DefaultsToAllToolCalls: with no action option, Evidence proves every completed tool call.
func TestEvidence_DefaultsToAllToolCalls(t *testing.T) {
	ctx := context.Background()
	store, runID, pub, priv := buildEvidenceRun(t)

	pkg, err := audit.Evidence(ctx, store, runID, edS(priv), 1700000000)
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if len(pkg.Actions) != 2 {
		t.Fatalf("default packaged %d actions, want 2 (all tool calls)", len(pkg.Actions))
	}
	report, err := pkg.Verify(edV(pub))
	if err != nil || !report.OK {
		t.Fatalf("default package did not verify (ok=%v err=%v)", report.OK, err)
	}
}

// TestEvidence_TamperFailsVerify: flipping a byte in a proven bundle's record breaks Verify, and the
// tampered item is reported unverified.
func TestEvidence_TamperFailsVerify(t *testing.T) {
	ctx := context.Background()
	store, runID, pub, priv := buildEvidenceRun(t)

	pkg, err := audit.Evidence(ctx, store, runID, edS(priv), 1700000000, audit.WithAllToolCalls())
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}

	// Tamper: alter the disclosed result of the first action's record.
	editRec(t, &pkg.Actions[0].Bundle, func(r *agent.Record) { r.Result = json.RawMessage(`{"charged":false}`) })

	report, err := pkg.Verify(edV(pub))
	if err := reportErr(report.OK, err); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.OK {
		t.Fatal("Verify passed on a tampered package")
	}
	if report.Items[0].Verified {
		t.Fatal("the tampered action was reported verified")
	}
}

// TestEvidence_JSONRoundTrip: the package marshals and unmarshals through JSON with no loss.
func TestEvidence_JSONRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, runID, _, priv := buildEvidenceRun(t)

	pkg, err := audit.Evidence(ctx, store, runID, edS(priv), 1700000000,
		audit.WithAllToolCalls(),
		audit.WithGrants(),
		audit.WithConsistencyFrom(earlyHead(t, store, runID, 2, priv)),
	)
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}

	b, err := json.Marshal(pkg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round audit.EvidencePackage
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	stripRaw(&pkg) // the records' stored bytes are not part of the package's JSON
	if !reflect.DeepEqual(pkg, round) {
		t.Fatalf("round trip lost data:\n got: %+v\nwant: %+v", round, pkg)
	}
	var strict audit.EvidencePackage
	if err := audit.UnmarshalStrict(b, &strict); err != nil {
		t.Fatalf("strict unmarshal: %v", err)
	}
	if !reflect.DeepEqual(pkg, strict) {
		t.Fatalf("strict round trip lost data:\n got: %+v\nwant: %+v", strict, pkg)
	}
}
