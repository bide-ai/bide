package demo

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
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
	res, err := demo(context.Background(), dir, config{}, newPrinter(&out), true)
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

	text := out.String()
	for _, want := range []string{"process killed mid-charge", "outcome unknown: not charging again", "provider charges: 1", "charge ch_1 found", "run completed", "charged exactly once."} {
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
	for _, want := range []string{"attempt claimed: tool charge-1", "reconciled by ResolveHalt", "ch_1", "2 of 3: signatures verified", "<b>Verified</b> (evidence package)", "completed"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// TestDemoCatchesDoubleCharge declares the charge retry-safe, which it is not: bide then retries
// it after the crash, the provider charges twice, and the demo must fail rather than report a
// single charge.
func TestDemoCatchesDoubleCharge(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	res, err := demo(context.Background(), dir, config{RetrySafe: true}, newPrinter(&out), true)
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
