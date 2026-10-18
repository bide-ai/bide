package auditcli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

var receiptTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// receiptFixture is a 1-of-2 gated charge approved by alice, her receipt, the
// journal as exported, and the files the verb reads.
type receiptFixture struct {
	t                          *testing.T
	dir                        string
	logPub, keys               string
	receipt, journal           string
	headsAnchored, headsBefore string
	store                      *agent.Journal
	recs                       []agent.Record
	logPriv                    ed25519.PrivateKey
	rc                         audit.DecisionReceipt
}

func writeHeads(t *testing.T, path string, hs ...audit.SignedTreeHead) {
	t.Helper()
	var b []byte
	for _, h := range hs {
		l, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		b = append(append(b, l...), '\n')
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newReceiptFixture(t *testing.T) *receiptFixture {
	t.Helper()
	ctx := context.Background()
	f := &receiptFixture{t: t, dir: t.TempDir(), store: agenttest.MemJournal()}
	alicePub, alicePriv, _ := ed25519.GenerateKey(rand.Reader)
	logPub, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	f.logPriv = logPriv
	f.logPub = keyText(audit.Ed25519Signer{Priv: logPriv})
	_ = logPub
	const runID = "run-r"
	policy := agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}
	charge := agent.MustFunc("charge", "charge the card", func(context.Context, struct {
		Amount int `json:"amount"`
	}) (string, error) {
		return "ok", nil
	}, agent.WithApproval(&policy))
	resolver := func(id string) (agent.ApproverVerifier, bool) {
		return audit.Ed25519Verifier{Pub: alicePub}, id == "alice"
	}
	a := agenttest.MustNew(algChargeModel{}, f.store, agent.WithTools(charge), agent.WithApproverVerifiers(resolver))
	if _, err := a.Run(ctx, runID, agent.UserText("pay")); err == nil {
		t.Fatal("did not pause")
	}
	paused, _ := f.store.History(ctx, runID)
	pausedHead, err := audit.NewTreeHead(ctx, f.store, runID, receiptTime.Add(-time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	f.headsBefore = filepath.Join(f.dir, "before.heads")
	writeHeads(t, f.headsBefore, mustSign(t, pausedHead, logPriv))

	_, call, _ := agent.FindToolCall(paused, "c1")
	s := agent.ApprovalSubject{RunID: runID, ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
	sig := ed25519.Sign(alicePriv, agent.ApprovalDecisionBytes(s, "alice", true, testDecidedAt))
	d := agent.Decision{RunID: runID, ToolUseID: "c1", ApproverID: "alice", Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig}
	if err := agent.SubmitDecision(ctx, f.store, d, agent.WithClock(func() time.Time { return receiptTime })); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, runID, agent.UserText("pay")); err != nil {
		t.Fatal(err)
	}
	f.recs, _ = f.store.History(ctx, runID)
	for i, r := range f.recs {
		if r.Kind == agent.StepApproval && r.Approver() == "alice" {
			if f.rc, err = audit.NewDecisionReceipt(s, d, r, i); err != nil {
				t.Fatal(err)
			}
		}
	}
	head, err := audit.NewTreeHead(ctx, f.store, runID, receiptTime.Add(time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	f.headsAnchored = filepath.Join(f.dir, "anchored.heads")
	writeHeads(t, f.headsAnchored, mustSign(t, pausedHead, logPriv), mustSign(t, head, logPriv))

	f.receipt = filepath.Join(f.dir, "r.receipt.json")
	writeJSON(t, f.receipt, f.rc)
	exp, err := audit.ExportJournal(ctx, f.store, runID)
	if err != nil {
		t.Fatal(err)
	}
	f.journal = filepath.Join(f.dir, "journal.json")
	writeJSON(t, f.journal, exp)
	f.keys = filepath.Join(f.dir, "keys.json")
	writeJSON(t, f.keys, map[string]string{"alice": keyText(audit.Ed25519Signer{Priv: alicePriv})})
	return f
}

func mustSign(t *testing.T, th audit.TreeHead, priv ed25519.PrivateKey) audit.SignedTreeHead {
	t.Helper()
	h, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (f *receiptFixture) args(heads string, extra ...string) []string {
	return append([]string{"verify-receipts", "-receipt", f.receipt, "-journal", f.journal, "-heads", heads, "-pubkey", f.logPub, "-approver-keys", f.keys}, extra...)
}

func TestVerifyReceipts_Anchored(t *testing.T) {
	f := newReceiptFixture(t)
	code, out, errs := runCLI(f.args(f.headsAnchored)...)
	if code != 0 || !strings.Contains(out, "PASS  alice approved") || !strings.Contains(out, "committed to by signed heads") {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	if strings.Contains(out, "under no signed head yet") {
		t.Fatalf("a tail line although the heads cover the whole journal:\n%s", out)
	}
	// A directory of receipts.
	args := f.args(f.headsAnchored)
	args[2] = f.dir
	if code, out, errs := runCLI(args...); code != 0 {
		t.Fatalf("directory: %d\n%s\n%s", code, out, errs)
	}
	// -json is the report the other verbs print.
	if code, out, _ := runCLI(append([]string{"-json"}, f.args(f.headsAnchored)...)...); code != 0 || !strings.Contains(out, `"verified": true`) {
		t.Fatalf("-json: %d %s", code, out)
	}
}

func TestVerifyReceipts_Pending(t *testing.T) {
	f := newReceiptFixture(t)
	code, out, errs := runCLI(f.args(f.headsBefore)...)
	if code != 0 || !strings.Contains(out, "INFO") || !strings.Contains(out, "pending") || !strings.Contains(out, "not yet signed into a head") {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "are under no signed head yet; the newest counted head covers 4") {
		t.Fatalf("no line saying how far the unheaded tail goes:\n%s", out)
	}
}

// removeDecision exports the journal without alice's decision and returns heads signed over that
// journal, as an application that had deleted the decision before any head covered it would sign.
func (f *receiptFixture) removeDecision() (journal, heads string) {
	f.t.Helper()
	ctx := context.Background()
	exp, err := audit.ExportJournal(ctx, f.store, f.rc.RunID)
	if err != nil {
		f.t.Fatal(err)
	}
	exp.Records = append(exp.Records[:f.rc.Index:f.rc.Index], exp.Records[f.rc.Index+1:]...)
	journal = filepath.Join(f.dir, "journal-deleted.json")
	writeJSON(f.t, journal, exp)
	recs, err := exp.Journal()
	if err != nil {
		f.t.Fatal(err)
	}
	th, err := audit.NewTreeHead(ctx, agentJournal(f.t, recs), f.rc.RunID, receiptTime.Add(time.Hour).UnixNano())
	if err != nil {
		f.t.Fatal(err)
	}
	heads = filepath.Join(f.dir, "deleted.heads")
	writeHeads(f.t, heads, mustSign(f.t, th, f.logPriv))
	return journal, heads
}

func agentJournal(t *testing.T, recs []agent.Record) *agent.Journal {
	t.Helper()
	j, err := agent.NewJournal(staticStore{recs: recs})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestVerifyReceipts_DeletedNeedsTheProof(t *testing.T) {
	f := newReceiptFixture(t)
	journal, heads := f.removeDecision()
	args := f.args(heads)
	args[4] = journal
	// Without an anchoring proof the receipt is the approver's claim.
	code, out, _ := runCLI(args...)
	if code != 1 || !strings.Contains(out, "receipt not matched by the journal (the approver's claim)") || strings.Contains(out, "decision deleted after signing") {
		t.Fatalf("unproven: %d\n%s", code, out)
	}
	// While the decision is anchored, -update stores the proof in the receipt file, keeping its mode.
	before, _ := os.Stat(f.receipt)
	code, out, _ = runCLI(f.args(f.headsAnchored, "-update")...)
	if code != 0 || !strings.Contains(out, "anchoring proof stored") {
		t.Fatalf("-update: %d\n%s", code, out)
	}
	after, _ := os.Stat(f.receipt)
	if before.Mode().Perm() != after.Mode().Perm() {
		t.Fatalf("mode %v became %v", before.Mode().Perm(), after.Mode().Perm())
	}
	if code, out, _ := runCLI(f.args(f.headsAnchored, "-update")...); code != 0 || strings.Contains(out, "stored") {
		t.Fatalf("a second -update changes nothing: %d\n%s", code, out)
	}
	// With the proof, the same tampered journal proves the deletion.
	code, out, _ = runCLI(args...)
	if code != 1 || !strings.Contains(out, "decision deleted after signing") {
		t.Fatalf("proven: %d\n%s", code, out)
	}
	// The receipt on its own still verifies.
	b, _ := os.ReadFile(f.receipt)
	var r audit.DecisionReceipt
	if err := audit.UnmarshalStrict(b, &r); err != nil || r.Anchor == nil {
		t.Fatalf("receipt after -update: %v %v", err, r.Anchor)
	}
	// -update does nothing for a pending receipt.
	g := newReceiptFixture(t)
	if code, out, _ := runCLI(g.args(g.headsBefore, "-update")...); code != 0 || strings.Contains(out, "stored") {
		t.Fatalf("pending -update: %d %s", code, out)
	}
}

func TestVerifyReceipts_AbsentIsAClaim(t *testing.T) {
	f := newReceiptFixture(t)
	// The journal lacks the decision (it ends before its position) and no head reaches it.
	exp, err := audit.ExportJournal(context.Background(), f.store, f.rc.RunID)
	if err != nil {
		t.Fatal(err)
	}
	exp.Records = exp.Records[:f.rc.Index]
	short := filepath.Join(f.dir, "short.json")
	writeJSON(t, short, exp)
	args := f.args(f.headsBefore)
	args[4] = short
	code, out, _ := runCLI(args...)
	if code != 1 || !strings.Contains(out, "receipt not matched by the journal") {
		t.Fatalf("%d\n%s", code, out)
	}
	// A late head that stops short does not accuse: the verdict is positional, not by time.
	th, err := audit.NewTreeHead(context.Background(), agentJournal(t, f.recs[:f.rc.Index]), f.rc.RunID, receiptTime.Add(24*time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	late := filepath.Join(f.dir, "late.heads")
	writeHeads(t, late, mustSign(t, th, f.logPriv))
	if code, out, _ := runCLI(f.args(late)...); code != 0 || !strings.Contains(out, "pending") {
		t.Fatalf("late head: %d\n%s", code, out)
	}
}

func TestVerifyReceipts_BadInputs(t *testing.T) {
	f := newReceiptFixture(t)
	// A receipt whose content was changed does not verify.
	tampered := f.rc
	tampered.Approved = false
	p := filepath.Join(f.dir, "t.receipt.json")
	writeJSON(t, p, tampered)
	args := f.args(f.headsAnchored)
	args[2] = p
	if code, out, _ := runCLI(args...); code != 1 || !strings.Contains(out, "does not verify") {
		t.Fatalf("tampered: %d %s", code, out)
	}
	// An approver the keys file does not hold.
	other := filepath.Join(f.dir, "other.json")
	writeJSON(t, other, map[string]string{"bob": f.logPub})
	if code, out, _ := runCLI(f.args(f.headsAnchored, "-approver-keys", other)...); code != 1 || !strings.Contains(out, "has no key for this approver") {
		t.Fatalf("unknown approver: %d %s", code, out)
	}
	// A head that no key of the log signed is ignored: the decision stays pending.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	forged := filepath.Join(f.dir, "forged.heads")
	th, _ := audit.NewTreeHead(context.Background(), f.store, f.rc.RunID, receiptTime.UnixNano())
	writeHeads(t, forged, mustSign(t, th, otherPriv))
	if code, out, _ := runCLI(f.args(forged)...); code != 0 || !strings.Contains(out, "pending") {
		t.Fatalf("forged head: %d %s", code, out)
	}
	// No journal for the receipt's run, a missing file, not a head, a negative skew, no flags.
	wrong := filepath.Join(f.dir, "wrong.json")
	exp, _ := audit.ExportJournal(context.Background(), f.store, f.rc.RunID)
	exp.RunID = "elsewhere"
	writeJSON(t, wrong, exp)
	args = f.args(f.headsAnchored)
	args[4] = wrong
	if code, _, _ := runCLI(args...); code != 4 {
		t.Fatalf("no journal for the run: %d", code)
	}
	args = f.args(f.headsAnchored)
	args[2] = filepath.Join(f.dir, "absent.receipt.json")
	if code, _, _ := runCLI(args...); code != 4 {
		t.Fatalf("missing receipt: %d", code)
	}
	if code, _, _ := runCLI(f.args(f.journal)...); code != 4 {
		t.Fatalf("a journal for -heads: %d", code)
	}
	if code, _, _ := runCLI("verify-receipts"); code != 2 {
		t.Fatalf("no flags: %d", code)
	}
}

// A head that verifies but is not the root of the journal's prefix covers nothing, so it must not
// shorten the reported tail.
func TestVerifyReceipts_TailIgnoresAHeadWithAnotherRoot(t *testing.T) {
	f := newReceiptFixture(t)
	_, deletedHeads := f.removeDecision() // a head signed over a different journal (one record shorter)
	_, out, _ := runCLI(f.args(deletedHeads)...)
	if !strings.Contains(out, "no counted head covers any of it") {
		t.Fatalf("a head over another journal was counted as covering:\n%s", out)
	}
}
