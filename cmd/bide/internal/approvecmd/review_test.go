package approvecmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/approveline"
	"github.com/bide-ai/bide/store/sqlite"
)

// Adversarial review proofs for bide approve. Each test states the behaviour a correct command has
// and fails today.

// R1: the default view clips the arguments to 40 lines, and the keys are sorted, so a model that
// controls the key names controls which keys fall below the cut. The decisive field is hidden, yet
// the command still asks "Approve this call?".
func TestReview_ClippedArgsHideDecisiveField(t *testing.T) {
	m := map[string]any{"zz_destination_account": "ATTACKER-ACCT-9999"}
	for i := 0; i < 60; i++ {
		m[fmt.Sprintf("_pad%02d", i)] = i
	}
	b, _ := json.Marshal(m)
	fx := newFixture(t, twoOfThree, string(b))
	code, out, errs := fx.run_("ops\n", fx.as("ops", "-approver-keys", fx.keysJSON)...)
	if strings.Contains(out, "ATTACKER-ACCT-9999") {
		return
	}
	if code != 0 {
		return // refusing to ask while arguments are hidden is also acceptable
	}
	t.Fatalf("recorded a signed approval (exit %d) after showing arguments that omit the field below the cut\nstderr: %s\nstdout:\n%s", code, errs, out)
}

// R2: a junk decision record by (or for) the approver, which the gate ignores, locks the real
// approver out of bide approve when -approver-keys is absent.
func TestReview_JunkRecordLocksApproverOutWithoutKeys(t *testing.T) {
	fx := newFixture(t, twoOfThree, refundArgs)
	j, closeJ := fx.store()
	err := agent.SubmitDecision(context.Background(), j, agent.Decision{
		RunID: fx.run, ToolUseID: fx.call, ApproverID: "ops", Approved: true, DecidedAt: time.Now(),
		Alg: audit.AlgEd25519, Signature: bytes.Repeat([]byte{7}, 64), // no key signed this
	}) // SubmitDecision without WithDecisionCheck records it, as documented
	closeJ()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.drive(twoOfThree, false); err == nil {
		t.Fatal("the gate counted a junk signature")
	}
	// The real ops, without -approver-keys, must still be able to decide: the junk holds nobody's place.
	code, out, errs := fx.run_("ops\n", fx.asUnverified("ops")...)
	if code != 0 {
		t.Fatalf("real ops refused (exit %d) because of a record the gate ignores:\n%s", code, errs)
	}
	_ = out
}

// R2b: two junk "approvals" make the listing claim the gate is settled and offer no command, while
// the gate still waits for real approvals.
func TestReview_JunkRecordsMakeListSayApproved(t *testing.T) {
	fx := newFixture(t, twoOfThree, refundArgs)
	j, closeJ := fx.store()
	for _, id := range []string{"ops", "finance"} {
		if err := agent.SubmitDecision(context.Background(), j, agent.Decision{
			RunID: fx.run, ToolUseID: fx.call, ApproverID: id, Approved: true, DecidedAt: time.Now(),
			Alg: audit.AlgEd25519, Signature: bytes.Repeat([]byte{7}, 64),
		}); err != nil {
			t.Fatal(err)
		}
	}
	closeJ()
	if _, err := fx.drive(twoOfThree, false); err == nil {
		t.Fatal("the gate counted junk signatures")
	}
	_, out, _ := fx.run_("", "-list")
	if strings.Contains(out, "approvals recorded") || strings.Contains(out, "approvals verified") {
		t.Fatalf("-list says the call runs on re-run; the gate (verified) still waits for all three approvers:\n%s", out)
	}
}

// R3: one unsigned agent.Approve record on an m-of-n call (the runtime ignores it, see
// IsApprovalDecision) makes the command treat the gate as a single-approver gate that has already
// decided, and refuse every approver.
func TestReview_StrayUnsignedApproveFlipsGateToSingle(t *testing.T) {
	fx := newFixture(t, twoOfThree, refundArgs)
	j, closeJ := fx.store()
	if err := agent.Approve(context.Background(), j, fx.run, fx.call, true); err != nil {
		t.Fatal(err)
	}
	closeJ()
	if _, err := fx.drive(twoOfThree, false); err == nil {
		t.Fatal("an unsigned decision passed an m-of-n gate")
	}
	code, _, errs := fx.run_("ops\n", fx.as("ops", "-approver-keys", fx.keysJSON)...)
	if code != 0 {
		t.Fatalf("ops refused (exit %d), though the gate still needs 2 signed approvals: %s", code, errs)
	}
}

// R4: preflight runs before the confirmation prompt and is not repeated after it. While the
// approver sits at the prompt, the gate is decided and the run finishes; the late approval is still
// signed and recorded, and the command reports success.
func TestReview_DecisionRecordedAfterGateDecidedDuringPrompt(t *testing.T) {
	fx := newFixture(t, twoOfThree, refundArgs)
	hook := &hookReader{r: strings.NewReader("ops\n"), fn: func() {
		for _, id := range []string{"finance", "risk"} {
			if code, _, errs := fx.run_("", fx.as(id, "-reject", "-yes", "-approver-keys", fx.keysJSON)...); code != 0 {
				t.Errorf("%s reject: %s", id, errs)
			}
		}
		if _, err := fx.drive(twoOfThree, false); err != nil {
			t.Errorf("the denied run should finish: %v", err)
		}
	}}
	var out, errb bytes.Buffer
	code := Main(append([]string{"-db", fx.db, "-color", "never"}, fx.as("ops", "-approver-keys", fx.keysJSON)...), hook, &out, &errb, stores)
	if code == 0 {
		t.Fatalf("ops' approval was signed and recorded after the gate was decided and the run ended:\n%s", out.String())
	}
}

type hookReader struct {
	once sync.Once
	fn   func()
	r    io.Reader
}

func (h *hookReader) Read(p []byte) (int, error) {
	h.once.Do(h.fn)
	return h.r.Read(p)
}

// R5: the command gates on the informational approval-pending record. When that write was lost
// (the library says a lost record is never written later), the call still waits for a decision at
// the gate, but bide approve refuses, and -list does not show it.
func TestReview_LostPendingRecordBlocksApprove(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "j.db")
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	pubs, keyFiles, keysJSON := enrol(t, dir)
	verify := func(id string) (agent.ApproverVerifier, bool) { v, ok := pubs[id]; return v, ok }
	refund := agent.MustFunc("refund", "refund", func(context.Context, json.RawMessage) (string, error) { return "ok", nil }, agent.WithApproval(twoOfThree))
	drive := func(st agent.Store) error {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c2", "refund", refundArgs), agenttest.TextTurn("done"))
		_, err := agenttest.MustNew(m, agenttest.MustJournal(st), agent.WithTools(refund), agent.WithApproverVerifiers(verify)).Run(context.Background(), "r1", agent.UserText("go"))
		return err
	}
	err = drive(dropPending{s}) // the pending note's write fails; the pause is the same without it
	if _, ok := errors.AsType[*agent.ApprovalPending](err); !ok {
		t.Fatalf("drive: %v", err)
	}
	s.Close()

	var out, errb bytes.Buffer
	code := Main([]string{"-db", db, "-color", "never", "-run", "r1", "-call", "c2", "-as", "ops", "-key", keyFiles["ops"], "-approver-keys", keysJSON, "-yes"}, strings.NewReader(""), &out, &errb, stores)
	// bide approve cannot know the gate's policy without the note, so it refuses, precisely, rather
	// than take a policy from the operator's flags; nothing is recorded.
	if code != 1 || !strings.Contains(errb.String(), "approval-pending") || !strings.Contains(errb.String(), "lost") {
		t.Fatalf("exit %d, want a precise refusal naming the lost approval-pending note: %s", code, errb.String())
	}
	if strings.Contains(errb.String(), "has no approval gate") {
		t.Fatalf("the refusal says there is no gate, though the call waits at one: %s", errb.String())
	}
	s2, _ := sqlite.Open(db)
	defer s2.Close()
	recs, _ := agenttest.MustJournal(s2).History(context.Background(), "r1")
	for _, r := range recs {
		if r.Kind == agent.StepApproval {
			t.Fatal("a decision was recorded")
		}
	}
}

type dropPending struct{ agent.Store }

func (d dropPending) Insert(ctx context.Context, run, name string, data []byte) (agent.Entry, bool, error) {
	if strings.HasPrefix(name, "approval-pending:") {
		return agent.Entry{}, false, errors.New("injected: write lost")
	}
	return d.Store.Insert(ctx, run, name, data)
}

func enrol(t *testing.T, dir string) (map[string]audit.Verifier, map[string]string, string) {
	t.Helper()
	pubs, files, keys := map[string]audit.Verifier{}, map[string]string{}, map[string]string{}
	for _, id := range approvers {
		p := filepath.Join(dir, id+".key")
		priv, err := writeKeyPair(p, audit.AlgEd25519)
		if err != nil {
			t.Fatal(err)
		}
		v, _ := audit.ParsePublicKey(publicKeyText(priv))
		pubs[id], files[id], keys[id] = v, p, publicKeyText(priv)
	}
	b, _ := json.Marshal(keys)
	kj := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(kj, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return pubs, files, kj
}

// R6: bide approve writes through the plain store. When the application journals through
// audit.AuditedStore, the signed decision is in the journal but no published tree head covers it
// until the application's next write. Every other journal write is anchored by the write itself.
func TestReview_DecisionWrittenOutsideAuditedStoreIsNotAnchored(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "j.db")
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pubs, keyFiles, keysJSON := enrol(t, dir)
	verify := func(id string) (agent.ApproverVerifier, bool) { v, ok := pubs[id]; return v, ok }
	logPub, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	anc := &memAnchor{}
	as, err := audit.NewAuditedStore(s, audit.Ed25519Signer{Priv: logPriv}, anc)
	if err != nil {
		t.Fatal(err)
	}
	aj := agenttest.MustJournal(as)
	refund := agent.MustFunc("refund", "refund", func(context.Context, json.RawMessage) (string, error) { return "ok", nil }, agent.WithApproval(twoOfThree))
	drive := func() error {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c2", "refund", refundArgs), agenttest.TextTurn("done"))
		_, err := agenttest.MustNew(m, aj, agent.WithTools(refund), agent.WithApproverVerifiers(verify)).Run(context.Background(), "r1", agent.UserText("go"))
		return err
	}
	if _, ok := errors.AsType[*agent.ApprovalPending](drive()); !ok {
		t.Fatal("no pause")
	}
	for _, id := range []string{"ops", "finance"} {
		var out, errb bytes.Buffer
		code := Main([]string{"-db", db, "-color", "never", "-run", "r1", "-call", "c2", "-as", id, "-key", keyFiles[id], "-approver-keys", keysJSON, "-yes"}, strings.NewReader(""), &out, &errb, stores)
		if code != 0 {
			t.Fatalf("%s: %s", id, errb.String())
		}
	}
	recs, _ := aj.History(context.Background(), "r1")
	covered := anc.latest("r1")
	headsBefore := anc.all("r1")

	// What the end-to-end flow does NOT lose: after the application resumes, its writes anchor the
	// whole journal and the evidence verifies.
	if err := drive(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	recs2, _ := aj.History(context.Background(), "r1")
	if got := anc.latest("r1"); got != len(recs2) {
		t.Fatalf("after resume the latest head covers %d of %d records", got, len(recs2))
	}
	pkg, err := audit.Evidence(context.Background(), aj, "r1", audit.Ed25519Signer{Priv: logPriv}, time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	acts, err := audit.ApprovalEvidence(context.Background(), aj, "r1", "c2", pkg.STH)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := audit.VerifyApprovals(acts, "c2", agent.ApprovalPolicy{Need: 2, Approvers: approvers}, verify, audit.Ed25519Verifier{Pub: logPub}); err != nil || !v.OK {
		t.Fatalf("VerifyApprovals: %v %+v", err, v)
	}

	// The window, documented: between bide approve and the application's next write, no published
	// head covers the decisions (the store promises this for its own writes, not for a second
	// process's). The receipt bide approve left is pending then, and anchored after the resume.
	if covered >= len(recs) {
		t.Fatalf("the window closed by itself: the latest head covers %d of %d records", covered, len(recs))
	}
	rc := readReceipt(t, receiptsIn(t, dir)[0])
	if fnd := audit.CheckReceipt(rc, recs, headsBefore, audit.Ed25519Verifier{Pub: logPub}); fnd.Verdict != audit.ReceiptPending {
		t.Fatalf("before the resume: %+v", fnd)
	}
	if fnd := audit.CheckReceipt(rc, recs2, anc.all("r1"), audit.Ed25519Verifier{Pub: logPub}); fnd.Verdict != audit.ReceiptAnchored {
		t.Fatalf("after the resume: %+v", fnd)
	}
}

type memAnchor struct {
	mu    sync.Mutex
	sizes map[string]int
	heads map[string][]audit.SignedTreeHead
}

func (m *memAnchor) all(run string) []audit.SignedTreeHead {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]audit.SignedTreeHead(nil), m.heads[run]...)
}

func (m *memAnchor) Publish(_ context.Context, run string, sth audit.SignedTreeHead) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sizes == nil {
		m.sizes = map[string]int{}
	}
	if m.heads == nil {
		m.heads = map[string][]audit.SignedTreeHead{}
	}
	m.heads[run] = append(m.heads[run], sth)
	if sth.Size > m.sizes[run] {
		m.sizes[run] = sth.Size
	}
	return nil
}

func (m *memAnchor) latest(run string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sizes[run]
}

// R7: characters that render as blank or reorder text and are not escaped by safe.
func TestReview_SafeLeavesInvisibleFillers(t *testing.T) {
	for _, r := range []rune{0x3164, 0x2800, 0x115F, 0x1160, 0xFFA0, 0x2062, 0x00AD, 0x061C, 0x180E} {
		s := "x" + string(r) + "y"
		if safe(s) == s {
			t.Errorf("safe leaves U+%04X unescaped; it renders as blank", r)
		}
	}
}

// R2c: an approver whose first attempt used the wrong key (rotated, or the wrong file) and no
// -approver-keys cannot correct it: the unverified record holds their place.
func TestReview_WrongKeyFirstAttemptCannotBeCorrectedWithoutKeys(t *testing.T) {
	fx := newFixture(t, twoOfThree, refundArgs)
	wrong := filepath.Join(fx.dir, "wrong.key")
	if _, err := writeKeyPair(wrong, audit.AlgEd25519); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := fx.run_("", "-run", fx.run, "-call", fx.call, "-as", "ops", "-key", wrong, "-yes", "-unverified"); code != 0 {
		t.Fatalf("setup: %s", errs)
	}
	if code, _, errs := fx.run_("", fx.asUnverified("ops", "-yes")...); code != 0 {
		t.Fatalf("the right key is refused after a wrong-key record the gate will not count: %s", errs)
	}
}

// R8: -list prints the bide approve command line with the model-chosen tool-use id inside it, and
// does not pass the line through safe. A tool-use id may hold any valid UTF-8 (checkToolUseIDs).
func TestReview_ListCommandLineCarriesTerminalEscapes(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "j.db")
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	pubs, _, _ := enrol(t, dir)
	verify := func(id string) (agent.ApproverVerifier, bool) { v, ok := pubs[id]; return v, ok }
	refund := agent.MustFunc("refund", "refund", func(context.Context, json.RawMessage) (string, error) { return "ok", nil }, agent.WithApproval(twoOfThree))
	id := "c2\x1b[2J\x1b[Hall clear\u202e"
	m := agenttest.NewScriptedModel(agenttest.ToolTurn(id, "refund", refundArgs), agenttest.TextTurn("done"))
	_, err = agenttest.MustNew(m, agenttest.MustJournal(s), agent.WithTools(refund), agent.WithApproverVerifiers(verify)).Run(context.Background(), "r1", agent.UserText("go"))
	if _, ok := errors.AsType[*agent.ApprovalPending](err); !ok {
		t.Fatalf("drive: %v", err)
	}
	s.Close()
	var out, errb bytes.Buffer
	if code := Main([]string{"-db", db, "-color", "never", "-list"}, strings.NewReader(""), &out, &errb, stores); code != 0 {
		t.Fatal(errb.String())
	}
	if strings.ContainsAny(out.String(), "\x1b\u202e") {
		t.Fatalf("-list output carries a raw escape or bidi override: %q", out.String())
	}
}

// R9: two concurrent bide approve processes of one approver (an approve and a reject) both pass
// SubmitDecision's check, which is a read followed by a write. Both are recorded; the first valid
// one counts; the other command also reports success.
func TestReview_ConcurrentApproveAndRejectBothReportSuccess(t *testing.T) {
	for i := 0; i < 40; i++ {
		fx := newFixture(t, twoOfThree, refundArgs)
		var wg sync.WaitGroup
		var mu sync.Mutex
		codes := map[string]int{}
		start := make(chan struct{})
		for _, flag := range []string{"-yes", "-reject"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				var out, errb bytes.Buffer
				args := append([]string{"-db", fx.db, "-color", "never"}, fx.as("ops", "-yes", "-approver-keys", fx.keysJSON)...)
				if flag == "-reject" {
					args = append(args, "-reject")
				}
				c := Main(args, strings.NewReader(""), &out, &errb, stores)
				mu.Lock()
				codes[flag] = c
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()
		if codes["-yes"] == 0 && codes["-reject"] == 0 {
			t.Fatalf("iteration %d: approve and reject by one approver both reported success; %d decision records", i, len(fx.decisions()))
		}
	}
}

type listAnchor struct {
	mu   sync.Mutex
	sths []audit.SignedTreeHead
}

func (l *listAnchor) Publish(_ context.Context, _ string, sth audit.SignedTreeHead) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sths = append(l.sths, sth)
	return nil
}

// R10: two processes (the app and bide approve), each with its own AuditedStore and signer state
// over one journal, append concurrently. Every published head must commit to a true prefix of the
// one journal (no fork), whatever order the heads arrive in.
func TestReview_TwoAuditedWritersNoFork(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "j.db")
	s1, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	_, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	anc := &listAnchor{}
	mk := func(s agent.Store) *agent.Journal {
		as, err := audit.NewAuditedStore(s, audit.Ed25519Signer{Priv: logPriv}, anc)
		if err != nil {
			t.Fatal(err)
		}
		return agenttest.MustJournal(as)
	}
	j1, j2 := mk(s1), mk(s2)
	pubs, _, _ := enrol(t, dir)
	verify := func(id string) (agent.ApproverVerifier, bool) { v, ok := pubs[id]; return v, ok }
	refund := agent.MustFunc("refund", "refund", func(context.Context, json.RawMessage) (string, error) { return "ok", nil }, agent.WithApproval(twoOfThree))
	m := agenttest.NewScriptedModel(agenttest.ToolTurn("c2", "refund", refundArgs), agenttest.TextTurn("done"))
	if _, err := agenttest.MustNew(m, j1, agent.WithTools(refund), agent.WithApproverVerifiers(verify)).Run(context.Background(), "r1", agent.UserText("go")); err == nil {
		t.Fatal("no pause")
	}
	var wg sync.WaitGroup
	for w, j := range []*agent.Journal{j1, j2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				sig := make([]byte, 64)
				rand.Read(sig)
				_ = agent.SubmitDecision(context.Background(), j, agent.Decision{RunID: "r1", ToolUseID: "c2", ApproverID: []string{"ops", "finance"}[w],
					Approved: true, DecidedAt: time.Now(), Alg: audit.AlgEd25519, Signature: sig})
			}
		}()
	}
	wg.Wait()
	recs, _ := j1.History(context.Background(), "r1")
	anc.mu.Lock()
	defer anc.mu.Unlock()
	inversions, last := 0, 0
	for _, sth := range anc.sths {
		if _, err := audit.ProveRecord(context.Background(), j1, "r1", 0, sth); err != nil {
			t.Fatalf("head of size %d is not a prefix of the journal: %v", sth.Size, err)
		}
		if sth.Size < last {
			inversions++
		}
		last = sth.Size
	}
	maxSize := 0
	for _, sth := range anc.sths {
		maxSize = max(maxSize, sth.Size)
	}
	t.Logf("%d heads, %d arrived smaller than their predecessor; journal %d records, largest head %d", len(anc.sths), inversions, len(recs), maxSize)
	if maxSize != len(recs) {
		t.Fatalf("no head covers the final journal: largest %d of %d", maxSize, len(recs))
	}
}

// R1b: with -full the approver sees everything, so the same call can be approved.
func TestReview_FullShowsHiddenFieldAndAllowsApproval(t *testing.T) {
	m := map[string]any{"zz_destination_account": "ATTACKER-ACCT-9999"}
	for i := 0; i < 60; i++ {
		m[fmt.Sprintf("_pad%02d", i)] = i
	}
	b, _ := json.Marshal(m)
	fx := newFixture(t, twoOfThree, string(b))
	code, out, errs := fx.run_("", fx.as("ops")...)
	if code != 1 || !strings.Contains(errs, "-full") || !strings.Contains(out, "args SHA-256") || len(fx.decisions()) != 0 {
		t.Fatalf("clipped arguments must be refused with a pointer to -full, keeping the digest line: %d %s\n%s", code, errs, out)
	}
	if code, out, errs := fx.run_("ops\n", fx.as("ops", "-full")...); code != 0 || !strings.Contains(out, "ATTACKER-ACCT-9999") {
		t.Fatalf("-full: %d %s\n%s", code, errs, out)
	}
	// Arguments that fit are approved without -full.
	fy := newFixture(t, twoOfThree, refundArgs)
	if code, _, errs := fy.run_("ops\n", fy.as("ops")...); code != 0 {
		t.Fatalf("short arguments refused: %s", errs)
	}
}

// R2d: -list with the approvers' keys verifies: junk records leave the gate waiting and the commands
// offered; without keys the status is labelled unverified and never settles the gate.
func TestReview_ListVerifiesWithKeys(t *testing.T) {
	fx := newFixture(t, twoOfThree, refundArgs)
	j, closeJ := fx.store()
	for _, id := range []string{"ops", "finance"} {
		_ = agent.SubmitDecision(context.Background(), j, agent.Decision{RunID: fx.run, ToolUseID: fx.call, ApproverID: id, Approved: true, DecidedAt: time.Now(),
			Alg: audit.AlgEd25519, Signature: bytes.Repeat([]byte{7}, 64)})
	}
	closeJ()
	_, out, _ := fx.run_("", "-list", "-approver-keys", fx.keysJSON)
	if strings.Contains(out, "approvals verified") || !strings.Contains(out, "-as ops") || !strings.Contains(out, "pending: ops, finance, risk") {
		t.Fatalf("verified -list:\n%s", out)
	}
	_, out, _ = fx.run_("", "-list")
	if strings.Contains(out, "approvals verified") || !strings.Contains(out, "unverified") || !strings.Contains(out, "-as risk") {
		t.Fatalf("unverified -list:\n%s", out)
	}
	// Real approvals, verified, settle it.
	for _, id := range []string{"ops", "finance"} {
		if code, _, errs := fx.run_("", fx.as(id, "-yes")...); code != 0 {
			t.Fatalf("%s: %s", id, errs)
		}
	}
	if _, out, _ = fx.run_("", "-list", "-approver-keys", fx.keysJSON); !strings.Contains(out, "approvals verified") {
		t.Fatalf("verified approvals do not settle -list:\n%s", out)
	}
}

// L1b: a run that ended while the approver sat at the prompt is refused.
func TestReview_NoDecisionAfterTheRunEndedDuringPrompt(t *testing.T) {
	fx := newFixture(t, twoOfThree, refundArgs)
	hook := &hookReader{r: strings.NewReader("ops\n"), fn: func() {
		j, closeJ := fx.store()
		defer closeJ()
		if err := agent.Cancel(context.Background(), j, fx.run, "operator cancelled"); err != nil {
			t.Errorf("cancel: %v", err)
		}
	}}
	var out, errb bytes.Buffer
	code := Main(append([]string{"-db", fx.db, "-color", "never"}, fx.as("ops")...), hook, &out, &errb, stores)
	if code != 1 || len(fx.decisions()) != 0 {
		t.Fatalf("exit %d, %d decisions: %s", code, len(fx.decisions()), errb.String())
	}
}

// R11: a receipt is the approver's own claim: Record and Index are unsigned, and nothing in it shows
// the decision was ever journaled. A decision signed by a key that never submitted it, naming a
// position, must not make the audit say a decision was deleted from an untouched journal.
func TestReview_FabricatedReceiptAccusesAnUntouchedJournal(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "j.db")
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pubs, keyFiles, keysJSON := enrol(t, dir)
	verify := func(id string) (agent.ApproverVerifier, bool) { v, ok := pubs[id]; return v, ok }
	logPub, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	anc := &listAnchor{}
	as, err := audit.NewAuditedStore(s, audit.Ed25519Signer{Priv: logPriv}, anc)
	if err != nil {
		t.Fatal(err)
	}
	aj := agenttest.MustJournal(as)
	refund := agent.MustFunc("refund", "refund", func(context.Context, json.RawMessage) (string, error) { return "ok", nil }, agent.WithApproval(twoOfThree))
	drive := func() error {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c2", "refund", refundArgs), agenttest.TextTurn("done"))
		_, err := agenttest.MustNew(m, aj, agent.WithTools(refund), agent.WithApproverVerifiers(verify)).Run(context.Background(), "r1", agent.UserText("go"))
		return err
	}
	if _, ok := errors.AsType[*agent.ApprovalPending](drive()); !ok {
		t.Fatal("no pause")
	}
	for _, id := range []string{"ops", "finance"} {
		var out, errb bytes.Buffer
		if code := Main([]string{"-db", db, "-color", "never", "-run", "r1", "-call", "c2", "-as", id, "-key", keyFiles[id], "-approver-keys", keysJSON, "-yes"}, strings.NewReader(""), &out, &errb, stores); code != 0 {
			t.Fatalf("%s: %s", id, errb.String())
		}
	}
	if err := drive(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	recs, _ := aj.History(context.Background(), "r1")
	anc.mu.Lock()
	heads := append([]audit.SignedTreeHead(nil), anc.sths...)
	anc.mu.Unlock()

	sg, err := LoadSigner(keyFiles["risk"])
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	subj := agent.ApprovalSubject{RunID: "r1", ToolUseID: "c2", ToolName: "refund", Args: json.RawMessage(refundArgs)}
	sig, _ := sg.Sign(agent.ApprovalDecisionBytes(subj, "risk", false, at))
	fab := audit.DecisionReceipt{
		Format: audit.ReceiptFormat, RunID: "r1", ToolUseID: "c2", ToolName: "refund", Args: json.RawMessage(refundArgs),
		ArgsSHA: audit.ApprovalArgsDigest(subj), Approver: "risk", Approved: false, DecidedAt: agent.FormatTimestamp(at),
		Alg: sg.Alg(), Signature: sig, Record: "approval:c2:risk:made-up", Index: 3, RecordedAt: agent.FormatTimestamp(at),
	}
	if err := fab.Verify(pubs["risk"]); err != nil {
		t.Fatalf("the fabricated receipt should verify on its own: %v", err)
	}
	f := audit.CheckReceipt(fab, recs, heads, audit.Ed25519Verifier{Pub: logPub})
	if f.Verdict != audit.ReceiptUnmatched {
		t.Fatalf("a receipt for a decision never submitted makes the audit say %q about an untouched journal: %s", f.Verdict, f.Detail)
	}
}

// R12: characters approveline.Unsafe must refuse: they render as nothing or change how the text
// around them is drawn.
func TestReview_UnsafeLeavesInvisibleMarks(t *testing.T) {
	for _, r := range []rune{0x034F, 0xFE0F, 0xE0100, 0x17B4, 0x17B5, 0xFFFC, 0x3000, 0x1D173} {
		if !approveline.Unsafe(r) {
			t.Errorf("approveline.Unsafe(U+%04X) = false", r)
		}
		if safe("x"+string(r)+"y") == "x"+string(r)+"y" {
			t.Errorf("safe leaves U+%04X unescaped", r)
		}
	}
}
