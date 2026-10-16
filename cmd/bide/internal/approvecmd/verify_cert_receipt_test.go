package approvecmd

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/testcertreceipt"
)

func verifyCert(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var o, e bytes.Buffer
	code := Main(append([]string{"-verify-receipt"}, append(args, "-color", "never")...), strings.NewReader(""), &o, &e, Stores{})
	return code, o.String(), e.String()
}

func TestVerifyReceipt_CertBound(t *testing.T) {
	f := testcertreceipt.New(t)
	code, out, errs := verifyCert(t, f.Receipt, "-identity-policy", f.Policy, "-log-key", f.LogKey)
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	contains(t, "identity path", out, "identity "+f.Identifier, f.DisplayName, "not part of the identity", "accepted for alice", "2026-10-13T12:15:03", "log-signer clock",
		"revocation status was not checked", "not the gate's verdict")
	for _, bad := range []string{"signature verifies", "provisional "} {
		if strings.Contains(out, bad) {
			t.Fatalf("%q in\n%s", bad, out)
		}
	}
}

func TestVerifyReceipt_CertBoundRefusals(t *testing.T) {
	f := testcertreceipt.New(t)
	// A certificate-bound receipt with only -approver-keys: say what is missing, do not guess.
	code, _, errs := verifyCert(t, f.Receipt, "-approver-keys", f.Policy)
	if code != 1 || !strings.Contains(errs, "-identity-policy") {
		t.Fatalf("no policy: %d %s", code, errs)
	}
	// A policy that trusts another root.
	if code, _, errs := verifyCert(t, f.Receipt, "-identity-policy", f.OtherPolicy, "-log-key", f.LogKey); code != 1 || !strings.Contains(errs, "does not verify") {
		t.Fatalf("other root: %d %s", code, errs)
	}
	// No anchoring proof: the time is unknown, nothing is accepted.
	if code, out, errs := verifyCert(t, f.NoProof, "-identity-policy", f.Policy, "-log-key", f.LogKey); code != 1 || strings.Contains(out, f.Identifier) || !strings.Contains(errs, "anchoring proof") {
		t.Fatalf("no proof: %d %s %s", code, out, errs)
	}
	// The wrong log key does not vouch for the proof.
	if code, _, _ := verifyCert(t, f.Receipt, "-identity-policy", f.Policy, "-log-key", "ed25519:"+strings.Repeat("00", 32)); code != 1 {
		t.Fatalf("wrong log key: %d", code)
	}
	// Flags that go together, and flags that go with -verify-receipt only.
	for _, args := range [][]string{
		{"-verify-receipt", f.Receipt, "-identity-policy", f.Policy},
		{"-verify-receipt", f.Receipt, "-log-key", f.LogKey},
		{"-list", "-db", "x", "-identity-policy", f.Policy},
	} {
		if code := Main(args, strings.NewReader(""), io.Discard, io.Discard, Stores{}); code != 2 {
			t.Errorf("%v: %d, want 2", args, code)
		}
	}
}
