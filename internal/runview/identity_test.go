package runview

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/http/httptest"
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
	recs        []agent.Record
	digest      string
	leaf        string
	tallyDigest string
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
	tl, _ := json.Marshal(agent.ApprovalTally{Need: 1, Approvers: []string{"alice"}, Approved: 1, ApprovedBy: []string{"alice"}, DecisionFormat: agent.DecisionFormat})
	td := sha256.Sum256(tl)
	id := agent.ApprovalIdentity{Format: agent.ApprovalIdentityFormat, ToolUseID: "c1", Tally: agent.ApprovalIdentTallyStep("c1", 0), TallyDigest: hex.EncodeToString(td[:]),
		Evaluated: agent.EvaluatedLive, PolicyDigest: digest, Rules: "v1", Verifier: "go1.27.0",
		Decisions: []agent.IdentityDecision{{Approver: "alice", Step: "approval:c1:alice:1", Identifier: testca.Person(1), LeafHash: hex.EncodeToString(lh[:]),
			AnchoredAt: stamp(identT0.Add(time.Minute)), TimeSource: agent.TimeSourceLogSignerClock, HeadSize: 7, Tier: tier, BasisKind: agent.BasisAtOrAfterDecision,
			Basis: "status issued after the decision became provable", StatusTime: stamp(identT0.Add(10 * time.Minute)), Window: "1h0m0s"}}}
	if tier == agent.TierProvisional {
		id.Decisions[0].BasisKind = agent.BasisLiveBeforeDecision
	}
	if edit != nil {
		edit(&id, &wait)
	}
	recs := []agent.Record{
		{Name: "@llm/0", Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "c1", Name: "refund", Args: json.RawMessage(`{}`)}}}},
		rec(agent.ApprovalPendingStep("c1"), wait),
		{Name: "approval:c1:alice:1", Kind: agent.StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &agent.ApproverSignature{Approver: "alice", ApproverAlg: "ed25519",
			Signature: []byte{1}, DecidedAt: stamp(identT0), RecordedAt: stamp(identT0.Add(time.Second)), ApproverCert: leaf.Chain()}},
	}
	if tally {
		recs = append(recs, agent.Record{Name: agent.ApprovalIdentTallyStep("c1", 0), Kind: agent.StepValue, Result: tl}, rec(agent.ApprovalIdentityStep("c1", 0), id))
	}
	return identFx{recs: recs, digest: digest, leaf: hex.EncodeToString(lh[:]), tallyDigest: hex.EncodeToString(td[:])}
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
		agent.TierLogclockFinal:    "final as recorded (log signer's clock)",
		agent.TierIndependentFinal: "final as recorded (independent time)",
	} {
		fx := identRecs(t, tier, true, nil)
		g := identGate(t, fx.recs)
		gi := g.Identity
		if gi == nil || !gi.Bound || gi.Class != "final" || len(gi.Cards) != 1 {
			t.Fatalf("%s: identity = %+v", tier, gi)
		}
		c := gi.Cards[0]
		if c.Identifier != testca.Person(1) || c.DisplayName != "Alice Example" || c.TierText != want || c.Class != "final" ||
			c.StatusTime != "2026-10-15 09:10:00.000" || c.AnchoredAt != "2026-10-15 09:01:00.000" || c.TimeSource != "log signer's clock" {
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
			if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(m), "not re-verified", ""), "not verified", ""), "verified") {
				t.Errorf("%s: identity block says verified: %s", tier, m)
			}
		}
	}
}

// identBlock matches the page's identity blocks.
var identBlock = regexp.MustCompile(`(?s)<div class="aident.*?</dl>.*?</div>`)

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
		if r.Name != agent.ApprovalIdentityStep("c1", 0) {
			kept = append(kept, r)
		}
	}
	g := identGate(t, kept)
	if g.Identity.Class != "bad" || !strings.Contains(strings.Join(g.Identity.Problems, "|"), "incomplete identity data") {
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
	wrongLeaf := identRecs(t, agent.TierLogclockFinal, true, func(id *agent.ApprovalIdentity, _ *agent.ApprovalWait) {
		id.Decisions[0].LeafHash = strings.Repeat("0", 64)
	})
	g = identGate(t, wrongLeaf.recs)
	if g.Identity.Class != "bad" || g.Identity.Cards[0].DisplayName != "" {
		t.Fatalf("leaf mismatch: %+v", g.Identity)
	}
}

// Disputed and audited come only from an audit result the viewer is given, never from the journal.
func TestIdentity_DisputedAndAuditedNeedAnAuditResult(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, true, nil)
	view := func(res map[string]IdentityAudit) *View {
		v := Options{Provers: []Prover{func(context.Context, string, []agent.Record) *Proof {
			return &Proof{OK: true, Summary: "verifies: 9 records", Approvals: map[string]ApprovalCheck{"c1": {Identity: res}}}
		}}}.viewOf(context.Background(), "r1", fx.recs, nil)
		return v
	}
	// No audit result: final as recorded, in the neutral style, never audited (green).
	if g := view(nil).Gates[0]; g.Identity.Class != "final" || g.Identity.Cards[0].Audited {
		t.Fatalf("no audit: %+v", g.Identity)
	}
	// An audit that agrees makes it the only green.
	g := view(map[string]IdentityAudit{"alice": {OK: true, TallyDigest: fx.tallyDigest, PolicyDigest: fx.digest, CheckedAt: "2026-10-15T10:00:00.000Z"}}).Gates[0]
	if g.Identity.Class != "audited" || !g.Identity.Cards[0].Audited || g.Class() != "good" {
		t.Fatalf("audited: %+v / %q", g.Identity, g.Class())
	}
	// An audit that disputes it: red, and it says why.
	v := view(map[string]IdentityAudit{"alice": {Disputed: true, Detail: "keyCompromise effective 2026-10-15T08:59:00Z", TallyDigest: fx.tallyDigest, PolicyDigest: fx.digest}})
	g = v.Gates[0]
	c := g.Identity.Cards[0]
	if g.Identity.Class != "bad" || !c.Disputed || c.Class != "bad" || g.Class() == "good" {
		t.Fatalf("disputed: %+v", g.Identity)
	}
	if v.Verdict.Class == "good" {
		t.Fatalf("a disputed identity under a VERIFIED verdict: %+v", v.Verdict)
	}
	html := html.UnescapeString(pageText(t, v))
	if !strings.Contains(html, "DISPUTED") || !strings.Contains(html, "keyCompromise") {
		t.Fatalf("page lacks the dispute")
	}
}

func TestIdentity_FinalAsRecordedIsNeverGreen(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, true, nil)
	html := page(t, fx.recs)
	for _, m := range identBlock.FindAllString(html, -1) {
		if strings.Contains(m, "id-audited") || strings.Contains(m, "good") {
			t.Fatalf("final as recorded drawn as audited: %s", m)
		}
		if !strings.Contains(m, "not re-verified here") {
			t.Fatalf("no as-recorded caveat: %s", m)
		}
	}
	if strings.Contains(html, "a-approved") {
		t.Fatal("a certificate-bound approval without an audit is styled approved")
	}
	g := identGate(t, fx.recs)
	if g.Class() == "good" {
		t.Fatal("the gate is good without an audit")
	}
}

// An :ident tally whose identity record is missing, or does not name that tally, is incomplete.
func TestIdentity_IncompleteData(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, true, func(id *agent.ApprovalIdentity, _ *agent.ApprovalWait) { id.TallyDigest = strings.Repeat("0", 64) })
	g := identGate(t, fx.recs)
	if g.Identity.Class != "bad" || !strings.Contains(strings.Join(g.Identity.Problems, "|"), "incomplete identity data") || len(g.Identity.Cards) != 0 {
		t.Fatalf("identity = %+v", g.Identity)
	}
	// The record under the digest-qualified name is found.
	fx = identRecs(t, agent.TierLogclockFinal, true, nil)
	var idr agent.ApprovalIdentity
	for i, r := range fx.recs {
		if r.Name == agent.ApprovalIdentityStep("c1", 0) {
			_ = json.Unmarshal(r.Result, &idr)
			fx.recs[i] = rec(agent.ApprovalIdentityDigestStep("c1", 0, idr.TallyDigest), idr)
		}
	}
	if g = identGate(t, fx.recs); g.Identity.Class != "final" {
		t.Fatalf("digest-named record: %+v", g.Identity)
	}
}

// A final tier on a live count of status issued before the decision is not something the gate writes;
// the page refuses to show it as final.
func TestIdentity_LiveCountIsProvisionalOnly(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, true, func(id *agent.ApprovalIdentity, _ *agent.ApprovalWait) {
		id.Decisions[0].BasisKind = agent.BasisLiveBeforeDecision
	})
	if g := identGate(t, fx.recs); g.Identity.Class != "bad" {
		t.Fatalf("identity = %+v", g.Identity)
	}
}

// identity_bound is the OR of the call's notes: a later note from a binary that predates the feature
// does not downgrade the display, and notes with different policy digests are flagged.
func TestIdentity_BoundIsStickyAndPolicyChangeIsFlagged(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, false, nil)
	plain, _ := json.Marshal(agent.ApprovalWait{ToolUseID: "c1", ToolName: "refund", Need: 1, Approvers: []string{"alice"}, Pause: agent.PauseAwaitingAnchor})
	recs := append(fx.recs, agent.Record{Name: agent.ApprovalPauseStep("c1", 1), Kind: agent.StepValue, Result: plain})
	g := identGate(t, recs)
	if g.Identity == nil || !g.Identity.Bound || g.Identity.PhaseText != "decision recorded, not yet provable" {
		t.Fatalf("identity = %+v", g.Identity)
	}
	other, _ := json.Marshal(agent.ApprovalWait{ToolUseID: "c1", Need: 1, IdentityBound: true, IdentityPolicyDigest: strings.Repeat("c", 64), Pause: agent.PauseAwaitingStatus})
	g = identGate(t, append(recs, agent.Record{Name: agent.ApprovalPauseStep("c1", 2), Kind: agent.StepValue, Result: other}))
	if !strings.Contains(strings.Join(g.Identity.Problems, "|"), "policy changed") || g.Identity.Phase != agent.PauseAwaitingStatus {
		t.Fatalf("identity = %+v", g.Identity)
	}
}

// After a hold the gate has a newer generation; the older hold is history, and the page says so.
func TestIdentity_GenerationsAfterAHold(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, true, nil)
	hold := agent.ExecHold{Format: agent.ApprovalExecHoldFormat, ToolUseID: "c1", Generation: 0, Reason: agent.ReasonRevoked, Detail: "a certificate of the chain was revoked", Approvers: []string{"alice"}, CheckedAt: stamp(identT0.Add(time.Hour)), Serial: "4242", ReasonCode: 1}
	recs := append(fx.recs, rec(agent.ApprovalExecHoldStep("c1", 0), hold))
	g := identGate(t, recs)
	if g.Identity.Hold == nil || g.Identity.Hold.Serial != "4242" || !g.Held || g.Identity.Class != "bad" {
		t.Fatalf("held: %+v", g.Identity)
	}
	// A new count, generation 1, replaces the hold as the current state.
	tl, _ := json.Marshal(agent.ApprovalTally{Need: 1, Approvers: []string{"alice"}, Approved: 1, ApprovedBy: []string{"alice"}, DecisionFormat: agent.DecisionFormat})
	td := sha256.Sum256(tl)
	var idr agent.ApprovalIdentity
	for _, r := range fx.recs {
		if r.Name == agent.ApprovalIdentityStep("c1", 0) {
			_ = json.Unmarshal(r.Result, &idr)
		}
	}
	idr.Generation, idr.Tally, idr.TallyDigest = 1, agent.ApprovalIdentTallyStep("c1", 1), hex.EncodeToString(td[:])
	recs = append(recs, agent.Record{Name: agent.ApprovalIdentTallyStep("c1", 1), Kind: agent.StepValue, Result: tl}, rec(agent.ApprovalIdentityStep("c1", 1), idr))
	g = identGate(t, recs)
	if g.Identity.Hold != nil || g.Held || !g.Identity.Reasked || g.Identity.Generation != 1 || g.Identity.Class != "final" {
		t.Fatalf("re-asked: %+v", g.Identity)
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
		{agent.PauseHeldUntilFinal, "", "counted provisionally, held until final", false},
		{agent.PauseAwaitingDecisions, "", "", true},
	} {
		fx := identRecs(t, agent.TierProvisional, false, func(_ *agent.ApprovalIdentity, w *agent.ApprovalWait) { w.Pause, w.PauseDetail = c.pause, c.detail })
		g := identGate(t, fx.recs)
		if c.text != "" && g.Identity.Line != c.text || c.text == "" && g.Identity.Line != "decision recorded, identity not checked yet" {
			t.Errorf("%s: line %q", c.pause, g.Identity.Line)
		}
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
	hold := agent.ExecHold{Format: agent.ApprovalExecHoldFormat, ToolUseID: "c1", Reason: agent.ReasonStatusStale, Detail: "status is older than the execution bound", Approvers: []string{"alice"}, CheckedAt: stamp(identT0.Add(time.Hour))}
	recs := append(fx.recs, rec(agent.ApprovalExecHoldStep("c1", 0), hold))
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

func TestIdentity_AttentionPage(t *testing.T) {
	fx := identRecs(t, agent.TierProvisional, false, func(_ *agent.ApprovalIdentity, w *agent.ApprovalWait) {
		w.Pause, w.PauseDetail = agent.PauseAwaitingStatus, "no CRL for the issuing CA yet"
	})
	v := Options{}.viewOf(context.Background(), "r1", fx.recs, nil)
	items := Options{}.runItems(v, fx.recs)
	if len(items) != 1 || items[0].Gate == nil || items[0].Gate.Identity == nil {
		t.Fatalf("items = %+v", items)
	}
	if !strings.Contains(items[0].Summary, "paused: identity status unknown") || !strings.Contains(items[0].Summary, "no CRL for the issuing CA yet") {
		t.Fatalf("summary = %q", items[0].Summary)
	}
	w := httptest.NewRecorder()
	render(w, "attention.html", AttentionList{Status: StatusAttention, Items: items, Now: identT0, Runs: 1, Coverage: Coverage{Listable: true}})
	body := html.UnescapeString(w.Body.String())
	if w.Code != 200 || !strings.Contains(body, "paused: identity status unknown") || !strings.Contains(body, "attn-id id-wait") {
		t.Fatalf("%d %s", w.Code, body)
	}
}

// An audit result for another count is not applied.
func TestIdentity_AuditResultForAnotherCountIsIgnored(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, true, nil)
	v := Options{Provers: []Prover{func(context.Context, string, []agent.Record) *Proof {
		return &Proof{OK: true, Summary: "verifies: 9 records", Approvals: map[string]ApprovalCheck{"c1": {Identity: map[string]IdentityAudit{
			"alice": {OK: true, TallyDigest: strings.Repeat("0", 64), PolicyDigest: fx.digest}}}}}
	}}}.viewOf(context.Background(), "r1", fx.recs, nil)
	if g := v.Gates[0]; g.Identity.Cards[0].Audited || g.Identity.Class == "audited" || g.Identity.Class == "final" {
		t.Fatalf("an audit of another tally applied: %+v", g.Identity)
	}
	// The green line names when the audit ran.
	ok := Options{Provers: []Prover{func(context.Context, string, []agent.Record) *Proof {
		return &Proof{OK: true, Summary: "verifies: 9 records", Approvals: map[string]ApprovalCheck{"c1": {Identity: map[string]IdentityAudit{
			"alice": {OK: true, TallyDigest: fx.tallyDigest, PolicyDigest: fx.digest, CheckedAt: "2026-10-15T10:00:00.000Z"}}}}}
	}}}.viewOf(context.Background(), "r1", fx.recs, nil)
	if l := ok.Gates[0].Identity.Line; !strings.Contains(l, "2026-10-15 10:00:00.000 UTC") || !strings.Contains(l, "not covered") {
		t.Fatalf("line = %q", l)
	}
}

// A finished run with a final-as-recorded identity and a failing proof is FAILED, not COMPLETED.
func TestIdentity_VerdictDoesNotMaskAFailedProof(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, true, nil)
	recs := append(fx.recs, agent.Record{Name: "tool:c1", Kind: agent.StepToolResult, ToolUseID: "c1", Result: []byte(`"ok"`)}, agent.Record{Name: "run:complete", Kind: agent.StepValue, Result: []byte(`"done"`)})
	v := Options{}.viewOf(context.Background(), "r1", recs, nil)
	if v.Verdict.Word != "COMPLETED" {
		t.Fatalf("no proof: %+v", v.Verdict)
	}
	v.Proof = &Proof{OK: false, Summary: "does not verify"}
	if d := runVerdict(v); d.Word != "FAILED" {
		t.Fatalf("failed proof: %+v", d)
	}
	v.Proof = &Proof{OK: true, Summary: "verifies: 9 records"}
	if d := runVerdict(v); d.Class == "good" {
		t.Fatalf("final as recorded under a good proof is %+v", d)
	}
}

// A certificate chain in a decision with nothing marking the gate bound: stripped, not a bare-key gate.
func TestIdentity_DowngradedGateIsNotBareKey(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, false, nil)
	var kept []agent.Record
	for _, r := range fx.recs {
		if r.Name != agent.ApprovalPendingStep("c1") {
			kept = append(kept, r)
		}
	}
	plain, _ := json.Marshal(agent.ApprovalWait{ToolUseID: "c1", ToolName: "refund", Need: 1, Approvers: []string{"alice"}})
	kept = append(kept, rec(agent.ApprovalPendingStep("c1"), json.RawMessage(plain)))
	g := identGate(t, kept)
	if g.Identity == nil || g.Identity.Class != "bad" || !strings.Contains(strings.Join(g.Identity.Problems, "|"), "identity data stripped") {
		t.Fatalf("identity = %+v", g.Identity)
	}
}

// An executed call with no identity tally is a problem, not "waiting for decisions".
func TestIdentity_ExecutedWithoutAnIdentityTally(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, false, nil)
	recs := append(fx.recs, agent.Record{Name: "tool:c1", Kind: agent.StepToolResult, ToolUseID: "c1", Result: []byte(`"ok"`)})
	if g := identGate(t, recs); g.Identity.Class != "bad" || !strings.Contains(strings.Join(g.Identity.Problems, "|"), "the call ran") {
		t.Fatalf("identity = %+v", g.Identity)
	}
}

// Only status at or after the decision is final; a missing or unknown basis is bad.
func TestIdentity_FinalNeedsTheAtOrAfterBasis(t *testing.T) {
	for _, kind := range []string{"", "something-new"} {
		fx := identRecs(t, agent.TierLogclockFinal, true, func(id *agent.ApprovalIdentity, _ *agent.ApprovalWait) { id.Decisions[0].BasisKind = kind })
		if g := identGate(t, fx.recs); g.Identity.Class != "bad" {
			t.Fatalf("basis %q: %+v", kind, g.Identity)
		}
	}
}

// An audit result with no digests is never for the current count.
func TestIdentity_AuditResultWithoutDigestsIsIgnored(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, true, nil)
	v := Options{Provers: []Prover{func(context.Context, string, []agent.Record) *Proof {
		return &Proof{OK: true, Summary: "verifies", Approvals: map[string]ApprovalCheck{"c1": {Identity: map[string]IdentityAudit{"alice": {OK: true}}}}}
	}}}.viewOf(context.Background(), "r1", fx.recs, nil)
	if g := v.Gates[0]; g.Identity.Cards[0].Audited {
		t.Fatalf("a result with no digests applied: %+v", g.Identity)
	}
}

func TestIdentity_LiveBasisOnAFinalTierIsNamed(t *testing.T) {
	fx := identRecs(t, agent.TierLogclockFinal, true, func(id *agent.ApprovalIdentity, _ *agent.ApprovalWait) {
		id.Decisions[0].BasisKind = agent.BasisLiveBeforeDecision
	})
	if g := identGate(t, fx.recs); !strings.Contains(strings.Join(g.Identity.Problems, "|"), agent.BasisLiveBeforeDecision) {
		t.Fatalf("problems = %v", g.Identity.Problems)
	}
}

func TestIdentity_LongDisplayNameIsCut(t *testing.T) {
	if got := shorten(strings.Repeat("a", 80), 48); len([]rune(got)) != 51 || !strings.HasSuffix(got, "...") {
		t.Fatalf("shorten = %q", got)
	}
}
