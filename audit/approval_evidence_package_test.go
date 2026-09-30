package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
)

// ApprovalEvidence's output is meant to be appended to an EvidencePackage (its doc says so, and
// bide-audit verify-approvals reads it from one). Every entry it emits must therefore verify as a
// package action too: its trailing "tool" entry is labelled as EvidencePackage.Verify requires of a
// tool action, by the call's tool-use id.
func TestApprovalEvidence_VerifiesInsideAnEvidencePackage(t *testing.T) {
	g := passedGate(t)
	ctx := context.Background()
	ts := time.Now().Add(-time.Hour).UnixNano()
	pkg, err := audit.Evidence(ctx, g.store, gateRun, edS(g.logPriv), ts)
	if err != nil {
		t.Fatal(err)
	}
	acts, err := audit.ApprovalEvidence(ctx, g.store, gateRun, "c1", pkg.STH)
	if err != nil {
		t.Fatal(err)
	}
	pkg.Actions = acts // the approval evidence alone, request to result
	if err := pkg.Seal(edS(g.logPriv)); err != nil {
		t.Fatal(err)
	}
	rep, err := pkg.Verify(edV(g.logPub))
	if err != nil || !rep.OK {
		t.Fatalf("a package of ApprovalEvidence's output does not verify: %+v", rep.Items)
	}
}
