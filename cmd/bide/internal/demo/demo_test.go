package demo

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/term"
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
		"\x1b[31m2\x1b[0m \x1b[1m\x1b[31mCrash  \x1b[0m\x1b[0m",
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
