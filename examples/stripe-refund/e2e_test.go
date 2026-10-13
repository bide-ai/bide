package main

// End-to-end tests for the refund agent against a fake Stripe (fake_test.go). Each actor is a
// separate process on one on-disk SQLite journal, so a crash is a real process exit and a restart
// is a fresh process. The observable that matters is the fake's own list of refunds: Stripe's
// record of what happened, which the journal can be checked against.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/store/sqlite"
)

const testKey = "sk_test_SECRETVALUE0123456789"

var (
	buildOnce sync.Once
	binPath   string
	buildErr  string
)

// binary compiles the example once. It skips, rather than fails, when the toolchain is
// unavailable, following the repo's prerequisite convention.
func binary(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping cross-process e2e")
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "stripe-refund-bin-*")
		if err != nil {
			buildErr = err.Error()
			return
		}
		name := "stripe-refund"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		binPath = filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = err.Error() + "\n" + string(out)
		}
	})
	if buildErr != "" {
		t.Skipf("go build failed (toolchain or deps unavailable): %s", buildErr)
	}
	return binPath
}

// proc runs the example once against base with key, returning its combined output and exit code.
func proc(t *testing.T, base, key string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Env = append(os.Environ(), "STRIPE_API_BASE="+base, "STRIPE_SECRET_KEY="+key)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("exec %v: %v\n%s", args, err, out)
	}
	return string(out), 0
}

func mustProc(t *testing.T, f *fakeStripe, args ...string) string {
	t.Helper()
	out, code := proc(t, f.URL(), f.key, args...)
	if code != 0 {
		t.Fatalf("%v exited %d:\n%s", args, code, out)
	}
	if strings.Contains(out, "SECRETVALUE") {
		t.Fatalf("the API key was printed:\n%s", out)
	}
	return out
}

func want(t *testing.T, out string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(out, s) {
			t.Fatalf("output missing %q:\n%s", s, out)
		}
	}
}

// journal reads a run's records from the on-disk journal once no process holds it.
func journal(t *testing.T, db, runID string) []agent.Record {
	t.Helper()
	store, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	j, err := agent.NewJournal(store)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := j.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// refundRecords returns the refund call's attempt markers and its result record, if any.
func refundRecords(recs []agent.Record) (attempts int, result *agent.Record) {
	for i, r := range recs {
		if r.ToolUseID != refundID {
			continue
		}
		switch r.Kind {
		case agent.StepAttempt:
			attempts++
		case agent.StepToolResult:
			result = &recs[i]
		}
	}
	return attempts, result
}

func setup(t *testing.T, pis ...fakePI) (*fakeStripe, string) {
	t.Helper()
	if len(pis) == 0 {
		pis = []fakePI{{ID: "pi_test_1", Status: "succeeded", Received: 5000, Currency: "usd"}}
	}
	return newFakeStripe(t, testKey, pis...), filepath.Join(t.TempDir(), "refund.db")
}

const runID = "stripe-refund-pi_test_1"

// TestCrashAfterSendThenReconcile is the example's point. The process dies right after the refund
// request went out. The restart halts instead of refunding again, and the reconciler resolves the
// halt from Stripe's own record. One refund exists, however many restarts follow.
func TestCrashAfterSendThenReconcile(t *testing.T) {
	f, db := setup(t)

	out, code := proc(t, f.URL(), f.key, "run", "-db", db, "-payment-intent", "pi_test_1", "-crash-after-send")
	if code != 3 {
		t.Fatalf("crash run exited %d, want 3:\n%s", code, out)
	}
	want(t, out, "step 1  lookup", "-crash-after-send")
	if n := f.refundCount(); n != 1 {
		t.Fatalf("%d refunds at Stripe after the crash, want 1", n)
	}
	// The journal knows the refund was attempted and never learned the result.
	attempts, result := refundRecords(journal(t, db, runID))
	if attempts != 1 || result != nil {
		t.Fatalf("after the crash: %d attempt markers and result %v, want 1 and none", attempts, result)
	}

	// Without a reconciler the restart halts, and sends nothing.
	out = mustProc(t, f, "run", "-db", db, "-payment-intent", "pi_test_1", "-reconcile=false")
	want(t, out, "status: halted", "outcome is unknown", "not reconciling")
	if n := f.refundCount(); n != 1 {
		t.Fatalf("%d refunds after the halted restart, want 1", n)
	}
	if _, result := refundRecords(journal(t, db, runID)); result != nil {
		t.Fatal("the halted restart recorded a result")
	}

	// With the reconciler it finds the refund at Stripe, resolves the halt, and finishes.
	out = mustProc(t, f, "run", "-db", db, "-payment-intent", "pi_test_1", "-min-halt-age", "0s")
	want(t, out, "status: halted", "reconcile: found re_fake_1", "resolved refund-1", "status: done", "Receipt: refunded 5000 usd (minor units) on pi_test_1 as re_fake_1 (succeeded).")
	if n := f.refundCount(); n != 1 {
		t.Fatalf("%d refunds after reconciling, want exactly 1", n)
	}
	if n := len(f.posts()); n != 1 {
		t.Fatalf("%d POST /v1/refunds requests in all, want 1", n)
	}

	// The journal shows the resolution: a reconciled result, with Stripe's record as evidence.
	_, result = refundRecords(journal(t, db, runID))
	if result == nil || !result.Reconciled {
		t.Fatalf("refund result = %+v, want a reconciled result", result)
	}
	var ev struct {
		Source  string
		Matched string
		Refund  refund
	}
	if err := json.Unmarshal(result.Evidence, &ev); err != nil {
		t.Fatalf("evidence %s: %v", result.Evidence, err)
	}
	if ev.Refund.ID != "re_fake_1" || ev.Matched != "metadata.bide_key="+runID+"/refund-1" {
		t.Fatalf("evidence = %+v, want re_fake_1 matched by the step's key", ev)
	}

	// A further restart changes nothing.
	out = mustProc(t, f, "run", "-db", db, "-payment-intent", "pi_test_1")
	want(t, out, "status: done")
	if f.refundCount() != 1 || len(f.posts()) != 1 {
		t.Fatalf("a finished run sent another request: %d refunds, %d posts", f.refundCount(), len(f.posts()))
	}

	// The request carried the idempotency key and the metadata the reconciler relies on.
	p := f.posts()[0]
	if p.Idem != runID+"/refund-1" || p.Form["metadata[bide_run]"] != runID || p.Form["metadata[bide_key]"] != p.Idem || p.Form["amount"] != "5000" {
		t.Fatalf("refund request = %+v", p)
	}
}

// TestReconcileWaitsOutYoungHalt: a halt younger than -min-halt-age is not resolved yet, because
// Stripe's record may not have settled; the reconciler waits, then resolves.
func TestReconcileWaitsOutYoungHalt(t *testing.T) {
	f, db := setup(t)
	if out, code := proc(t, f.URL(), f.key, "run", "-db", db, "-payment-intent", "pi_test_1", "-crash-after-send"); code != 3 {
		t.Fatalf("crash run exited %d:\n%s", code, out)
	}
	out := mustProc(t, f, "run", "-db", db, "-payment-intent", "pi_test_1", "-min-halt-age", "2500ms")
	want(t, out, "waiting", "to settle", "resolved refund-1", "status: done")
	if n := strings.Count(out, "reconcile: asking Stripe"); n != 1 {
		t.Fatalf("asked Stripe %d times, want once, after the wait:\n%s", n, out)
	}
	if f.refundCount() != 1 {
		t.Fatalf("%d refunds, want 1", f.refundCount())
	}
}

// TestReconcileFindsNothing: if Stripe has no refund with the step's key, the reconciler does not
// guess. The step stays halted for a human, and nothing is sent.
func TestReconcileFindsNothing(t *testing.T) {
	f, db := setup(t)
	if _, code := proc(t, f.URL(), f.key, "run", "-db", db, "-payment-intent", "pi_test_1", "-crash-after-send"); code != 3 {
		t.Fatal("no crash")
	}
	// Stripe loses the refund (here: the fake forgets it), as if the request never arrived.
	f.mu.Lock()
	f.refunds = nil
	f.mu.Unlock()
	out := mustProc(t, f, "run", "-db", db, "-payment-intent", "pi_test_1", "-min-halt-age", "0s")
	want(t, out, "status: halted", "no refund at Stripe carries that key", "A human decides")
	if strings.Contains(out, "status: done") || f.refundCount() != 0 || len(f.posts()) != 1 {
		t.Fatalf("unresolved halt must not complete or resend: %d refunds, %d posts\n%s", f.refundCount(), len(f.posts()), out)
	}
}

// TestApprovalGateThenCrash: over the threshold the refund waits for 2 of 3 approvers; then the
// same crash and reconcile story holds for the gated refund.
func TestApprovalGateThenCrash(t *testing.T) {
	f, db := setup(t)
	base := []string{"-db", db, "-payment-intent", "pi_test_1", "-approval-threshold", "1000"}
	args := func(more ...string) []string { return append(append([]string{"run"}, base...), more...) }

	out := mustProc(t, f, args()...)
	want(t, out, "status: paused", "approved 0 of 2", "waiting on ops, finance, risk", "-run "+runID+" -as ops")
	mustProc(t, f, "approve", "-db", db, "-payment-intent", "pi_test_1", "-as", "ops")
	out = mustProc(t, f, args()...)
	want(t, out, "status: paused", "approved 1 of 2")
	if f.refundCount() != 0 {
		t.Fatal("refunded below quorum")
	}
	mustProc(t, f, "approve", "-db", db, "-payment-intent", "pi_test_1", "-as", "finance")

	if out, code := proc(t, f.URL(), f.key, args("-crash-after-send")...); code != 3 {
		t.Fatalf("crash run exited %d:\n%s", code, out)
	}
	out = mustProc(t, f, args("-min-halt-age", "0s")...)
	want(t, out, "status: halted", "resolved refund-1", "status: done")
	if f.refundCount() != 1 || len(f.posts()) != 1 {
		t.Fatalf("%d refunds, %d posts, want 1 and 1", f.refundCount(), len(f.posts()))
	}
}

// TestPartialRefundAndNothingToRefund covers the read-only lookup's guards.
func TestPartialRefundAndNothingToRefund(t *testing.T) {
	f, db := setup(t,
		fakePI{ID: "pi_test_1", Status: "succeeded", Received: 5000, Currency: "usd"},
		fakePI{ID: "pi_unpaid", Status: "requires_payment_method", Received: 0, Currency: "usd"})

	out := mustProc(t, f, "run", "-db", db, "-payment-intent", "pi_test_1", "-amount", "1200")
	want(t, out, "refundable 5000", "status: done", "refunded 1200 usd")
	out = mustProc(t, f, "run", "-db", db, "-payment-intent", "pi_test_1", "-run", "again", "-amount", "9999")
	want(t, out, "No refund: 9999 requested, 3800 refundable")
	out = mustProc(t, f, "run", "-db", db, "-payment-intent", "pi_unpaid")
	want(t, out, "nothing to refund")
	if f.refundCount() != 1 {
		t.Fatalf("%d refunds, want 1", f.refundCount())
	}
}

// TestNonTestKeyRefused: the example never touches live mode, and never prints the key.
func TestNonTestKeyRefused(t *testing.T) {
	f, db := setup(t)
	for _, key := range []string{"sk_live_ABCSECRET123", "rk_live_ABCSECRET123", "pk_test_ABCSECRET123", ""} {
		out, code := proc(t, f.URL(), key, "run", "-db", db, "-payment-intent", "pi_test_1")
		if code != 1 {
			t.Fatalf("key %q: exit %d, want 1:\n%s", key, code, out)
		}
		if key == "" {
			want(t, out, "STRIPE_SECRET_KEY is not set")
		} else {
			want(t, out, "not a test-mode secret key", "sk_test_")
		}
		if strings.Contains(out, "ABCSECRET123") {
			t.Fatalf("the refused key was printed:\n%s", out)
		}
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("%d requests reached Stripe with a refused key", n)
	}
	// The API base is limited too: a test key does not go to an arbitrary host.
	out, code := proc(t, "http://stripe.example.com", testKey, "run", "-db", db, "-payment-intent", "pi_test_1")
	if code != 1 || !strings.Contains(out, "STRIPE_API_BASE") {
		t.Fatalf("remote base: exit %d:\n%s", code, out)
	}
}

func TestCheckTestKeyAndBase(t *testing.T) {
	for key, ok := range map[string]bool{"sk_test_abc": true, "sk_live_abc": false, "rk_test_abc": false, "": false, " sk_test_abc": false} {
		if got := checkTestKey(key) == nil; got != ok {
			t.Errorf("checkTestKey(%q) ok = %v, want %v", key, got, ok)
		}
	}
	for base, ok := range map[string]bool{defaultBase: true, "http://127.0.0.1:1234": true, "http://localhost:80": true,
		"https://api.stripe.com.evil.example": false, "http://api.stripe.com": false, "https://127.0.0.1": false} {
		if got := checkBase(base) == nil; got != ok {
			t.Errorf("checkBase(%q) ok = %v, want %v", base, got, ok)
		}
	}
}

// TestListRefundsPaginates: the reconciler must see a refund on a later page.
func TestListRefundsPaginates(t *testing.T) {
	f, _ := setup(t)
	f.page(2)
	for i := 1; i <= 5; i++ {
		f.seed(refund{ID: "re_p" + string(rune('0'+i)), Amount: 1, PaymentIntent: "pi_test_1"})
	}
	api, err := newStripe(f.URL(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := api.listRefunds(context.Background(), "pi_test_1")
	if err != nil || len(got) != 5 {
		t.Fatalf("listRefunds = %d refunds, err %v; want 5", len(got), err)
	}
}

// TestFakeHonorsIdempotencyKey checks the fake models the one property the example leans on
// at the HTTP level: the same key and parameters never create a second refund.
func TestFakeHonorsIdempotencyKey(t *testing.T) {
	f, _ := setup(t)
	api, err := newStripe(f.URL(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := api.createRefund(ctx, "pi_test_1", 100, "k1", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := api.createRefund(ctx, "pi_test_1", 100, "k1", nil)
	if err != nil || b.ID != a.ID || f.refundCount() != 1 {
		t.Fatalf("replay: %+v err %v, %d refunds; want the first refund and 1", b, err, f.refundCount())
	}
	if _, err := api.createRefund(ctx, "pi_test_1", 200, "k1", nil); err == nil {
		t.Fatal("same key with different parameters was accepted")
	}
}

// TestReconcileAdoptsOnlyItsOwnRefund: the payment intent already has a refund from somewhere else.
// The reconciler must match on this step's key, not take the first refund it sees.
func TestReconcileAdoptsOnlyItsOwnRefund(t *testing.T) {
	f, db := setup(t)
	f.seed(refund{ID: "re_other", Amount: 1000, Currency: "usd", Status: "succeeded", PaymentIntent: "pi_test_1",
		Metadata: map[string]string{"bide_key": "some-other-run/refund-1"}})
	args := []string{"run", "-db", db, "-payment-intent", "pi_test_1", "-amount", "1500"}
	if out, code := proc(t, f.URL(), f.key, append(args, "-crash-after-send")...); code != 3 {
		t.Fatalf("crash run exited %d:\n%s", code, out)
	}
	out := mustProc(t, f, append(args, "-min-halt-age", "0s")...)
	want(t, out, "reconcile: found re_fake_2", "Receipt: refunded 1500 usd (minor units) on pi_test_1 as re_fake_2")
	if strings.Contains(out, "re_other") {
		t.Fatalf("adopted a refund that is not this step's:\n%s", out)
	}
}

// TestDemoScript runs demo.sh, the script the README points at, against the fake: it must end
// with exactly one refund at Stripe.
func TestDemoScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("demo.sh is a bash script")
	}
	for _, tool := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	binary(t) // skips when the toolchain is unavailable
	f, _ := setup(t)
	cmd := exec.Command("bash", "demo.sh", "pi_test_1")
	cmd.Env = append(os.Environ(), "STRIPE_API_BASE="+f.URL(), "STRIPE_SECRET_KEY="+f.key, "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("demo.sh: %v\n%s", err, out)
	}
	want(t, string(out), "status: paused", "status: halted", "not reconciling", "reconcile: found re_fake_1", "status: done", "matching refunds: 1")
	if f.refundCount() != 1 || len(f.posts()) != 1 {
		t.Fatalf("%d refunds, %d posts, want 1 and 1", f.refundCount(), len(f.posts()))
	}
	if strings.Contains(string(out), "SECRETVALUE") {
		t.Fatal("the API key was printed")
	}
}
