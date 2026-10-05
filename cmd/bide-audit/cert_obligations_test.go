package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bide-ai/bide/audit"
)

// A certificate written before govern carried gsm v0.13.0's delivery obligations has none of
// their keys. It still reads strictly and verifies, and the report says it records no
// obligations rather than implying there are none.
func TestVerifyConvergence_LegacyCertificateVerifies(t *testing.T) {
	legacy := readGolden(t, "certificate-legacy.json")
	var m confluenceCert
	if err := audit.UnmarshalStrict(legacy, &m); err != nil {
		t.Fatalf("the mirror does not read a certificate written before the obligations: %v", err)
	}

	dir := t.TempDir()
	bin := buildCLI(t, dir)
	agree := fakeChecker(t, dir, "agree", 0, "compensation_free=false")
	policy, digest := testPolicy()
	p, _ := json.Marshal(policy)
	cert := `{"machine":"kyc","policy_digest":"` + digest + `","converges":true,"wfc":true,"cc":true,"max_repair_len":1,` +
		`"pairs_total":1,"pairs_disjoint":0,"pairs_brute":1,"states":4,"compensation_free":false}`
	paths, pub := leafFiles(t, dir,
		valueLeaf("audit:convergence:"+digest, `{"digest":"`+digest+`","certificate":`+cert+`}`),
		valueLeaf("audit:policy:"+digest, `{"digest":"`+digest+`","policy":`+string(p)+`}`))
	code, out := exitCode(t, bin, "verify-convergence", "-cert-bundle", paths[0], "-policy-bundle", paths[1], "-pubkey", pub, "-checker", agree)
	if code != 0 {
		t.Fatalf("a certificate written before the obligations: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "records no delivery obligations") {
		t.Errorf("the report does not say the certificate predates the obligations:\n%s", out)
	}
}

// A certificate that names obligations verifies, and the report states each one.
func TestVerifyConvergence_PrintsObligations(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	agree := fakeChecker(t, dir, "agree", 0, "compensation_free=false")
	policy, digest := testPolicy()
	p, _ := json.Marshal(policy)
	b, err := json.Marshal(confluenceCert{Machine: "kyc", PolicyDigest: digest, Converges: true, WFC: true, CC: true,
		MaxRepairLen: 1, PairsTotal: 1, PairsBrute: 1, States: 4, PairsUndeclared: 2,
		CausalOrderRequired: []eventPair{{First: "open", Second: "close"}},
		NotIdempotent:       []string{"inc"},
		Saturations:         []saturation{{Rule: `event "inc"`, Var: "n", States: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	paths, pub := leafFiles(t, dir,
		valueLeaf("audit:convergence:"+digest, `{"digest":"`+digest+`","certificate":`+string(b)+`}`),
		valueLeaf("audit:policy:"+digest, `{"digest":"`+digest+`","policy":`+string(p)+`}`))
	code, out := exitCode(t, bin, "verify-convergence", "-cert-bundle", paths[0], "-policy-bundle", paths[1], "-pubkey", pub, "-checker", agree)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"causal order required for 1 undeclared pair(s): open/close",
		"exactly once: inc",
		`saturation: event "inc" clamps its write to n on 2 state(s)`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not state %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "records no delivery obligations") {
		t.Errorf("a certificate that records obligations is reported as recording none:\n%s", out)
	}
}
