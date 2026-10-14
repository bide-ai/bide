package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

var receiptRecordedAt = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// decideReceipt records id's approval at the fixed recorded_at and returns its receipt.
func decideReceipt(t *testing.T, g *gate, id string) audit.DecisionReceipt {
	t.Helper()
	ctx := context.Background()
	s := g.subject(t)
	sig := ed25519.Sign(g.privs[id], agent.ApprovalDecisionBytes(s, id, true, testDecidedAt))
	d := agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: id, Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig}
	if err := agent.SubmitDecision(ctx, g.store, d, agent.WithClock(func() time.Time { return receiptRecordedAt })); err != nil {
		t.Fatal(err)
	}
	recs, err := g.store.History(ctx, gateRun)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range recs {
		if r.Kind == agent.StepApproval && r.Approver() == id {
			rc, err := audit.NewDecisionReceipt(s, d, r, i)
			if err != nil {
				t.Fatal(err)
			}
			return rc
		}
	}
	t.Fatal("decision not in the journal")
	return audit.DecisionReceipt{}
}

func journalOf(t *testing.T, g *gate) []agent.Record {
	t.Helper()
	recs, err := g.store.History(context.Background(), gateRun)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// headAt signs the head of g's journal as it is now, stamped ts.
func headAt(t *testing.T, g *gate, logPriv ed25519.PrivateKey, ts time.Time) audit.SignedTreeHead {
	t.Helper()
	th, err := audit.NewTreeHead(context.Background(), g.store, gateRun, ts.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	return signTH(t, th, logPriv)
}

// receiptWorld is a 2-of-3 gate where alice decides at the recorded_at, and what the journal and
// the signed heads look like at three moments.
type receiptWorld struct {
	g          *gate
	receipt    audit.DecisionReceipt
	before     audit.SignedTreeHead // signed over the journal before alice's decision
	pausedJ    []agent.Record
	afterAlice []agent.Record
	final      []agent.Record
	head       audit.SignedTreeHead // signed over the finished journal
}

func newReceiptWorld(t *testing.T) *receiptWorld {
	t.Helper()
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	wantPaused(t, g.run(), 0)
	w := &receiptWorld{g: g, pausedJ: journalOf(t, g)}
	w.before = headAt(t, g, g.logPriv, receiptRecordedAt.Add(-time.Hour))
	w.receipt = decideReceipt(t, g, "alice")
	w.afterAlice = journalOf(t, g)
	g.decide(t, "bob", true, false)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	w.final = journalOf(t, g)
	w.head = headAt(t, g, g.logPriv, receiptRecordedAt.Add(time.Hour))
	return w
}

func (w *receiptWorld) check(journal []agent.Record, heads ...audit.SignedTreeHead) audit.ReceiptFinding {
	return audit.CheckReceipt(w.receipt, journal, heads, edV(w.g.logPub))
}

// TestReceipt_Anchored: a head over the finished journal commits to the decision.
func TestReceipt_Anchored(t *testing.T) {
	w := newReceiptWorld(t)
	if f := w.check(w.final, w.before, w.head); f.Verdict != audit.ReceiptAnchored {
		t.Fatalf("%+v", f)
	}
}

// TestReceipt_Pending: the journal holds the decision and no head reaches it yet: the window until
// the application's next write. Not a failure.
func TestReceipt_Pending(t *testing.T) {
	w := newReceiptWorld(t)
	f := w.check(w.afterAlice, w.before)
	if f.Verdict != audit.ReceiptPending || f.Verdict.Failed() {
		t.Fatalf("%+v", f)
	}
	if f := w.check(w.afterAlice); f.Verdict != audit.ReceiptPending {
		t.Fatalf("no heads: %+v", f)
	}
}

// tamperedWorld is the same run in which alice's decision never landed (bob and carol decided
// instead), with a head the log key signed over that journal.
func tamperedWorld(t *testing.T, w *receiptWorld) ([]agent.Record, audit.SignedTreeHead) {
	t.Helper()
	g2 := newGate(2, []string{"alice", "bob", "carol"}, nil)
	g2.pubs, g2.privs, g2.logPub, g2.logPriv = w.g.pubs, w.g.privs, w.g.logPub, w.g.logPriv
	wantPaused(t, g2.run(), 0)
	g2.decide(t, "bob", true, false)
	g2.decide(t, "carol", true, false)
	if err := g2.run(); err != nil {
		t.Fatal(err)
	}
	return journalOf(t, g2), headAt(t, g2, g2.logPriv, receiptRecordedAt.Add(time.Hour))
}

// TestReceipt_UnmatchedWithoutProof: a journal that lacks the decision where the receipt says, under
// a signed head, is the approver's claim until the receipt carries an anchoring proof: nothing shows
// the application ever journaled the decision.
func TestReceipt_UnmatchedWithoutProof(t *testing.T) {
	w := newReceiptWorld(t)
	tampered, h := tamperedWorld(t, w)
	f := w.check(tampered, w.before, h)
	if f.Verdict != audit.ReceiptUnmatched || !f.Verdict.Failed() || !strings.Contains(f.Detail, "only if the approver's receipt is trusted") {
		t.Fatalf("%+v", f)
	}
	// The same head with an honest journal is anchored: the verdict is about the journal.
	if f := w.check(w.final, w.head); f.Verdict != audit.ReceiptAnchored {
		t.Fatalf("%+v", f)
	}
}

// TestReceipt_DeletedWithProof: once the decision is anchored, the receipt takes the proof (the record
// under a signed head), and from then on a signed head that matches the journal and holds something
// else at the position proves the deletion.
func TestReceipt_DeletedWithProof(t *testing.T) {
	w := newReceiptWorld(t)
	anchored, err := audit.AnchorReceipt(w.receipt, w.final, []audit.SignedTreeHead{w.before, w.head}, edV(w.g.logPub))
	if err != nil {
		t.Fatal(err)
	}
	if anchored.Anchor == nil || anchored.Anchor.STH.Size != w.head.Size {
		t.Fatalf("anchor: %+v", anchored.Anchor)
	}
	if err := anchored.Verify(edV(w.g.pubs["alice"])); err != nil {
		t.Fatalf("a receipt with a proof still verifies on its own: %v", err)
	}
	// The receipt survives JSON, strictly.
	b, _ := json.Marshal(anchored)
	var back audit.DecisionReceipt
	if err := audit.UnmarshalStrict(b, &back); err != nil {
		t.Fatal(err)
	}
	tampered, h := tamperedWorld(t, w)
	f := audit.CheckReceipt(back, tampered, []audit.SignedTreeHead{w.before, h}, edV(w.g.logPub))
	if f.Verdict != audit.ReceiptDeleted || !strings.Contains(f.Detail, "removed or replaced") {
		t.Fatalf("%+v", f)
	}
	// The honest journal still reads as anchored with the proof in the receipt.
	if f := audit.CheckReceipt(back, w.final, []audit.SignedTreeHead{w.head}, edV(w.g.logPub)); f.Verdict != audit.ReceiptAnchored {
		t.Fatalf("%+v", f)
	}
	// A proof that does not hold is unchecked, never an accusation.
	bad := back
	proof := *bad.Anchor
	proof.RecordBytes = append([]byte(nil), proof.RecordBytes...)
	proof.RecordBytes[len(proof.RecordBytes)-2] ^= 1
	bad.Anchor = &proof
	if f := audit.CheckReceipt(bad, tampered, []audit.SignedTreeHead{h}, edV(w.g.logPub)); f.Verdict != audit.ReceiptUnchecked {
		t.Fatalf("tampered proof: %+v", f)
	}
	// A proof signed by another key is not a proof.
	if f := audit.CheckReceipt(back, tampered, []audit.SignedTreeHead{h}, edV(ed25519PubOf(t))); f.Verdict == audit.ReceiptDeleted {
		t.Fatalf("a proof under a key the auditor does not trust accused: %+v", f)
	}
	// AnchorReceipt makes a proof only of what holds.
	if _, err := audit.AnchorReceipt(w.receipt, tampered, []audit.SignedTreeHead{h}, edV(w.g.logPub)); err == nil {
		t.Fatal("a proof was made for a decision the journal does not hold")
	}
	if _, err := audit.AnchorReceipt(w.receipt, w.afterAlice, []audit.SignedTreeHead{w.before}, edV(w.g.logPub)); err == nil {
		t.Fatal("a proof was made with no head reaching the position")
	}
}

func ed25519PubOf(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// TestReceipt_FabricatedIsTheApproversClaim: an approver signs a decision they never submitted and
// names a position. The journal and its heads are untouched; the verdict must not accuse anyone.
func TestReceipt_FabricatedIsTheApproversClaim(t *testing.T) {
	w := newReceiptWorld(t)
	s := w.g.subject(t)
	at := testDecidedAt
	sig := ed25519.Sign(w.g.privs["carol"], agent.ApprovalDecisionBytes(s, "carol", false, at))
	fab := audit.DecisionReceipt{
		Format: audit.ReceiptFormat, RunID: gateRun, ToolUseID: "c1", ToolName: s.ToolName, Args: s.Args, ArgsSHA: audit.ApprovalArgsDigest(s),
		Approver: "carol", Approved: false, DecidedAt: agent.FormatTimestamp(at), Alg: audit.AlgEd25519, Signature: sig,
		Record: "approval:c1:carol:made-up", Index: 3, RecordedAt: agent.FormatTimestamp(at), LeafHash: strings.Repeat("ab", 32),
	}
	if err := fab.Verify(edV(w.g.pubs["carol"])); err != nil {
		t.Fatalf("the fabricated receipt verifies on its own: %v", err)
	}
	f := audit.CheckReceipt(fab, w.final, []audit.SignedTreeHead{w.before, w.head}, edV(w.g.logPub))
	if f.Verdict == audit.ReceiptDeleted || f.Verdict != audit.ReceiptUnmatched {
		t.Fatalf("%+v", f)
	}
	// It cannot be given a proof either: the journal does not hold it.
	if _, err := audit.AnchorReceipt(fab, w.final, []audit.SignedTreeHead{w.head}, edV(w.g.logPub)); err == nil {
		t.Fatal("a proof was made for a fabricated receipt")
	}
}

// TestReceipt_Redacted: a decision a redaction has replaced with a tombstone (which keeps the leaf
// hash) is "redacted (anchored)", with the proof or without, never deleted.
func TestReceipt_Redacted(t *testing.T) {
	w := newReceiptWorld(t)
	anchored, err := audit.AnchorReceipt(w.receipt, w.final, []audit.SignedTreeHead{w.head}, edV(w.g.logPub))
	if err != nil {
		t.Fatal(err)
	}
	exp, err := audit.ExportJournal(context.Background(), w.g.store, gateRun)
	if err != nil {
		t.Fatal(err)
	}
	exp.Records[w.receipt.Index] = []byte(`{"redacted":{"leaf_hash":"` + w.receipt.LeafHash + `","at_ms":1}}`)
	redacted, err := exp.Journal()
	if err != nil {
		t.Fatal(err)
	}
	if !redacted[w.receipt.Index].Redacted {
		t.Fatal("the tombstone did not read as redacted")
	}
	for name, r := range map[string]audit.DecisionReceipt{"without proof": w.receipt, "with proof": anchored} {
		f := audit.CheckReceipt(r, redacted, []audit.SignedTreeHead{w.head}, edV(w.g.logPub))
		if f.Verdict != audit.ReceiptRedacted || f.Verdict.Failed() {
			t.Errorf("%s: %+v", name, f)
		}
	}
	// A tombstone that keeps some other leaf is not this decision.
	exp.Records[w.receipt.Index] = []byte(`{"redacted":{"leaf_hash":"` + strings.Repeat("cd", 32) + `","at_ms":1}}`)
	if _, err := exp.Journal(); err != nil {
		t.Fatal(err)
	}
}

// TestReceipt_NeverAnchoredIsAClaim: the journal lacks the decision and no head reaches its
// position. A late head that stops short proves nothing (clocks differ, heads arrive out of order).
func TestReceipt_AbsentIsAClaim(t *testing.T) {
	w := newReceiptWorld(t)
	if f := w.check(w.pausedJ, w.before); f.Verdict != audit.ReceiptUnmatched || !f.Verdict.Failed() {
		t.Fatalf("absent from the journal: %+v", f)
	}
	late := headAt(t, &gate{store: freshPaused(t, w), logPriv: w.g.logPriv}, w.g.logPriv, receiptRecordedAt.Add(24*time.Hour))
	if f := w.check(w.afterAlice, late); f.Verdict != audit.ReceiptPending {
		t.Fatalf("a late head that stops short must not accuse: %+v", f)
	}
}

// freshPaused returns a journal paused at the gate, as w's run was before any decision.
func freshPaused(t *testing.T, w *receiptWorld) *agent.Journal {
	t.Helper()
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	wantPaused(t, g.run(), 0)
	return g.store
}

// TestReceipt_UncheckedAndIgnored: a head that does not match the journal says nothing; heads that do
// not verify under the log key, or are of another run, are ignored.
func TestReceipt_UncheckedAndIgnored(t *testing.T) {
	w := newReceiptWorld(t)
	other := newReceiptWorld(t) // another run's world: its heads do not match this journal
	// A head the log key signed over some other journal of this run that reaches the position.
	foreign := headAt(t, other.g, w.g.logPriv, receiptRecordedAt.Add(time.Hour))
	if f := w.check(w.afterAlice, foreign); f.Verdict != audit.ReceiptUnchecked {
		t.Fatalf("mismatched head: %+v", f)
	}
	_, badPriv, _ := ed25519.GenerateKey(rand.Reader)
	forged := headAt(t, w.g, badPriv, receiptRecordedAt.Add(time.Hour))
	if f := w.check(w.pausedJ, forged); f.Verdict != audit.ReceiptUnmatched {
		t.Fatalf("a head the log key did not sign must be ignored: %+v", f)
	}
	wrongRun := w.head
	wrongRun.RunID = "elsewhere"
	if f := w.check(w.final, wrongRun); f.Verdict != audit.ReceiptPending {
		t.Fatalf("a head of another run must be ignored: %+v", f)
	}
}

// TestReceipt_Verify: a receipt verifies on its own, and any change to what it states does not.
func TestReceipt_Verify(t *testing.T) {
	w := newReceiptWorld(t)
	v := edV(w.g.pubs["alice"])
	if err := w.receipt.Verify(v); err != nil {
		t.Fatal(err)
	}
	// Through JSON, strictly.
	b, _ := json.Marshal(w.receipt)
	var back audit.DecisionReceipt
	if err := audit.UnmarshalStrict(b, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.Verify(v); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "seed") || strings.Contains(string(b), "private") {
		t.Fatalf("a receipt mentions key material: %s", b)
	}
	for name, mutate := range map[string]func(*audit.DecisionReceipt){
		"approver":   func(r *audit.DecisionReceipt) { r.Approver = "bob" },
		"approved":   func(r *audit.DecisionReceipt) { r.Approved = false },
		"run":        func(r *audit.DecisionReceipt) { r.RunID = "other" },
		"call":       func(r *audit.DecisionReceipt) { r.ToolUseID = "c9" },
		"tool":       func(r *audit.DecisionReceipt) { r.ToolName = "refund" },
		"args":       func(r *audit.DecisionReceipt) { r.Args = json.RawMessage(`{"amount":1}`) },
		"digest":     func(r *audit.DecisionReceipt) { r.ArgsSHA = strings.Repeat("0", 64) },
		"decided_at": func(r *audit.DecisionReceipt) { r.DecidedAt = "2026-01-01T00:00:00.000Z" },
		"signature":  func(r *audit.DecisionReceipt) { r.Signature = append([]byte(nil), r.Signature...); r.Signature[0] ^= 1 },
		"alg":        func(r *audit.DecisionReceipt) { r.Alg = audit.AlgMLDSA65 },
		"format":     func(r *audit.DecisionReceipt) { r.Format = "x" },
	} {
		r := w.receipt
		mutate(&r)
		if err := r.Verify(v); err == nil || (name != "format" && !errors.Is(err, audit.ErrNotVerified)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := w.receipt.Verify(edV(w.g.pubs["bob"])); err == nil {
		t.Error("verified under another approver's key")
	}
	// A receipt for a record that is not the decision is refused at creation.
	if _, err := audit.NewDecisionReceipt(w.g.subject(t), agent.Decision{ApproverID: "alice"}, w.final[0], 0); err == nil {
		t.Error("a receipt was made for the journal header")
	}
}
