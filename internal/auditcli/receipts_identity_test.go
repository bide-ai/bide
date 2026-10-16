package auditcli

import (
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/testcertreceipt"
)

func certArgs(f *testcertreceipt.Fixture, receipt string, extra ...string) []string {
	return append([]string{"verify-receipts", "-receipt", receipt, "-journal", f.Journal, "-heads", f.Heads, "-pubkey", f.LogKey}, extra...)
}

func TestVerifyReceipts_CertBoundWithPolicy(t *testing.T) {
	f := testcertreceipt.New(t)
	for name, receipt := range map[string]string{"with its proof": f.Receipt, "proof taken from the heads": f.NoProof} {
		code, out, errs := runCLI(certArgs(f, receipt, "-identity-policy", f.Policy)...)
		if code != 0 {
			t.Fatalf("%s: %d\n%s\n%s", name, code, out, errs)
		}
		for _, want := range []string{"PASS  alice approved", f.Identifier, f.DisplayName, "2026-10-13T12:15:03", "log-signer clock", "revocation status not checked", "not the gate's verdict"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s: missing %q in\n%s", name, want, out)
			}
		}
		// The identity check is not a verdict: the output never says the decision counted or is final.
		for _, bad := range []string{"final", "provisional", "counted"} {
			if strings.Contains(out, bad) {
				t.Fatalf("%s: %q in\n%s", name, bad, out)
			}
		}
	}
}

func TestVerifyReceipts_CertBoundNeedsAPolicy(t *testing.T) {
	f := testcertreceipt.New(t)
	code, out, errs := runCLI(certArgs(f, f.Receipt)...)
	if code == 0 || !strings.Contains(out+errs, "-identity-policy") {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
}

func TestVerifyReceipts_CertBoundRefusals(t *testing.T) {
	f := testcertreceipt.New(t)
	// A policy that trusts another root.
	code, out, _ := runCLI(certArgs(f, f.Receipt, "-identity-policy", f.OtherPolicy)...)
	if code != 1 || !strings.Contains(out, "FAIL  alice approved") {
		t.Fatalf("other root: %d\n%s", code, out)
	}
	// No head reaches the decision: the identity time is unknown, so nothing is accepted.
	args := certArgs(f, f.NoProof, "-identity-policy", f.Policy)
	args[6] = f.HeadsBefore
	code, out, _ = runCLI(args...)
	if code != 0 || !strings.Contains(out, "pending") || strings.Contains(out, f.Identifier) {
		t.Fatalf("pending: %d\n%s", code, out)
	}
	// A policy file that does not load is a usage-class failure, never a weaker check.
	if code, _, errs := runCLI(certArgs(f, f.Receipt, "-identity-policy", f.Dir+"/absent.json")...); code == 0 || errs == "" {
		t.Fatalf("absent policy: %d %s", code, errs)
	}
}
