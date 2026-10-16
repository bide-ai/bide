package runview

import (
	"html"
	"context"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/testca"
)

// The run page and the Needs attention list show the approver identity of a gate with a
// certificate-bound approver (docs/design/approver-identity.md): who, the tier, the basis, the
// status time S, the time the decision became provable, and a dispute. The hard rule: nothing that is
// provisional, still waiting, or missing its identity record is shown in the verified (green) style.

var identT0 = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)

type identFx struct {
	recs   []agent.Record
	digest string
	leaf   string
}

// identRecs is a run paused at a one-approver gate on c1 whose approver "alice" is certificate-bound,
// with her decision, and (when tally) the :ident tally and the identity record carrying the decision
// of the given tier. edit changes the identity record before it is written.
func identRecs(t *testing.T, tier string, tally bool, edit func(*agent.ApprovalIdentity, *agent.ApprovalWait)) identFx {
	t.Helper()
	root := testca.NewRoot(t, "root", testca.Opts{})
	leaf := root.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(c *x509.Certificate) { c.Subject.CommonName = "Alice Example" }})
	lh := agent.CertLeafHash(leaf.DER)
	const digest = "d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d"
	wait := agent.ApprovalWait{ToolUseID: "c1", ToolName: "refund", Need: 1, Approvers: []string{"alice"}, RecordedAt: stamp(identT0),
		IdentityBound: true, IdentityApprovers: []string{"alice"}, IdentityPolicyDigest: digest}
	id := agent.ApprovalIdentity{Format: agent.ApprovalIdentityFormat, ToolUseID: "c1", PolicyDigest: digest, Rules: "v1", Verifier: "go1.27.0",
		Decisions: []agent.IdentityDecision{{Approver: "alice", Step: "approval:c1:alice:1", Identifier: testca.Person(1), LeafHash: hex.EncodeToString(lh[:]),
			AnchoredAt: stamp(identT0.Add(time.Minute)), TimeSource: "log-signer clock", HeadSize: 7, Tier: tier,
			Basis: "status issued after the decision became provable", StatusTime: stamp(identT0.Add(10 * time.Minute)), Window: "1h0m0s"}}}
	if edit != nil {
		edit(&id, &wait)
	}
	tl, _ := json.Marshal(agent.ApprovalTally{Need: 1, Approvers: []string{"alice"}, Approved: 1, ApprovedBy: []string{"alice"}, DecisionFormat: agent.DecisionFormat})
	recs := []agent.Record{
		{Name: "@llm/0", Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "c1", Name: "refund", Args: json.RawMessage(`{}`)}}}},
		rec(agent.ApprovalPendingStep("c1"), wait),
		{Name: "approval:c1:alice:1", Kind: agent.StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &agent.ApproverSignature{Approver: "alice", ApproverAlg: "ed25519",
			Signature: []byte{1}, DecidedAt: stamp(identT0), RecordedAt: stamp(identT0.Add(time.Second)), ApproverCert: leaf.Chain()}},
	}
	if tally {
		recs = append(recs, agent.Record{Name: agent.ApprovalIdentTallyStep("c1"), Kind: agent.StepValue, Result: tl}, rec(agent.ApprovalIdentityStep("c1"), id))
	}
	return identFx{recs: recs, digest: digest, leaf: hex.EncodeToString(lh[:])}
}

func identGate(t *testing.T, recs []agent.Record) Gate {
	t.Helper()
	g := gates(recs, map[string]agent.ToolUse{"c1": {ID: "c1", Name: "refund"}}, []string{"c1"})
	if len(g) != 1 {
		t.Fatalf("gates = %+v", g)
	}
	return g[0]
}

func page(t *testing.T, recs []agent.Record) string {
	t.Helper()
	v := Options{}.viewOf(context.Background(), "r1", recs, nil)
	return html.UnescapeString(pageText(t, v))
}

func TestIdentity_FinalShowsTheTierAndNeverTheBareWordVerified(t *testing.T) {
	for tier, want := range map[string]string{
		agent.TierLogclockFinal:    "final (log signer's clock)",
		agent.TierIndependentFinal: "final (independent time)",
	} {
		fx := identRecs(t, tier, true, nil)
		g := identGate(t, fx.recs)
		gi := g.Identity
		if gi == nil || !gi.Bound || gi.Class != "final" || len(gi.Cards) != 1 {
			t.Fatalf("%s: identity = %+v", tier, gi)
		}
		c := gi.Cards[0]
		if c.Identifier != testca.Person(1) || c.DisplayName != "Alice Example" || c.TierText != want || c.Class != "final" ||
			c.StatusTime != "2026-10-15 09:10:00.000" || c.AnchoredAt != "2026-10-15 09:01:00.000" || c.TimeSource != "log-signer clock" {
			t.Fatalf("%s: card = %+v", tier, c)
		}
		html := page(t, fx.recs)
		for _, w := range []string{testca.Person(1), "Alice Example", want, "status time", "S "} {
			if !strings.Contains(html, w) {
				t.Errorf("%s: page lacks %q", tier, w)
			}
		}
		// Whatever the page says of the identity, it never calls it "verified": the tier is named.
		for _, m := range identBlock.FindAllString(html, -1) {
			if strings.Contains(strings.ToLower(m), "verified") && !strings.Contains(m, "not verified") {
				t.Errorf("%s: identity block says verified: %s", tier, m)
			}
		}
	}
}

// identBlock matches the page's identity blocks.
var identBlock = regexp.MustCompile(`(?s)<div class="aident.*?</div>`)

func TestIdentity_ProvisionalIsNeverShownAsVerified(t *testing.T) {
	fx := identRecs(t, agent.TierProvisional, true, func(id *agent.ApprovalIdentity, _ *agent.ApprovalWait) {
		id.Decisions[0].Basis = "live count: status issued before the decision (provisional only)"
	})
	g := identGate(t, fx.recs)
	if g.Identity.Class != "prov" || g.Identity.Cards[0].Class != "prov" || g.Identity.Cards[0].TierText != "PROVISIONAL" {
		t.Fatalf("identity = %+v", g.Identity)
	}
	if g.Class() == "good" {
		t.Fatalf("a provisional count gives the gate the good style: %q / %q", g.Headline(), g.Class())
	}
	html := page(t, fx.recs)
	if !strings.Contains(html, "PROVISIONAL") || !strings.Contains(html, "live count: status issued before the decision") {
		t.Fatalf("page lacks the tier or the basis")
	}
	// The identity block carries the provisional class, never the final one, and the approver's card
	// is not styled approved (green).
	for _, m := range identBlock.FindAllString(html, -1) {
		if strings.Contains(m, "id-final") || strings.Contains(m, "good") {
			t.Fatalf("provisional block in the final style: %s", m)
		}
		if !strings.Contains(m, "id-prov") {
			t.Fatalf("provisional block lacks its class: %s", m)
		}
	}
	if strings.Contains(html, "a-approved") {
		t.Fatalf("a provisional approver's card is styled approved")
	}
	// The run, finished with an audit proof, is not VERIFIED either.
	v := Options{}.viewOf(context.Background(), "r1", append(fx.recs, agent.Record{Name: "tool:c1", Kind: agent.StepToolResult, ToolUseID: "c1", Result: []byte(`"ok"`)}, agent.Record{Name: "run:complete", Kind: agent.StepValue, Result: []byte(`"done"`)}), nil)
	v.Proof = &Proof{OK: true, Summary: "verifies: 9 records"}
	v.Verdict = runVerdict(v)
	if v.Verdict.Class == "good" || v.Verdict.Word == "VERIFIED" {
		t.Fatalf("verdict = %+v", v.Verdict)
	}
}

func TestIdentity_MissingOrMismatchedRecordIsAProblem(t *testing.T) {
	// The gate says it is identity-bound and counted, but the identity record is gone.
	stripped := identRecs(t, agent.TierLogclockFinal, true, nil)
	var kept []agent.Record
	for _, r := range stripped.recs {
		if r.Name != agent.ApprovalIdentityStep("c1") {
			kept = append(kept, r)
		}
	}
	g := identGate(t, kept)
	if g.Identity.Class != "bad" || !strings.Contains(strings.Join(g.Identity.Problems, "|"), "identity data stripped") {
		t.Fatalf("stripped: %+v", g.Identity)
	}
	if g.Class() == "good" {
		t.Fatalf("stripped gate is good")
	}
	// The record's policy digest is not the one the pause note stated.
	diff := identRecs(t, agent.TierLogclockFinal, true, func(id *agent.ApprovalIdentity, _ *agent.ApprovalWait) { id.PolicyDigest = strings.Repeat("e", 64) })
	g = identGate(t, diff.recs)
	if g.Identity.Class != "bad" || !strings.Contains(strings.Join(g.Identity.Problems, "|"), "policy differs") {
		t.Fatalf("digest mismatch: %+v", g.Identity)
	}
	// A tier the page does not know is not shown as final.
	odd := identRecs(t, "not verified", true, nil)
	if g = identGate(t, odd.recs); g.Identity.Class != "bad" || g.Identity.Cards[0].Class != "bad" {
		t.Fatalf("unknown tier: %+v", g.Identity)
	}
	// A certificate in the decision record that is not the one the identity record names.
	wrongLeaf := identRecs(t, agent.TierLogclockFinal, true, func(id *agent.ApprovalIdentity, _ *agent.ApprovalWait) { id.Decisions[0].LeafHash = strings.Repeat("0", 64) })
	g = identGate(t, wrongLeaf.recs)
	if g.Identity.Class != "bad" || g.Identity.Cards[0].DisplayName != "" {
		t.Fatalf("leaf mismatch: %+v", g.Identity)
	}
}

func TestIdentity_Disputed(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, true, func(id *agent.ApprovalIdentity, _ *agent.ApprovalWait) { id.Decisions[0].Disputed = true })
	g := identGate(t, fx.recs)
	c := g.Identity.Cards[0]
	if g.Identity.Class != "bad" || !c.Disputed || c.Class != "bad" {
		t.Fatalf("disputed: %+v", g.Identity)
	}
	html := page(t, fx.recs)
	if !strings.Contains(html, "DISPUTED") || !strings.Contains(html, "keyCompromise") {
		t.Fatalf("page lacks the dispute")
	}
}

func TestIdentity_PlainTallyOnABoundGateIsSuperseded(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, false, nil)
	tl, _ := json.Marshal(agent.ApprovalTally{Need: 1, Approvers: []string{"alice"}, Approved: 1, ApprovedBy: []string{"alice"}, DecisionFormat: agent.DecisionFormat})
	recs := append(fx.recs, agent.Record{Name: agent.ApprovalTallyStep("c1"), Kind: agent.StepValue, Result: tl})
	g := identGate(t, recs)
	if g.Tallied || len(g.ApprovedBy) != 0 || !strings.Contains(strings.Join(g.Identity.Problems, "|"), "superseded (counted without identity)") {
		t.Fatalf("gate = %+v identity = %+v", g, g.Identity)
	}
}

func TestIdentity_RecordedDecisionIsNotCountedByTheViewer(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, false, nil) // paused, decision recorded, no tally
	g := identGate(t, fx.recs)
	if len(g.ApprovedBy) != 0 || !g.Waiting || g.Identity.Class != "wait" {
		t.Fatalf("gate = %+v identity = %+v", g, g.Identity)
	}
	cs := cards(g, nil)
	if len(cs) != 1 || cs[0].State != "recorded" || cs[0].Verify != "" {
		t.Fatalf("cards = %+v", cs)
	}
	if html := page(t, fx.recs); strings.Contains(html, "a-approved") || !strings.Contains(html, "identity not checked") {
		t.Fatalf("page shows a recorded, unchecked decision as approved")
	}
}

func TestIdentity_PausePhases(t *testing.T) {
	for _, c := range []struct {
		pause, detail, text string
		attention           bool
	}{
		{agent.PauseAwaitingStatus, "no CRL for the issuing CA", "paused: identity status unknown", true},
		{agent.PauseAwaitingAnchor, "", "decision recorded, not yet provable", true},
		{agent.PauseAwaitingFinal, "", "counted provisionally, not final", false},
		{agent.PauseAwaitingDecisions, "", "", true},
	} {
		fx := identRecs(t, agent.TierProvisional, false, func(_ *agent.ApprovalIdentity, w *agent.ApprovalWait) { w.Pause, w.PauseDetail = c.pause, c.detail })
		g := identGate(t, fx.recs)
		if g.Identity.PhaseText != c.text {
			t.Errorf("%s: phase text %q, want %q", c.pause, g.Identity.PhaseText, c.text)
		}
		v := Options{}.viewOf(context.Background(), "r1", fx.recs, nil)
		items := Options{}.runItems(v, fx.recs)
		if len(items) != 1 {
			t.Fatalf("%s: items = %+v", c.pause, items)
		}
		if c.text != "" && !strings.Contains(items[0].Summary, c.text) {
			t.Errorf("%s: summary %q lacks %q", c.pause, items[0].Summary, c.text)
		}
		if c.detail != "" && !strings.Contains(items[0].Summary, c.detail) {
			t.Errorf("%s: summary %q lacks the detail", c.pause, items[0].Summary)
		}
	}
}

func TestIdentity_ExecHoldNeedsAPerson(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, true, func(_ *agent.ApprovalIdentity, w *agent.ApprovalWait) {
		w.Pause, w.PauseDetail = agent.PauseExecHold, "status is older than the execution bound"
	})
	hold := agent.ExecHold{Format: agent.ApprovalExecHoldFormat, ToolUseID: "c1", Reason: agent.ReasonStatusStale, Detail: "status is older than the execution bound", Approver: "alice", CheckedAt: stamp(identT0.Add(time.Hour))}
	recs := append(fx.recs, rec(agent.ApprovalExecHoldStep("c1"), hold))
	g := identGate(t, recs)
	if g.Identity.Hold == nil || g.Identity.Class == "final" || !strings.Contains(g.Identity.PhaseText, "held before execution") {
		t.Fatalf("identity = %+v", g.Identity)
	}
	v := Options{}.viewOf(context.Background(), "r1", recs, nil)
	items := Options{}.runItems(v, recs)
	if len(items) != 1 || !strings.Contains(items[0].Summary, "held before execution") {
		t.Fatalf("items = %+v", items)
	}
	if html := pageText(t, v); !strings.Contains(html, "held before execution") {
		t.Fatal("run page lacks the hold")
	}
}

func TestIdentity_BareKeyGateIsUnchanged(t *testing.T) {
	g := identGate(t, gateRecs(time.Hour))
	if g.Identity != nil {
		t.Fatalf("a gate with no certificate-bound approver has an identity block: %+v", g.Identity)
	}
}

func TestIdentity_NoControlCharactersSurviveIntoThePage(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, true, func(id *agent.ApprovalIdentity, w *agent.ApprovalWait) {
		id.Decisions[0].Basis = "basis\x1b[31mred"
		w.PauseDetail = "detail\x07"
	})
	html := page(t, fx.recs)
	if strings.ContainsAny(html, "\x1b\x07") {
		t.Fatal("a control character reached the page")
	}
}
