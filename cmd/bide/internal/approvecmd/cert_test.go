package approvecmd

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/policyfile"
	"github.com/bide-ai/bide/internal/testca"
)

// bide approve -cert: a certificate-bound decision, signed over the v4 bytes with the approver's own key,
// carrying the chain, after the key was checked against the leaf and the chain against the policy.

// signerKey turns a key file bide keygen wrote into the crypto.Signer it holds, so a test can have a
// certificate issued for it.
func signerKey(t *testing.T, path string) crypto.Signer {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(b))
	if hexSeed, ok := strings.CutPrefix(line, "ecdsa-p256:"); ok {
		d, err := hex.DecodeString(hexSeed)
		if err != nil {
			t.Fatal(err)
		}
		k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	seed, err := hex.DecodeString(line)
	if err != nil {
		t.Fatal(err)
	}
	return ed25519.NewKeyFromSeed(seed)
}

// pki is a CA hierarchy that issues a certificate to the key of an approver's key file, with the policy
// file and chain file bide approve reads.
type pki struct {
	root, inter *testca.CA
	dir         string
	policy      string // the -identity-policy file
	chain       string // the -cert file for ops (valid around testca.T0)
	leaf        *testca.Leaf
}

func newPKI(t *testing.T, f *fixture, as string) *pki {
	t.Helper()
	p := &pki{dir: t.TempDir()}
	p.root = testca.NewRoot(t, "root", testca.Opts{})
	p.inter = p.root.NewIntermediate(t, "inter", testca.Opts{})
	p.leaf = p.inter.NewLeaf(t, testca.Opts{KeyWith: signerKey(t, f.keys[as])})
	p.chain = p.writeChain(t, "chain.pem", p.leaf.Chain()...)
	p.policy = p.writePolicy(t, "policy.json", p.root, p.inter, as)
	return p
}

func (p *pki) writeChain(t *testing.T, name string, ders ...[]byte) string {
	t.Helper()
	var b bytes.Buffer
	for _, d := range ders {
		b.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d}))
	}
	path := filepath.Join(p.dir, name)
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (p *pki) writePolicy(t *testing.T, name string, root, inter *testca.CA, as string) string {
	t.Helper()
	pemPath := name + "-root.pem"
	if err := os.WriteFile(filepath.Join(p.dir, pemPath), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	spki := audit.SPKIHashOf(inter.Cert)
	b, _ := json.Marshal(map[string]any{
		"anchors": []string{pemPath}, "intermediates": []string{hex.EncodeToString(spki[:])},
		"identity": map[string]string{as: testca.Person(1)}, "id_kind": "person-uri",
		"eku": testca.EKU.String(), "policy_oid": testca.PolicyOID.String(), "canonical": map[string]string{"host": "idp.bank.example"},
	})
	path := filepath.Join(p.dir, name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// atT0 makes the command's clock testca.T0 plus d for the test.
func atT0(t *testing.T, d time.Duration) {
	t.Helper()
	old := now
	now = func() time.Time { return testca.T0.Add(d) }
	t.Cleanup(func() { now = old })
}

// certArgs are the flags of a certificate-bound decision by id with the PKI's chain and policy.
func (f *fixture) certArgs(id string, p *pki, extra ...string) []string {
	return append([]string{"-run", f.run, "-call", f.call, "-as", id, "-key", f.keys[id], "-cert", p.chain, "-identity-policy", p.policy}, extra...)
}

func newCertFixture(t *testing.T) (*fixture, *pki) {
	t.Helper()
	f := newFixtureBound(t, twoOfThree, refundArgs, audit.AlgEd25519, "", "ops")
	return f, newPKI(t, f, "ops")
}

func TestCert_SignsV4WithTheChainAndRecordsIt(t *testing.T) {
	atT0(t, time.Minute)
	f, p := newCertFixture(t)
	code, out, errs := f.run_("", f.certArgs("ops", p, "-yes")...)
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	contains(t, "output", out, "Approve this call?", "leaf SHA-256 "+hex.EncodeToString(sum(p.leaf.DER)), "identity", testca.Person(1), "accepted for ops by the policy now",
		"the hash of your certificate", "signed and recorded", "receipt")
	ds := f.decisions()
	if len(ds) != 1 {
		t.Fatalf("%d decisions", len(ds))
	}
	r := ds[0]
	if got := r.ApproverCert(); len(got) != len(p.leaf.Chain()) || !bytes.Equal(got[0], p.leaf.DER) {
		t.Fatalf("the record carries %d certificates", len(got))
	}
	// The signature is over the v4 bytes, under the leaf's key, binding the leaf's hash.
	at, err := agent.ParseTimestamp(r.DecidedAt())
	if err != nil {
		t.Fatal(err)
	}
	j, closeJ := f.store()
	defer closeJ()
	recs, _ := j.History(t.Context(), f.run)
	_, call, _ := agent.FindToolCall(recs, f.call)
	s := agent.ApprovalSubject{RunID: f.run, ToolUseID: f.call, ToolName: call.Name, Args: call.Args}
	pub := p.leaf.Key.Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, agent.ApprovalDecisionBytesCert(s, "ops", true, at, agent.CertLeafHash(p.leaf.DER)), r.Signature()) {
		t.Fatal("the signature is not over the v4 bytes")
	}
	if ed25519.Verify(pub, agent.ApprovalDecisionBytes(s, "ops", true, at), r.Signature()) {
		t.Fatal("the signature also verifies over the bare (v3) bytes")
	}
	// The policy accepts the recorded chain for ops at the decision time.
	ci, err := policyFor(t, p.policy)
	if err != nil {
		t.Fatal(err)
	}
	if ac, err := ci.VerifyChain("ops", r.ApproverCert(), at, at); err != nil || ac.Identity != testca.Person(1) {
		t.Fatalf("%v", err)
	}
	// The receipt carries the chain.
	found := false
	entries, _ := os.ReadDir(f.dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".receipt.json") {
			b, _ := os.ReadFile(filepath.Join(f.dir, e.Name()))
			var rc audit.DecisionReceipt
			if err := json.Unmarshal(b, &rc); err != nil || len(rc.Cert) != len(p.leaf.Chain()) {
				t.Fatalf("receipt %s: %v %d", e.Name(), err, len(rc.Cert))
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no receipt was written")
	}
	// The command does not claim the gate counted it.
	if strings.Contains(out, "verified tally") || strings.Contains(out, "another decision of yours") {
		t.Fatalf("the bare-key tally was shown for a certificate-bound decision:\n%s", out)
	}
}

func sum(b []byte) []byte { h := agent.CertLeafHash(b); return h[:] }

func policyFor(t *testing.T, path string) (*audit.CertIdentity, error) {
	t.Helper()
	return policyfile.Read(path)
}

// The key given must be the leaf's key, whatever else is right: another key of the same scheme, a key of
// another scheme, and the key of another approver. Nothing is signed or recorded.
func TestCert_KeyMustBeTheLeafKey(t *testing.T) {
	atT0(t, time.Minute)
	f := newFixtureBound(t, twoOfThree, refundArgs, audit.AlgEd25519, "", "ops")
	p := newPKI(t, f, "ops")
	g := newFixtureAlg(t, twoOfThree, refundArgs, audit.AlgECDSAP256)
	for name, key := range map[string]string{"another ed25519 key": f.keys["finance"], "another scheme": g.keys["ops"]} {
		args := f.certArgs("ops", p, "-yes")
		for i := range args {
			if args[i] == "-key" {
				args[i+1] = key
			}
		}
		code, out, errs := f.run_("", args...)
		if code != 1 || !strings.Contains(errs, "is not the key of the certificate's leaf") {
			t.Errorf("%s: %d\n%s\n%s", name, code, out, errs)
		}
		if n := len(f.decisions()); n != 0 {
			t.Errorf("%s: %d decisions were recorded", name, n)
		}
		if strings.Contains(errs, hexOfKeyFile(t, key)) || strings.Contains(out, hexOfKeyFile(t, key)) {
			t.Errorf("%s: the key file's content is in the output", name)
		}
	}
}

func hexOfKeyFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// Every failure before the signature leaves nothing recorded and never falls back to a bare-key decision.
func TestCert_FailuresRecordNothingAndNeverFallBack(t *testing.T) {
	f := newFixtureBound(t, twoOfThree, refundArgs, audit.AlgEd25519, "", "ops")
	p := newPKI(t, f, "ops")
	otherRoot := testca.NewRoot(t, "other", testca.Opts{})
	otherInter := otherRoot.NewIntermediate(t, "other-inter", testca.Opts{})
	otherPolicy := p.writePolicy(t, "other.json", otherRoot, otherInter, "ops")
	keyBlock := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("very-secret-key-bytes-0123456789")})
	withKey := filepath.Join(p.dir, "withkey.pem")
	os.WriteFile(withKey, append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.leaf.DER}), keyBlock...), 0o600)
	junk := filepath.Join(p.dir, "junk.pem")
	os.WriteFile(junk, append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.leaf.DER}), []byte("trailing words")...), 0o600)
	empty := filepath.Join(p.dir, "empty.pem")
	os.WriteFile(empty, nil, 0o600)
	notCert := filepath.Join(p.dir, "notcert.pem")
	os.WriteFile(notCert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not der")}), 0o600)
	tooMany := p.writeChain(t, "many.pem", append(p.leaf.Chain(), repeatDER(p.leaf.DER, 8)...)...)
	big := filepath.Join(p.dir, "big.pem")
	os.WriteFile(big, bytes.Repeat([]byte("x"), maxChainFile+1), 0o600)
	bigDER := p.writeChain(t, "bigder.pem", bytes.Repeat([]byte{0x30}, agent.MaxDecisionCertDER+1))
	wrongLeaf := p.writeChain(t, "wrongleaf.pem", p.inter.DER) // a CA certificate as the leaf, with a key that is not ours

	cases := []struct {
		name string
		at   time.Duration
		mod  func(args []string) []string
		want string
	}{
		{"a policy of another root", time.Minute, set("-identity-policy", otherPolicy), "not accepted for ops by the policy"},
		{"a private key block in the file", time.Minute, set("-cert", withKey), "private key block"},
		{"trailing data", time.Minute, set("-cert", junk), "not a PEM certificate block"},
		{"an empty file", time.Minute, set("-cert", empty), "holds no certificate"},
		{"a certificate that does not parse", time.Minute, set("-cert", notCert), "does not parse"},
		{"too many certificates", time.Minute, set("-cert", tooMany), "more than"},
		{"a file too large", time.Minute, set("-cert", big), "is larger than"},
		{"a certificate too large", time.Minute, set("-cert", bigDER), "is larger than"},
		{"a missing file", time.Minute, set("-cert", filepath.Join(p.dir, "absent.pem")), "-cert:"},
		{"a CA certificate as the leaf", time.Minute, set("-cert", wrongLeaf), "leaf"},
		{"expired", 2 * time.Hour, nil, "has expired"},
		{"not yet valid", -2 * time.Hour, nil, "not valid yet"},
		{"too little validity left", time.Hour - 30*time.Second, nil, "less than the 1m0s"},
	}
	for _, tc := range cases {
		atT0(t, tc.at)
		args := f.certArgs("ops", p, "-yes")
		if tc.mod != nil {
			args = tc.mod(args)
		}
		code, out, errs := f.run_("", args...)
		if code != 1 || !strings.Contains(errs, tc.want) {
			t.Errorf("%s: %d\nstdout: %s\nstderr: %s", tc.name, code, out, errs)
		}
		if n := len(f.decisions()); n != 0 {
			t.Errorf("%s: %d decisions were recorded", tc.name, n)
		}
		for _, secret := range []string{"very-secret-key-bytes", hex.EncodeToString([]byte("very-secret-key-bytes"))} {
			if strings.Contains(out, secret) || strings.Contains(errs, secret) {
				t.Errorf("%s: the private key block's content is in the output", tc.name)
			}
		}
	}
	// A cert expiring in exactly the minimum is accepted; one nanosecond less is not.
	atT0(t, time.Hour-defaultCertMinLeft)
	if code, _, errs := f.run_("", f.certArgs("ops", p, "-yes")...); code != 0 {
		t.Errorf("exactly the minimum left: %d %s", code, errs)
	}
}

func set(flagName, value string) func([]string) []string {
	return func(args []string) []string {
		out := append([]string(nil), args...)
		for i := range out {
			if out[i] == flagName {
				out[i+1] = value
			}
		}
		return out
	}
}

func repeatDER(der []byte, n int) [][]byte {
	var out [][]byte
	for range n {
		out = append(out, der)
	}
	return out
}

// An approver the journal says is certificate-bound cannot decide without -cert, and a bare-key approver
// of such a gate cannot decide with one: both would be recorded and never counted.
func TestCert_TheGateSaysWhoIsCertificateBound(t *testing.T) {
	atT0(t, time.Minute)
	f, p := newCertFixture(t)
	code, _, errs := f.run_("", f.asUnverified("ops", "-yes")...)
	if code != 1 || !strings.Contains(errs, "certificate-bound approver of this gate") || !strings.Contains(errs, "-cert") {
		t.Fatalf("a bound approver without -cert: %d %s", code, errs)
	}
	q := newPKI(t, f, "finance")
	code, _, errs = f.run_("", f.certArgs("finance", q, "-yes")...)
	if code != 1 || !strings.Contains(errs, "bare-key approver of this gate") {
		t.Fatalf("a bare approver with -cert: %d %s", code, errs)
	}
	if n := len(f.decisions()); n != 0 {
		t.Fatalf("%d decisions", n)
	}
	// finance still decides with a bare key, and ops with the certificate.
	if code, _, errs := f.run_("", f.asUnverified("finance", "-yes")...); code != 0 {
		t.Fatalf("finance: %d %s", code, errs)
	}
	if code, _, errs := f.run_("", f.certArgs("ops", p, "-yes")...); code != 0 {
		t.Fatalf("ops: %d %s", code, errs)
	}
}

func TestCert_UsageErrors(t *testing.T) {
	f, p := newCertFixture(t)
	base := []string{"-db", f.db, "-color", "never", "-run", f.run, "-call", f.call, "-as", "ops", "-key", f.keys["ops"]}
	for name, args := range map[string][]string{
		"-cert needs a policy or -unverified": append(append([]string{}, base...), "-cert", p.chain),
		"-cert with both":                     append(append([]string{}, base...), "-cert", p.chain, "-identity-policy", p.policy, "-unverified"),
		"-identity-policy without -cert":      append(append([]string{}, base...), "-identity-policy", p.policy, "-unverified"),
		"-cert with -list":                    {"-db", f.db, "-list", "-cert", p.chain},
		"-cert without -as":                   {"-db", f.db, "-run", f.run, "-call", f.call, "-cert", p.chain, "-identity-policy", p.policy},
		"-cert with -verify-receipt":          {"-verify-receipt", "x", "-approver-keys", "y", "-cert", p.chain},
		"negative -cert-min-left":             append(append([]string{}, base...), "-cert", p.chain, "-identity-policy", p.policy, "-cert-min-left", "-1s"),
		"-cert with -approver-keys on -to":    {"-to", "https://x.example", "-run", f.run, "-call", f.call, "-as", "ops", "-key", f.keys["ops"], "-cert", p.chain, "-identity-policy", p.policy, "-approver-keys", f.keysJSON},
	} {
		var out, errb bytes.Buffer
		if code := Main(args, strings.NewReader(""), &out, &errb, stores); code != 2 {
			t.Errorf("%s: code %d\n%s", name, code, errb.String())
		}
	}
}

// Without a policy (-unverified) the chain is not checked and the output says so; the key is still
// checked against the leaf and the validity window.
func TestCert_UnverifiedSignsWithTheWarning(t *testing.T) {
	atT0(t, time.Minute)
	f, p := newCertFixture(t)
	args := []string{"-run", f.run, "-call", f.call, "-as", "ops", "-key", f.keys["ops"], "-cert", p.chain, "-unverified", "-yes"}
	code, out, errs := f.run_("", args...)
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	contains(t, "output", out, "UNVERIFIED", "the chain is not checked against a policy")
	if len(f.decisions()) != 1 {
		t.Fatal("not recorded")
	}
	// A wrong key is still refused.
	g, q := newCertFixture(t)
	bad := []string{"-run", g.run, "-call", g.call, "-as", "ops", "-key", g.keys["finance"], "-cert", q.chain, "-unverified", "-yes"}
	if code, _, errs := g.run_("", bad...); code != 1 || !strings.Contains(errs, "is not the key of the certificate's leaf") {
		t.Fatalf("%d %s", code, errs)
	}
}

// A rejection is a certificate-bound decision too.
func TestCert_RejectIsSignedWithTheChain(t *testing.T) {
	atT0(t, time.Minute)
	f, p := newCertFixture(t)
	if code, out, errs := f.run_("", f.certArgs("ops", p, "-yes", "-reject")...); code != 0 {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	ds := f.decisions()
	if len(ds) != 1 || ds[0].Approved || len(ds[0].ApproverCert()) == 0 {
		t.Fatalf("%+v", ds)
	}
}

// readChain: a certificate file is read for certificates only, and a chain file with a blank line or
// comment text between blocks (PEM allows text outside the blocks, but this reader does not).
func TestReadChain(t *testing.T) {
	root := testca.NewRoot(t, "root", testca.Opts{})
	leaf := root.NewLeaf(t, testca.Opts{})
	dir := t.TempDir()
	write := func(name string, parts ...[]byte) string {
		var b bytes.Buffer
		for _, p := range parts {
			b.Write(p)
		}
		path := filepath.Join(dir, name)
		os.WriteFile(path, b.Bytes(), 0o600)
		return path
	}
	blk := func(d []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d}) }
	cc, err := readChain(write("ok.pem", blk(leaf.DER), []byte("\n\n"), blk(root.DER)))
	if err != nil || len(cc.der) != 2 || !cc.leaf.Equal(leaf.Cert) {
		t.Fatalf("%v", err)
	}
	// Exactly the limit is accepted.
	if _, err := readChain(write("limit.pem", blk(leaf.DER), blk(root.DER), blk(root.DER), blk(root.DER), blk(root.DER), blk(root.DER), blk(root.DER), blk(root.DER))); err != nil {
		t.Fatalf("eight certificates: %v", err)
	}
	if _, err := readChain(write("over.pem", blk(leaf.DER), blk(root.DER), blk(root.DER), blk(root.DER), blk(root.DER), blk(root.DER), blk(root.DER), blk(root.DER), blk(root.DER))); err == nil {
		t.Fatal("nine certificates")
	}
	for name, kind := range map[string]string{"EC PRIVATE KEY": "EC PRIVATE KEY", "PRIVATE KEY": "PRIVATE KEY", "OPENSSH": "OPENSSH PRIVATE KEY", "PUBLIC KEY": "PUBLIC KEY"} {
		_, err := readChain(write("k.pem", blk(leaf.DER), pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: []byte("secret-material")})))
		if err == nil || strings.Contains(err.Error(), "secret-material") {
			t.Errorf("%s: %v", name, err)
		}
	}
	_ = x509.Certificate{}
}

// The key is opened (for an HSM, a login; for a KMS, a billed call) only after the certificate file, the
// policy and the validity passed: a bad -cert opens nothing.
func TestCert_ABadCertificateOpensNoKey(t *testing.T) {
	atT0(t, time.Minute)
	f, p := newCertFixture(t)
	opened := 0
	src := FileKeys()
	load := src.Load
	src.Load = func(ref string) (audit.Signer, error) { opened++; return load(ref) }
	run := func(args ...string) int {
		var out, errb bytes.Buffer
		return MainWith(append([]string{"-db", f.db, "-color", "never"}, args...), strings.NewReader(""), &out, &errb, stores, src)
	}
	if code := run(set("-cert", filepath.Join(p.dir, "absent.pem"))(f.certArgs("ops", p, "-yes"))...); code != 1 || opened != 0 {
		t.Fatalf("a missing chain file: code %d, key opened %d times", code, opened)
	}
	atT0(t, 3*time.Hour)
	if code := run(f.certArgs("ops", p, "-yes")...); code != 1 || opened != 0 {
		t.Fatalf("an expired certificate: code %d, key opened %d times", code, opened)
	}
	atT0(t, time.Minute)
	// A wrong key is found after it is opened (the key's public half is what is compared), and signs nothing.
	bad := set("-key", f.keys["finance"])(f.certArgs("ops", p, "-yes"))
	if code := run(bad...); code != 1 || opened != 1 || len(f.decisions()) != 0 {
		t.Fatalf("a wrong key: code %d, opened %d, decisions %d", code, opened, len(f.decisions()))
	}
}

// ---- -to: the approvals intake ----

// intakeStub is the application's verifier of a certificate-bound approver as the approvals intake uses it
// (agent.CertIntakeVerifier, with the gate's identity rule): it accepts a decision whose signature verifies
// over the v4 bytes under the leaf the chain carries, and says the revocation status is unavailable while
// down is set.
type intakeStub struct {
	boundStub
	ident string
	down  *atomic.Bool
	seen  atomic.Int32
}

func (s *intakeStub) HasIdentity(string) bool                   { return true }
func (s *intakeStub) Identifier(string) (string, bool)          { return s.ident, true }
func (s *intakeStub) RequesterIdentifier(string) (string, bool) { return "", false }
func (s *intakeStub) IdentityMappingConfigured() bool           { return false }
func (s *intakeStub) RosterDigest() string                      { return "roster" }
func (s *intakeStub) VerifyCertDecision(cd agent.CertDecision) agent.CertCheck {
	return s.VerifyCertIntake(cd)
}
func (s *intakeStub) VerifyCertIntake(cd agent.CertDecision) agent.CertCheck {
	s.seen.Add(1)
	if s.down != nil && s.down.Load() {
		return agent.CertCheck{Reason: agent.ReasonStatusUnknown, Detail: "no CRL"}
	}
	r := cd.Record
	chain := r.ApproverCert()
	if len(chain) == 0 {
		return agent.CertCheck{Reason: agent.ReasonNoCert}
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return agent.CertCheck{Reason: agent.ReasonCertChain}
	}
	alg, pub, err := audit.CertDecisionKey(leaf)
	if err != nil {
		return agent.CertCheck{Reason: agent.ReasonCertUsage}
	}
	v, err := audit.NewVerifier(alg, pub)
	if err != nil {
		return agent.CertCheck{Reason: agent.ReasonCertUsage}
	}
	at, err := agent.ParseTimestamp(r.DecidedAt())
	if err != nil || r.ApproverAlg() != alg || !v.Verify(agent.ApprovalDecisionBytesCert(cd.Subject, r.Approver(), r.Approved, at, agent.CertLeafHash(chain[0])), r.Signature()) {
		return agent.CertCheck{Reason: agent.ReasonBadSig}
	}
	return agent.CertCheck{Identity: s.ident, Anchored: at}
}

// certIntakeFixture is a fixture whose "ops" is certificate-bound on the application's side, resolved by
// an intakeStub at the intake.
func certIntakeFixture(t *testing.T, down *atomic.Bool) (*fixture, *pki, *intakeStub) {
	t.Helper()
	f, p := newCertFixture(t)
	st := &intakeStub{boundStub: boundStub{id: "ops"}, ident: testca.Person(1), down: down}
	f.intakeBound = map[string]*intakeStub{"ops": st}
	return f, p, st
}

func TestCertRemote_EndToEnd(t *testing.T) {
	atT0(t, time.Minute)
	f, p, st := certIntakeFixture(t, nil)
	srv := f.intake(nil)
	rcpt := filepath.Join(f.dir, "r.json")
	args := []string{"-to", srv.URL, "-run", f.run, "-call", f.call, "-as", "ops", "-key", f.keys["ops"], "-cert", p.chain, "-identity-policy", p.policy, "-yes", "-receipt", rcpt}
	code, out, errs := f.runTo("", args...)
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	contains(t, "output", out, "leaf SHA-256", "accepted for ops by the policy now", "signed and recorded by the application", "receipt ")
	if st.seen.Load() == 0 || len(f.decisions()) != 1 || len(f.decisions()[0].ApproverCert()) != len(p.leaf.Chain()) {
		t.Fatalf("seen %d, %d decisions", st.seen.Load(), len(f.decisions()))
	}
	b, err := os.ReadFile(rcpt)
	if err != nil {
		t.Fatal(err)
	}
	var rc audit.DecisionReceipt
	if err := json.Unmarshal(b, &rc); err != nil || len(rc.Cert) != len(p.leaf.Chain()) {
		t.Fatalf("receipt: %v", err)
	}
}

// A 503 from the intake (status the application cannot judge yet) records nothing and says to retry; the
// same command succeeds once status is available.
func TestCertRemote_StatusUnavailableThenRetry(t *testing.T) {
	atT0(t, time.Minute)
	var down atomic.Bool
	down.Store(true)
	f, p, _ := certIntakeFixture(t, &down)
	srv := f.intake(nil)
	args := []string{"-to", srv.URL, "-run", f.run, "-call", f.call, "-as", "ops", "-key", f.keys["ops"], "-cert", p.chain, "-identity-policy", p.policy, "-yes"}
	code, _, errs := f.runTo("", args...)
	if code != 1 || !strings.Contains(errs, "cannot judge your certificate's revocation status") || len(f.decisions()) != 0 {
		t.Fatalf("%d %s, %d decisions", code, errs, len(f.decisions()))
	}
	down.Store(false)
	if code, out, errs := f.runTo("", append(args, "-receipt", filepath.Join(f.dir, "r2.json"))...); code != 0 {
		t.Fatalf("retry: %d\n%s\n%s", code, out, errs)
	}
}

// The gate's identity rule decides who uses a certificate: a certificate-bound approver without -cert, a
// bare-key approver with one, and a certificate for another identity than the gate expects are all refused
// before anything is signed.
func TestCertRemote_TheIdentityRule(t *testing.T) {
	atT0(t, time.Minute)
	f, p, st := certIntakeFixture(t, nil)
	srv := f.intake(nil)
	code, _, errs := f.runTo("", "-to", srv.URL, "-run", f.run, "-call", f.call, "-as", "ops", "-key", f.keys["ops"], "-unverified", "-yes")
	if code != 1 || !strings.Contains(errs, "certificate-bound approver of this gate") {
		t.Fatalf("a bound approver without -cert: %d %s", code, errs)
	}
	q := newPKI(t, f, "finance")
	code, _, errs = f.runTo("", "-to", srv.URL, "-run", f.run, "-call", f.call, "-as", "finance", "-key", f.keys["finance"], "-cert", q.chain, "-identity-policy", q.policy, "-yes")
	if code != 1 || !strings.Contains(errs, "bare-key approver of this gate") {
		t.Fatalf("a bare approver with -cert: %d %s", code, errs)
	}
	st.ident = testca.Person(2) // the gate expects another person than our certificate carries
	code, _, errs = f.runTo("", "-to", srv.URL, "-run", f.run, "-call", f.call, "-as", "ops", "-key", f.keys["ops"], "-cert", p.chain, "-identity-policy", p.policy, "-yes")
	if code != 1 || !strings.Contains(errs, "carries the identity") || len(f.decisions()) != 0 {
		t.Fatalf("another identity: %d %s %d", code, errs, len(f.decisions()))
	}
}

// A receipt that does not carry the chain that was sent, or whose signature is not over the v4 bytes, is not
// written, though the decision is recorded.
func TestCertRemote_ABadReceiptIsNotWritten(t *testing.T) {
	atT0(t, time.Minute)
	f, p, _ := certIntakeFixture(t, nil)
	strip := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			body := rec.Body.Bytes()
			if r.Method == http.MethodPost {
				var m map[string]any
				if json.Unmarshal(body, &m) == nil {
					if rc, ok := m["receipt"].(map[string]any); ok {
						delete(rc, "cert")
						body, _ = json.Marshal(m)
					}
				}
			}
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			w.Write(body)
		})
	}
	srv := f.intake(strip)
	rcpt := filepath.Join(f.dir, "r.json")
	code, _, errs := f.runTo("", "-to", srv.URL, "-run", f.run, "-call", f.call, "-as", "ops", "-key", f.keys["ops"], "-cert", p.chain, "-identity-policy", p.policy, "-yes", "-receipt", rcpt)
	if code != 1 || !strings.Contains(errs, "does not verify") {
		t.Fatalf("%d %s", code, errs)
	}
	if _, err := os.Stat(rcpt); err == nil {
		t.Fatal("a receipt that fails was written")
	}
	if len(f.decisions()) != 1 {
		t.Fatalf("%d decisions", len(f.decisions()))
	}
}
