package demo

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/notify"
	"github.com/bide-ai/bide/store/sqlite"
)

// TestMain lets the test binary stand in for the demo's child process: demo re-executes
// os.Executable, which under go test is this binary.
func TestMain(m *testing.M) {
	if dir := os.Getenv(childEnv); dir != "" {
		os.Exit(child(dir))
	}
	os.Exit(m.Run())
}

// TestDemo runs the demo headless (as -fast -no-browser does): the child is killed after the
// charge, the resume halts instead of charging again, the resolution completes the run, the
// provider holds exactly one charge, and the evidence, the anchored head and the gate's signatures
// verify. The page bide view serves shows the crashed attempt, the resolution and the proofs.
func TestDemo(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	res, err := demo(context.Background(), dir, config{}, newPrinter(&out, term.Auto), true)
	if err != nil {
		t.Fatalf("demo: %v\n%s", err, out.String())
	}
	t.Cleanup(res.close)
	charges, err := providerCharges(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(charges) != 1 || res.charges != 1 {
		t.Fatalf("the provider holds %d charges (demo saw %d), want exactly 1", len(charges), res.charges)
	}
	if res.state != "completed" {
		t.Fatalf("run state %q, want completed", res.state)
	}
	if !res.proof.OK || !res.anchored.OK {
		t.Fatalf("proofs do not verify: evidence %v, anchored head %v", res.proof.Problems, res.anchored.Problems)
	}
	if !res.approvals.OK || !strings.HasPrefix(res.approvals.Text, "2 of 3: signatures verified") {
		t.Fatalf("approvals: %q", res.approvals.Text)
	}

	// Without bide, the same crash and a naive retry charge the same provider twice.
	if res.naiveCharges != 2 {
		t.Fatalf("the run without bide left %d charges, want 2", res.naiveCharges)
	}
	if naive, err := providerCharges(filepath.Join(dir, naiveDir)); err != nil || len(naive) != 2 {
		t.Fatalf("the naive provider holds %d charges (%v), want 2", len(naive), err)
	}

	// The halt reached the receiver as a signed webhook, which verified it (the demo prints the
	// line only then).
	if n := res.notified; n == nil || n.Kind != "outcome_unknown" || n.RunID != runID || n.Halt == nil || n.Step == nil || n.Step.OpID != callID {
		t.Fatalf("the halt notification the receiver verified: %+v", n)
	}

	// The same verification fails on a copy of the journal with one amount changed, and the
	// journal itself is untouched.
	if res.tampered == nil || res.tampered.OK || len(res.tampered.Problems) == 0 {
		t.Fatalf("the tampered journal verified: %+v", res.tampered)
	}
	if !errors.Is(res.tamperErr, audit.ErrRootMismatch) {
		t.Fatalf("the tampered journal failed with %v, want audit.ErrRootMismatch", res.tamperErr)
	}
	if res.tamperedSeq != 3 {
		t.Fatalf("tampered record %d, want 3 (the charge request)", res.tamperedSeq)
	}
	if got, want := journalArgs(t, filepath.Join(dir, tamperedFile)), `"amount":1200}`; !strings.Contains(got, want) {
		t.Fatalf("the copy's charge request %s lacks %s", got, want)
	}
	if got, want := journalArgs(t, filepath.Join(dir, journalFile)), `"amount":120}`; !strings.Contains(got, want) {
		t.Fatalf("the journal's charge request %s lacks %s: the demo changed the original", got, want)
	}

	text := out.String()
	for _, want := range []string{"Without bide  restart retries the charge", "provider charges: 2  double charge", "ops notified  signed webhook (outcome_unknown)", "6 Tamper  change record 3: $120 \u2192 $1,200", "verify failed: journal changed since it was signed", "tree head root does not match", "process killed mid-charge", "outcome unknown: not charging again", "provider charges: 1", "charge ch_1 found", "run completed", "charged exactly once."} {
		if !strings.Contains(text, want) {
			t.Errorf("the narration lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b[") {
		t.Error("the narration to a non-terminal is colored")
	}

	req := httptest.NewRequest("GET", "/run?id="+runID, nil)
	req.Host = "127.0.0.1:8484"
	rec := httptest.NewRecorder()
	res.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /run = %d: %s", rec.Code, rec.Body)
	}
	page := rec.Body.String()
	for _, want := range []string{"attempt claimed: tool charge-1", "reconciled by ResolveHalt", "ch_1", "2 of 3: signatures verified", "VERIFIED", "evidence package", "completed"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// journalArgs is the charge request's record in the SQLite journal at path.
func journalArgs(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var data string
	if err := db.QueryRow(`SELECT data FROM bide_steps WHERE name = '@llm/0'`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	return data
}

// The plain-language line follows the error's type, not its text: a wrapped sentinel gets its
// wording, and any other error gets none.
func TestVerdictReasonFollowsTheErrorType(t *testing.T) {
	wrapped := fmt.Errorf("the signed head does not commit: %w", fmt.Errorf("%w run r at size 3", audit.ErrRootMismatch))
	if got := verdictReason(wrapped); !strings.Contains(got, "changed since it was signed") {
		t.Errorf("verdictReason(root mismatch) = %q", got)
	}
	// The same words in another error's text do not borrow the wording.
	lookalike := errors.New("audit: tree head root does not match run r at size 3")
	for _, err := range []error{lookalike, errors.New("boom"), nil} {
		if got := verdictReason(err); got != "" {
			t.Errorf("verdictReason(%v) = %q, want none", err, got)
		}
	}
}

// The order of the narration is the story's: the run without bide first, the halt before its
// notification, the proofs before the tamper, the verdict last.
func TestDemoOrder(t *testing.T) {
	var out bytes.Buffer
	res, err := demo(context.Background(), t.TempDir(), config{}, newPrinter(&out, term.Auto), true)
	if err != nil {
		t.Fatalf("demo: %v\n%s", err, out.String())
	}
	t.Cleanup(res.close)
	text, last := out.String(), -1
	for _, want := range []string{"Without bide", "provider charges: 2", "1 Approve", "2 Crash", "3 Restart", "halted:", "ops notified", "4 Resolve", "5 Prove", "6 Tamper", "verify failed", "charged exactly once."} {
		i := strings.Index(text, want)
		if i < 0 || i < last {
			t.Fatalf("%q is missing or out of order in:\n%s", want, text)
		}
		last = i
	}
}

// A receiver accepts only a delivery signed with its secret: a webhook with another secret is
// refused, so the demo cannot print that ops was notified for an unauthenticated delivery.
func TestReceiverRefusesBadSignature(t *testing.T) {
	r, err := newReceiver()
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.srv.Certificate().Raw})
	w, err := notify.NewWebhook(r.srv.URL, notify.WithSecrets("not-the-receivers-secret", ""), notify.WithServerCAPEM(ca), notify.WithRequestsPerCall(1))
	if err != nil {
		t.Fatal(err)
	}
	n := agent.Notification{ID: "id1", Key: "k", Kind: agent.NotificationOutcomeUnknown, RunID: runID}
	if err := w.Notify(context.Background(), n); err == nil {
		t.Fatal("a delivery signed with another secret was accepted")
	}
	if _, err := r.verified(); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("verified() = %v, want the refusal", err)
	}
	// And one signed with the right secret is accepted.
	good, err := notify.NewWebhook(r.srv.URL, notify.WithSecrets(r.secret, ""), notify.WithServerCAPEM(ca))
	if err != nil {
		t.Fatal(err)
	}
	if err := good.Notify(context.Background(), n); err != nil {
		t.Fatalf("a delivery signed with the secret: %v", err)
	}
	if pl, err := r.verified(); err != nil || pl.ID != "id1" {
		t.Fatalf("verified() = %+v, %v", pl, err)
	}
}

// The recorder (assets/demo/record.sh) gets a journal at each state of the story, in order, with
// the times it reached them: each copy is a readable journal with the records of its state.
func TestDemoRecords(t *testing.T) {
	rec := t.TempDir()
	t.Setenv(recordEnv, rec)
	var out bytes.Buffer
	res, err := demo(context.Background(), t.TempDir(), config{}, newPrinter(&out, term.Auto), true)
	if err != nil {
		t.Fatalf("demo: %v\n%s", err, out.String())
	}
	t.Cleanup(res.close)
	b, err := os.ReadFile(filepath.Join(rec, "marks.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		names = append(names, strings.Fields(l)[0])
	}
	want := []string{"pending", "approved", "halted", "resolved", "verified", "tampered", "final"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("marks %v, want %v", names, want)
	}
	counts := map[string]int{}
	for _, name := range want[:4] {
		s, err := sqlite.OpenReadOnly(filepath.Join(rec, name+".db"))
		if err != nil {
			t.Fatalf("%s.db: %v", name, err)
		}
		j, err := agent.NewJournal(s)
		if err != nil {
			t.Fatal(err)
		}
		recs, err := j.History(context.Background(), runID)
		s.Close()
		if err != nil {
			t.Fatalf("%s.db: %v", name, err)
		}
		counts[name] = len(recs)
	}
	// Each state holds more of the story than the one before it.
	prev := -1
	for _, name := range want[:4] {
		if counts[name] <= prev {
			t.Errorf("%s.db holds %d records, not more than the state before (%d): %v", name, counts[name], prev, counts)
		}
		prev = counts[name]
	}
}

// TestDemoCatchesDoubleCharge declares the charge retry-safe, which it is not: bide then retries
// it after the crash, the provider charges twice, and the demo must fail rather than report a
// single charge.
func TestDemoCatchesDoubleCharge(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	res, err := demo(context.Background(), dir, config{RetrySafe: true}, newPrinter(&out, term.Auto), true)
	if err == nil {
		res.close()
		t.Fatalf("the demo passed with a retry-safe charge:\n%s", out.String())
	}
	charges, cerr := providerCharges(dir)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if len(charges) != 2 {
		t.Fatalf("the provider holds %d charges, want 2 (the retry charged again)", len(charges))
	}
	if !strings.Contains(err.Error(), "2 charges") {
		t.Fatalf("the demo's error does not report the double charge: %v", err)
	}
	if strings.Contains(out.String(), "charged exactly once") {
		t.Fatal("the demo claimed a single charge")
	}
}

// TestDemoColor: with -color=always each step's header is in its color, and the crash (red), the
// halt (amber), the resolution (blue) and the verified proofs (green) are each colored apart, each
// with its word.
func TestDemoColor(t *testing.T) {
	var out bytes.Buffer
	res, err := demo(context.Background(), t.TempDir(), config{}, newPrinter(&out, term.Always), true)
	if err != nil {
		t.Fatalf("demo: %v\n%s", err, out.String())
	}
	t.Cleanup(res.close)
	text := out.String()
	for _, want := range []string{
		"\x1b[31m0\x1b[0m \x1b[1m\x1b[31mWithout bide\x1b[0m\x1b[0m ",
		"\x1b[31mprovider charges: 2\x1b[0m  \x1b[31m\u2717\x1b[0m \x1b[31m\x1b[1mdouble charge\x1b[0m\x1b[0m",
		"\x1b[31m2\x1b[0m \x1b[1m\x1b[31mCrash  \x1b[0m\x1b[0m",
		"\x1b[33m\u2192 ops notified\x1b[0m  signed webhook (outcome_unknown)",
		"\x1b[31m6\x1b[0m \x1b[1m\x1b[31mTamper \x1b[0m\x1b[0m",
		"\x1b[31m\u2717\x1b[0m \x1b[31m\x1b[1mverify failed\x1b[0m: journal changed since it was signed\x1b[0m",
		"\x1b[1m\x1b[33mRestart\x1b[0m\x1b[0m",
		"\x1b[1m\x1b[34mResolve\x1b[0m\x1b[0m",
		"\x1b[1m\x1b[32mProve  \x1b[0m\x1b[0m",
		"\x1b[31m\x1b[1mprocess killed mid-charge\x1b[0m\x1b[0m",
		"\x1b[33m\x1b[1mhalted\x1b[0m\x1b[0m\x1b[33m: outcome unknown: not charging again\x1b[0m",
		"\x1b[34m\x1b[1mresolved\x1b[0m\x1b[0m with ResolveHalt + evidence",
		"\x1b[32m✓\x1b[0m \x1b[32mverified (",
		"\x1b[32m✓\x1b[0m \x1b[32m\x1b[1mcharged exactly once.\x1b[0m\x1b[0m",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the colored narration lacks %q:\n%q", want, text)
		}
	}
}

// TestDemoColorFlag: -color takes auto, always or never; anything else is a usage error, before
// the demo runs.
func TestDemoColorFlag(t *testing.T) {
	var out bytes.Buffer
	if code := Main([]string{"-color=sometimes"}, []string{"demo"}, &out, &out); code != 2 || !strings.Contains(out.String(), "-color") {
		t.Fatalf("-color=sometimes: exit %d\n%s", code, out.String())
	}
}

// TestDemoFastExits: -fast is for CI, so after the demo completes the command exits 0 on its own,
// where without it the command serves the viewer until interrupted. It says nothing about Ctrl-C,
// and with -keep it still prints the bide view command that reopens the run.
func TestDemoFastExits(t *testing.T) {
	var out bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- Main([]string{"-fast", "-no-browser", "-keep"}, []string{"demo"}, &out, &out) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("-fast exited %d\n%s", code, out.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatalf("-fast did not exit; it is serving the viewer\n%s", out.String())
	}
	text := out.String()
	if strings.Contains(text, "Ctrl-C") || !strings.Contains(text, "bide view -db ") {
		t.Errorf("-fast output:\n%s", text)
	}
	if i := strings.Index(text, "kept    "); i >= 0 {
		dir := strings.Fields(text[i:])[1]
		t.Cleanup(func() { os.RemoveAll(dir) })
	}
}
