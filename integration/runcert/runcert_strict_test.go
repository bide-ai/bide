package runcert_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// strictRun journals a governed run whose policy and convergence leaves carry the given Result
// bytes verbatim (as a producer that writes its own journal can), certifies it, and returns the
// certificate, the approved digest, and the key.
func strictRun(t *testing.T, digest string, policyLeaf, convLeaf []byte) (audit.RunCertificate, ed25519.PublicKey) {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "run-strict"
	for _, r := range []agent.Record{
		{Name: "audit:policy:" + digest, Kind: agent.StepValue, Result: policyLeaf},
		{Name: "audit:convergence:" + digest, Kind: agent.StepValue, Result: convLeaf},
		{Name: "action", Kind: agent.StepToolResult, ToolUseID: "call_1",
			Result: []byte(`{"event":"approve","applied":true,"policy_digest":"` + digest + `"}`)},
	} {
		if _, err := store.Do(ctx, runID, r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := audit.CertifyRun(ctx, store, runID, audit.SignTreeHead(th, priv), audit.RunCertSpec{ApprovedPolicies: []string{digest}}, priv, 2)
	if err != nil {
		t.Fatalf("CertifyRun: %v", err)
	}
	return cert, pub
}

// VerifyRun reads the policy and convergence leaves strictly: a leaf whose JSON a reader would read
// differently from encoding/json (a case-variant or unknown name, a lone surrogate escape) does not
// verify, so the policy bytes and certificate a caller cross-checks are the ones the file shows.
func TestVerifyRun_LeavesReadAsWritten(t *testing.T) {
	digest, policyBytes, certBytes := buildKYC(t, "kyc-decision", false)
	policy, _ := json.Marshal(string(policyBytes))
	goodPolicy := `{"digest":"` + digest + `","policy":` + string(policy) + `}`
	goodConv := `{"digest":"` + digest + `","certificate":` + string(certBytes) + `}`

	cert, pub := strictRun(t, digest, []byte(goodPolicy), []byte(goodConv))
	res, err := audit.VerifyRun(cert, []string{digest}, pub)
	if err != nil || !res.OK {
		t.Fatalf("genuine leaves: %+v, %v", res, err)
	}
	// What VerifyRun verified is what it hands back for the oracle cross-check.
	want := []audit.VerifiedPolicy{{Digest: digest, Policy: string(policyBytes), Certificate: certBytes}}
	if len(res.Policies) != 1 || res.Policies[0].Digest != want[0].Digest || res.Policies[0].Policy != want[0].Policy ||
		string(res.Policies[0].Certificate) != string(want[0].Certificate) {
		t.Fatalf("Policies = %+v, want %+v", res.Policies, want)
	}
	// Leaves that verify under a certificate that does not are not handed back either.
	bad := cert
	bad.Properties = []string{"only-approved-policies"}
	if res, err := audit.VerifyRun(bad, []string{digest}, pub); err != nil || res.OK || len(res.Policies) != 0 {
		t.Fatalf("certificate with a wrong property list: %+v, %v; want not OK and no Policies", res, err)
	}

	for name, leaves := range map[string][2]string{
		// encoding/json matches "Policy" to the policy field; a reader sees a leaf with no policy.
		"case-variant policy name": {`{"digest":"` + digest + `","Policy":` + string(policy) + `}`, goodConv},
		"unknown policy field":     {strings.TrimSuffix(goodPolicy, "}") + `,"approved_by":"cfo"}`, goodConv},
		"case-variant cert name":   {goodPolicy, `{"digest":"` + digest + `","Certificate":` + string(certBytes) + `}`},
		"unknown cert field":       {goodPolicy, strings.TrimSuffix(goodConv, "}") + `,"approved_by":"cfo"}`},
		"lone surrogate in policy": {`{"digest":"` + digest + `","policy":"\ud800"}`, goodConv},
	} {
		cert, pub := strictRun(t, digest, []byte(leaves[0]), []byte(leaves[1]))
		res, err := audit.VerifyRun(cert, []string{digest}, pub)
		if err == nil && (res.OK || res.ConvergenceCertified) {
			t.Errorf("%s: VerifyRun accepted it: %+v", name, res)
		}
		if len(res.Policies) != 0 {
			t.Errorf("%s: VerifyRun handed back unverified contents %+v", name, res.Policies)
		}
	}
}
