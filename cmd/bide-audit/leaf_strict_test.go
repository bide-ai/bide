package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// The leaves these tests commit are written by hand, as a producer that controls its own journal
// can: each one is signed and proven, so only the CLI's reading of the leaf stands between a file
// that shows a reader one thing and a verdict on another.

// testPolicy is policy text and its digest as the CLI recomputes it.
func testPolicy() (policy, digest string) {
	policy = "(doms 2 2)\n(ev (do (set 0 (lit 1))))\n"
	h := sha256.Sum256([]byte(policyFormatVersion + "\n" + policy))
	return policy, hex.EncodeToString(h[:])
}

// leafFiles journals recs in one run, signs a head over all of them under a fresh key, and writes a
// proof bundle file for each record into a new directory under dir. It returns the paths in record
// order and the public key as hex.
func leafFiles(t *testing.T, dir string, recs ...agent.Record) ([]string, string) {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	for _, r := range recs {
		if _, err := store.Do(ctx, "run1", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, "run1", 1)
	if err != nil {
		t.Fatal(err)
	}
	sth := audit.SignTreeHead(th, priv)
	sub, err := os.MkdirTemp(dir, "leaves")
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(recs))
	for i := range recs {
		b, err := audit.ProveRecord(ctx, store, "run1", i, sth)
		if err != nil {
			t.Fatal(err)
		}
		paths[i] = filepath.Join(sub, fmt.Sprintf("leaf%d.json", i))
		writeJSON(t, paths[i], b)
	}
	return paths, hex.EncodeToString(pub)
}

func valueLeaf(name, result string) agent.Record {
	return agent.Record{Name: name, Kind: agent.StepValue, Result: json.RawMessage(result)}
}

func toolLeaf(name, result string) agent.Record {
	return agent.Record{Name: name, Kind: agent.StepToolResult, ToolUseID: name, Result: json.RawMessage(result)}
}

// edit applies one literal replacement and fails the test if it does not apply.
func edit(t *testing.T, s, old, new string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("edit %q does not apply to %s", old, s)
	}
	return strings.Replace(s, old, new, 1)
}

// testCert is a genuine serialized convergence certificate for digest. It is encoded through the
// CLI's mirror of govern.ConfluenceCertificate, which TestWireMirrorsMatchGovern holds to govern's
// encoding field for field.
func testCert(t *testing.T, digest string) string {
	t.Helper()
	b, err := json.Marshal(confluenceCert{Machine: "kyc", PolicyDigest: digest, Converges: true, WFC: true, CC: true,
		MaxRepairLen: 1, PairsTotal: 1, PairsBrute: 1, States: 4})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// verify-governed-action reads the action and policy leaves as a reader does: by exact names.
func TestVerifyGovernedAction_LeavesReadAsWritten(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	policy, digest := testPolicy()
	p, _ := json.Marshal(policy)
	action := `{"event":"approve","applied":true,"policy_digest":"` + digest + `","state_digest":"abc","actor":"agent-7"}`
	policyLeaf := `{"digest":"` + digest + `","policy":` + string(p) + `}`
	run := func(action, policyLeaf string) (int, string) {
		paths, pub := leafFiles(t, dir, toolLeaf("act", action), valueLeaf("audit:policy:"+digest, policyLeaf))
		return exitCode(t, bin, "verify-governed-action", "-action", paths[0], "-policy-bundle", paths[1], "-pubkey", pub)
	}
	if code, out := run(action, policyLeaf); code != 0 {
		t.Fatalf("genuine leaves (the action payload may carry other fields): exit %d\n%s", code, out)
	}
	for name, leaves := range map[string][2]string{
		// encoding/json matches Policy_Digest to policy_digest; a reader sees no policy digest.
		"case-variant digest in the action": {edit(t, action, `"policy_digest"`, `"Policy_Digest"`), policyLeaf},
		"case-variant policy name":          {action, edit(t, policyLeaf, `"policy"`, `"Policy"`)},
		"unknown policy leaf field":         {action, edit(t, policyLeaf, `{`, `{"approved_by":"cfo",`)},
	} {
		if code, out := run(leaves[0], leaves[1]); code == 0 {
			t.Errorf("%s: verify-governed-action exited 0\n%s", name, out)
		}
	}
}

// verify-convergence reads the convergence leaf, its certificate, and the policy leaf strictly, so
// the claim cross-checked against the oracle is the claim the file shows.
func TestVerifyConvergence_LeavesReadAsWritten(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	agree := fakeChecker(t, dir, "agree", 0, "compensation_free=false")
	policy, digest := testPolicy()
	p, _ := json.Marshal(policy)
	cert := testCert(t, digest)
	convLeaf := `{"digest":"` + digest + `","certificate":` + cert + `}`
	policyLeaf := `{"digest":"` + digest + `","policy":` + string(p) + `}`
	run := func(convLeaf, policyLeaf string) (int, string) {
		paths, pub := leafFiles(t, dir, valueLeaf("audit:convergence:"+digest, convLeaf), valueLeaf("audit:policy:"+digest, policyLeaf))
		return exitCode(t, bin, "verify-convergence", "-cert-bundle", paths[0], "-policy-bundle", paths[1], "-pubkey", pub, "-checker", agree)
	}
	if code, out := run(convLeaf, policyLeaf); code != 0 {
		t.Fatalf("genuine leaves: exit %d\n%s", code, out)
	}
	for name, leaves := range map[string][2]string{
		"case-variant convergence leaf name": {edit(t, convLeaf, `"digest"`, `"Digest"`), policyLeaf},
		"unknown convergence leaf field":     {edit(t, convLeaf, `{`, `{"approved_by":"cfo",`), policyLeaf},
		"case-variant policy name":           {convLeaf, edit(t, policyLeaf, `"policy"`, `"Policy"`)},
		// The certificate a reader sees makes no convergence claim; encoding/json reads converges=true.
		"case-variant certificate claim":    {edit(t, convLeaf, `"converges"`, `"Converges"`), policyLeaf},
		"unknown certificate field":         {edit(t, convLeaf, `"machine"`, `"approved_by":"cfo","machine"`), policyLeaf},
		"lone surrogate in the certificate": {edit(t, convLeaf, `"machine":"kyc"`, `"machine":"\ud800"`), policyLeaf},
	} {
		if code, out := run(leaves[0], leaves[1]); code == 0 {
			t.Errorf("%s: verify-convergence exited 0\n%s", name, out)
		}
	}
}

// verify-quorum reads the tally, the votes, and the commit as a reader does.
func TestVerifyQuorum_LeavesReadAsWritten(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	_, digest := testPolicy()
	tally := `{"decision":"approve","votes_for":2,"total":2,"agreed":true,"votes":[{"voter":"a","decision":"approve"},{"voter":"b","decision":"approve"}]}`
	voteA, voteB := `{"voter":"a","decision":"approve"}`, `{"voter":"b","decision":"approve"}`
	commit := `{"event":"approve","applied":true,"policy_digest":"` + digest + `"}`
	run := func(tally, voteA, voteB, commit string) (int, string) {
		paths, pub := leafFiles(t, dir, valueLeaf("quorum/q/vote/a", voteA), valueLeaf("quorum/q/vote/b", voteB),
			valueLeaf("quorum/q/tally", tally), toolLeaf("commit", commit))
		return exitCode(t, bin, "verify-quorum", "-name", "q", "-tally", paths[2], "-vote", paths[0], "-vote", paths[1],
			"-commit", paths[3], "-pubkey", pub, "-k", "2")
	}
	if code, out := run(tally, voteA, voteB, commit); code != 0 {
		t.Fatalf("genuine leaves: exit %d\n%s", code, out)
	}
	for name, l := range map[string][4]string{
		"case-variant tally name":             {edit(t, tally, `"votes_for"`, `"Votes_For"`), voteA, voteB, commit},
		"unknown tally field":                 {edit(t, tally, `{`, `{"approved_by":"cfo",`), voteA, voteB, commit},
		"case-variant name in a tallied vote": {edit(t, tally, `{"voter":"a"`, `{"Voter":"a"`), voteA, voteB, commit},
		"case-variant vote name":              {tally, edit(t, voteA, `"decision"`, `"Decision"`), voteB, commit},
		"unknown vote field":                  {tally, voteA, edit(t, voteB, `{`, `{"weight":3,`), commit},
		"case-variant digest in the commit":   {tally, voteA, voteB, edit(t, commit, `"policy_digest"`, `"Policy_Digest"`)},
		// encoding/json reads the escape as U+FFFD; the file shows no such digest.
		"lone surrogate in the commit digest": {tally, voteA, voteB, `{"policy_digest":"\ud800"}`},
	} {
		if code, out := run(l[0], l[1], l[2], l[3]); code == 0 {
			t.Errorf("%s: verify-quorum exited 0\n%s", name, out)
		}
	}
}

// verify-run -checker cross-checks the certificate VerifyRun verified, read strictly.
func TestVerifyRun_CheckerReadsCertificateAsWritten(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	agree := fakeChecker(t, dir, "agree", 0, "compensation_free=false")
	policy, digest := testPolicy()
	cert := testCert(t, digest)
	run := func(cert string) (int, string) {
		ctx := context.Background()
		store := agent.NewMemStore()
		if _, err := audit.RecordPolicy(ctx, store, "run1", []byte(policy), digest); err != nil {
			t.Fatal(err)
		}
		if _, err := audit.RecordConvergence(ctx, store, "run1", []byte(cert), digest); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Do(ctx, "run1", "act", func(context.Context) (agent.Record, error) {
			return toolLeaf("act", `{"event":"approve","applied":true,"policy_digest":"`+digest+`"}`), nil
		}); err != nil {
			t.Fatal(err)
		}
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		th, err := audit.NewTreeHead(ctx, store, "run1", 1)
		if err != nil {
			t.Fatal(err)
		}
		rc, err := audit.CertifyRun(ctx, store, "run1", audit.SignTreeHead(th, priv), audit.RunCertSpec{ApprovedPolicies: []string{digest}}, priv, 2)
		if err != nil {
			t.Fatal(err)
		}
		sub, _ := os.MkdirTemp(dir, "run")
		path := filepath.Join(sub, "runcert.json")
		writeJSON(t, path, rc)
		return exitCode(t, bin, "verify-run", "-cert", path, "-pubkey", hex.EncodeToString(pub), "-approved", digest, "-checker", agree)
	}
	if code, out := run(cert); code != 0 {
		t.Fatalf("genuine certificate: exit %d\n%s", code, out)
	}
	for name, c := range map[string]string{
		"case-variant claim":  edit(t, cert, `"converges"`, `"Converges"`),
		"unknown claim field": edit(t, cert, `"machine"`, `"approved_by":"cfo","machine"`),
	} {
		if code, out := run(c); code == 0 {
			t.Errorf("%s: verify-run exited 0\n%s", name, out)
		}
	}
}

// governedPolicyDigest reads an open payload: other names are allowed, but the digest comes from
// the exact name only, and a payload that does not decode strictly is an error.
func TestGovernedPolicyDigest(t *testing.T) {
	for in, want := range map[string]string{
		`{"policy_digest":"d","actor":"a","extra":{"x":[1]}}`: "d",
		`{"Policy_Digest":"x","policy_digest":"d"}`:           "d",
	} {
		if got, err := governedPolicyDigest(json.RawMessage(in)); err != nil || got != want {
			t.Errorf("governedPolicyDigest(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		`{"Policy_Digest":"d"}`,
		`{"policy_digest":"a","policy_digest":"d"}`,
		`{"policy_digest":"d","x":{"k":1,"k":2}}`,
		`{"policy_digest":"\udc00"}`,
		`{"policy_digest":7}`,
		`["policy_digest","d"]`,
		`{"policy_digest":"d"} {}`,
	} {
		if got, err := governedPolicyDigest(json.RawMessage(in)); err == nil {
			t.Errorf("governedPolicyDigest(%s) = %q, want an error", in, got)
		}
	}
}

// The CLI mirrors govern's wire types so it imports neither gsm nor govern. Each mirror must read
// every field govern writes, and nothing else. govern is a separate module, so the check is split at
// the golden files in testdata/govern-wire: the integration module
// (integration/bideaudit/wire_test.go) proves they are govern's encoding of the same values and
// that each govern type has one field per key, and this test proves that a fully populated value
// strict-decodes into the mirror and re-encodes to the same JSON, that the mirror has one field per
// key, and that the zero mirror encodes as govern's zero value does (no field the mirror omits).
func TestWireMirrorsMatchGovern(t *testing.T) {
	for name, mirror := range map[string]func() any{
		"certificate": func() any { return new(confluenceCert) },
		"tally":       func() any { return new(quorumTally) },
		"vote":        func() any { return new(quorumVote) },
	} {
		want := readGolden(t, name+".json")
		m := mirror()
		if err := audit.UnmarshalStrict(want, m); err != nil {
			t.Errorf("%s: the mirror does not read govern's encoding: %v", name, err)
			continue
		}
		got, _ := json.Marshal(m)
		if string(got) != string(want) {
			t.Errorf("%s: the mirror reads %s as %s", name, want, got)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(want, &keys); err != nil {
			t.Fatalf("%s: golden: %v", name, err)
		}
		if n := reflect.TypeOf(m).Elem().NumField(); n != len(keys) {
			t.Errorf("%s: the mirror has %d fields, govern's encoding %d", name, n, len(keys))
		}
		zero, _ := json.Marshal(mirror())
		if want := readGolden(t, name+"-zero.json"); string(zero) != string(want) {
			t.Errorf("%s: the zero mirror encodes as %s, govern's zero value as %s", name, zero, want)
		}
	}
}

// readGolden reads a file of govern's wire encoding from testdata/govern-wire.
func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "govern-wire", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
