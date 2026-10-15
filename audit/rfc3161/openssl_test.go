package rfc3161

import (
	"bytes"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requireOpenSSL returns the path of an OpenSSL (not LibreSSL) with the `ts` command. When it is
// missing the differential tests skip locally, but fail in CI on Linux, where the runner image has
// OpenSSL: a skip there would let the differential check silently stop running.
func requireOpenSSL(t *testing.T) string {
	t.Helper()
	candidates := []string{"openssl"}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, "/opt/homebrew/opt/openssl@3/bin/openssl", "/opt/homebrew/opt/openssl@4/bin/openssl", "/usr/local/opt/openssl@3/bin/openssl")
	}
	for _, c := range candidates {
		p, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		out, err := exec.Command(p, "version").Output()
		if err != nil || !strings.HasPrefix(string(out), "OpenSSL") {
			continue
		}
		if err := exec.Command(p, "ts", "-help").Run(); err != nil && !strings.Contains(err.Error(), "exit status 1") {
			continue
		}
		return p
	}
	if os.Getenv("CI") != "" && runtime.GOOS == "linux" {
		t.Fatal("openssl with the ts command is required in CI on Linux (the differential test must not skip there)")
	}
	t.Skip("openssl with the ts command is not available")
	return ""
}

type osslCase struct {
	openssl, root, query, tokFile string
	at                            time.Time
}

// accepts runs `openssl ts -verify` on the token bytes.
func (c osslCase) accepts(tok []byte) (bool, string) {
	if err := os.WriteFile(c.tokFile, tok, 0o600); err != nil {
		return false, err.Error()
	}
	cmd := exec.Command(c.openssl, "ts", "-verify", "-in", c.tokFile, "-token_in", "-queryfile", c.query,
		"-CAfile", c.root, "-attime", strconv.FormatInt(c.at.Unix(), 10))
	out, err := cmd.CombinedOutput()
	return err == nil && bytes.Contains(out, []byte("Verification: OK")), string(out)
}

func newOsslCase(t *testing.T, ossl string, v vector) osslCase {
	t.Helper()
	dir := t.TempDir()
	return osslCase{openssl: ossl, tokFile: filepath.Join(dir, "tok.der"),
		root:  filepath.Join("testdata", v.dir, v.rootFile),
		query: filepath.Join("testdata", v.dir, v.reqFile), at: v.genTime}
}

// certRanges returns the byte ranges of the certificates embedded in a token other than the first
// one equal to the signer certificate (a token can carry duplicates, as OpenSSL's do).
func certRanges(t *testing.T, tok []byte, signer *x509.Certificate) (other [][2]int) {
	used := false
	t.Helper()
	ci, _, r := parseNode(tok)
	if r != nil {
		t.Fatal(r)
	}
	cs, _ := kids(ci)
	sdn, _ := only(cs[1], "content")
	sd, _ := kids(sdn)
	for _, n := range sd {
		if !isCtx(n, 0, true) {
			continue
		}
		certs, _ := kids(n)
		for _, c := range certs {
			start := bytes.Index(tok, c.FullBytes)
			if !used && bytes.Equal(c.FullBytes, signer.Raw) {
				used = true
				continue
			}
			other = append(other, [2]int{start, start + len(c.FullBytes)})
		}
	}
	return other
}

func inRanges(pos int, rs [][2]int) bool {
	for _, r := range rs {
		if pos >= r[0] && pos < r[1] {
			return true
		}
	}
	return false
}

func sameChain(a, b []*x509.Certificate) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

// The checked-in vectors: OpenSSL accepts all of them; the Go verifier accepts all but the one
// outside its profile.
func TestDifferentialRealVectors(t *testing.T) {
	ossl := requireOpenSSL(t)
	all := append([]vector(nil), realVectors...)
	all = append(all, v1Vectors...)
	for _, v := range all {
		t.Run(v.dir+"/"+v.tokFile, func(t *testing.T) {
			c := newOsslCase(t, ossl, v)
			tok := readTestdata(t, v.dir, v.tokFile)
			ok, out := c.accepts(tok)
			if !ok {
				t.Fatalf("openssl rejects a stored vector: %s", out)
			}
			pol := v.policyFor(t)
			_, err := Verify(tok, v.expected(t), pol)
			if _, v1 := refusedVectors[v.dir]; v1 {
				// OpenSSL accepts a SHA-1-only token; the profile refuses it unless the TSA key is allowed
				var r *Refusal
				if !asRefusal(err, &r) || r.Reason != ReasonESSV1Only {
					t.Fatalf("%s: err = %v, want signing-certificate-v1-only", v.dir, err)
				}
				pol.AllowESSV1 = []*x509.Certificate{v.signerCert(t)}
				if res, err := Verify(tok, v.expected(t), pol); err != nil || !res.ESSV1 {
					t.Fatalf("%s with the opt-in: %v", v.dir, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("openssl accepts, the Go verifier refuses: %v", err)
			}
		})
	}
}

// Single-byte mutations of real tokens: the Go verifier must never accept what OpenSSL rejects.
// Where OpenSSL accepts and Go refuses, the Go reason must be one of those outside the profile or
// the mutation must have hit a field Go reads and OpenSSL does not (counted and listed).
func TestDifferentialMutatedTokens(t *testing.T) {
	ossl := requireOpenSSL(t)
	for _, v := range []struct {
		vec    vector
		stride int
	}{{realVectors[0], 3}, {realVectors[2], 41}, {realVectors[3], 97}} {
		v := v
		t.Run(v.vec.tokFile+"/"+v.vec.dir, func(t *testing.T) {
			c := newOsslCase(t, ossl, v.vec)
			tok := readTestdata(t, v.vec.dir, v.vec.tokFile)
			exp, pol := v.vec.expected(t), v.vec.policyFor(t)
			var goAccepts, bothAccept, opensslOnly, junkCert int
			opensslOnlyReasons := map[Reason]int{}
			base, err := Verify(tok, exp, pol)
			if err != nil {
				t.Fatal(err)
			}
			for pos := 0; pos < len(tok); pos += v.stride {
				m := append([]byte(nil), tok...)
				m[pos] ^= 0x01
				res, goErr := Verify(m, exp, pol)
				if goErr == nil {
					goAccepts++
					if ok, out := c.accepts(m); !ok {
						// Known, benign difference: a corrupted copy of a certificate that is not the
						// signer's (an untrusted chain element) fails OpenSSL's chain building, while
						// the Go verifier takes its path from the auditor's anchors. Allowed only
						// when the flip is inside such a certificate and the trust path is unchanged.
						if inRanges(pos, certRanges(t, m, res.Signer)) && sameChain(res.Chain, base.Chain) {
							junkCert++
							continue
						}
						t.Fatalf("byte %d flipped: Go accepts, openssl rejects: %s", pos, out)
					}
					bothAccept++
					continue
				}
				// Go refuses: only spend an openssl run where the reason could be a divergence
				var r *Refusal
				asRefusal(goErr, &r)
				if r != nil && !r.Reason.OutsideProfile() && (r.Reason == ReasonBadSignature || r.Reason == ReasonMessageDigest) {
					continue // a flipped signed byte: OpenSSL rejects these too (covered by the Go-accepts rule)
				}
				if ok, _ := c.accepts(m); ok {
					opensslOnly++
					if r != nil {
						opensslOnlyReasons[r.Reason]++
					}
					if r == nil || !r.Reason.OutsideProfile() {
						t.Errorf("byte %d flipped: openssl accepts, Go refuses with %v (a within-profile reason)", pos, goErr)
					}
				}
			}
			t.Logf("%d positions: Go accepts %d (openssl agrees on %d, %d flips in an unused certificate), openssl-only accepts %d %v", len(tok)/v.stride+1, goAccepts, bothAccept, junkCert, opensslOnly, opensslOnlyReasons)
		})
	}
}

// Tokens written by the test builder, which share no code with the verifier: OpenSSL must accept
// the valid ones, and must not reject any token the Go verifier accepts.
func TestDifferentialBuiltTokens(t *testing.T) {
	ossl := requireOpenSSL(t)
	for _, kind := range []kitOpts{{tsaKey: kEC256}, {tsaKey: kEC384}, {tsaKey: kRSA3072}, {tsaKey: kRSA3072, rootKey: kRSA3072}} {
		kind := kind
		t.Run(fmt.Sprintf("%s-%s-ski%v", kind.tsaKey, kind.rootKey, kind.ski), func(t *testing.T) {
			f := newFixture(t)
			f.kit = newKit(t, kind)
			f.spec.kit = f.kit
			dir := t.TempDir()
			rootPEM := filepath.Join(dir, "root.pem")
			if err := os.WriteFile(rootPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.kit.root.Raw}), 0o600); err != nil {
				t.Fatal(err)
			}
			query := filepath.Join(dir, "q.tsq")
			hexd := fmt.Sprintf("%x", f.digest)
			if out, err := exec.Command(ossl, "ts", "-query", "-digest", hexd, "-sha256", "-no_nonce", "-cert", "-out", query).CombinedOutput(); err != nil {
				t.Fatalf("openssl ts -query: %v: %s", err, out)
			}
			c := osslCase{openssl: ossl, root: rootPEM, query: query, tokFile: filepath.Join(dir, "t.der"), at: f.gen}
			tok := f.spec.build(t)
			mustAccept(t, tok, f.expected(), f.policy())
			if ok, out := c.accepts(tok); !ok {
				t.Fatalf("openssl rejects a valid built token: %s", out)
			}
			// mutated: whatever Go accepts, OpenSSL accepts
			exp, pol := f.expected(), f.policy()
			base := mustAccept(t, tok, exp, pol)
			for pos := 0; pos < len(tok); pos += 11 {
				m := append([]byte(nil), tok...)
				m[pos] ^= 0x80
				if res, err := Verify(m, exp, pol); err == nil {
					if ok, out := c.accepts(m); !ok && !(inRanges(pos, certRanges(t, m, res.Signer)) && sameChain(res.Chain, base.Chain)) {
						t.Fatalf("byte %d: Go accepts, openssl rejects: %s", pos, out)
					}
				}
			}
		})
	}
}

// osslFor prepares OpenSSL to verify tokens of one fixture's kit.
func osslFor(t *testing.T, ossl string, f *fixture) osslCase {
	t.Helper()
	dir := t.TempDir()
	rootPEM := filepath.Join(dir, "root.pem")
	if err := os.WriteFile(rootPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.kit.root.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	query := filepath.Join(dir, "q.tsq")
	if out, err := exec.Command(ossl, "ts", "-query", "-digest", fmt.Sprintf("%x", f.digest), "-sha256", "-no_nonce", "-cert", "-out", query).CombinedOutput(); err != nil {
		t.Fatalf("openssl ts -query: %v: %s", err, out)
	}
	return osslCase{openssl: ossl, root: rootPEM, query: query, tokFile: filepath.Join(dir, "t.der"), at: f.gen}
}

// opensslAccepts lists the negative cases that OpenSSL accepts although the Go verifier refuses
// them: the intentional strictness of the profile (DER forms of defaults, canonical order, hashes
// OpenSSL tolerates, claims OpenSSL does not check). Every other negative case must be refused by
// OpenSSL too, and the list may not hold a case OpenSSL now rejects.
var opensslAccepts = map[string]string{
	"ESS hash is SHA-1":                             "OpenSSL has no algorithm allowlist; the profile never accepts SHA-1, MD5 or SHA-224",
	"ESS hash is SHA-256 spelled out (DER default)": "a DER form of a default value or a BER-style encoding that OpenSSL tolerates",
	"TSA certificate is a CA":                       "OpenSSL does not reject the CA flag on the signer (the assessment expected it to; measured otherwise)",
	"accuracy above a day by its millis":            "OpenSSL does not bound or range-check the accuracy",
	"accuracy beyond a day":                         "OpenSSL does not bound or range-check the accuracy",
	"accuracy millis zero":                          "OpenSSL does not bound or range-check the accuracy",
	"accuracy seconds that overflow to zero":        "OpenSSL does not bound or range-check the accuracy",
	"contentType attribute names another type":      "OpenSSL does not compare the contentType attribute with the content type",
	"critical tstinfo extension":                    "OpenSSL does not refuse critical TSTInfo extensions",
	"duplicate contentType":                         "OpenSSL reads one instance of a repeated signed attribute; the profile requires exactly one",
	"duplicate messageDigest":                       "OpenSSL reads one instance of a repeated signed attribute; the profile requires exactly one",
	"duplicate signingCertificateV2":                "OpenSSL reads one instance of a repeated signed attribute; the profile requires exactly one",
	"ecdsa with SHA-1":                              "OpenSSL has no algorithm allowlist; the profile never accepts SHA-1, MD5 or SHA-224",
	"ecdsa with SHA-224":                            "OpenSSL has no algorithm allowlist; the profile never accepts SHA-1, MD5 or SHA-224",
	"explicit false critical flag (not DER)":        "a DER form of a default value or a BER-style encoding that OpenSSL tolerates",
	"genTime at hour 24":                            "OpenSSL does not re-validate the genTime text strictly",
	"genTime in month 13":                           "OpenSSL does not re-validate the genTime text strictly",
	"genTime on a day that does not exist":          "OpenSSL does not re-validate the genTime text strictly",
	"genTime with a ten-digit fraction":             "OpenSSL does not re-validate the genTime text strictly",
	"genTime with impossible date":                  "OpenSSL does not re-validate the genTime text strictly",
	"genTime with ten fraction digits":              "OpenSSL does not re-validate the genTime text strictly",
	"genTime with trailing zero fraction":           "OpenSSL does not re-validate the genTime text strictly",
	"genTime with zone offset":                      "OpenSSL does not re-validate the genTime text strictly",
	"genTime without Z":                             "OpenSSL does not re-validate the genTime text strictly",
	"messageDigest with two values":                 "OpenSSL reads the first value of a signed attribute",
	"missing contentType":                           "OpenSSL does not require the contentType signed attribute",
	"negative accuracy":                             "OpenSSL does not bound or range-check the accuracy",
	"ordering FALSE spelled out":                    "a DER form of a default value or a BER-style encoding that OpenSSL tolerates",
	"ordering with a non-DER true":                  "a DER form of a default value or a BER-style encoding that OpenSSL tolerates",
	"p-521 signer":                                  "OpenSSL has no key-size or curve allowlist; the profile does",
	"rsa 1024 signer":                               "OpenSSL has no key-size or curve allowlist; the profile does",
	"rsa 2048 CA":                                   "OpenSSL has no key-size or curve allowlist; the profile does",
	"rsa 2048 signer":                               "OpenSSL has no key-size or curve allowlist; the profile does",
	"rsa signature algorithm on an EC key":          "OpenSSL takes the key type from the certificate and has no algorithm allowlist",
	"rsa with MD5":                                  "OpenSSL has no algorithm allowlist; the profile never accepts SHA-1, MD5 or SHA-224",
	"rsa with SHA-1":                                "OpenSSL has no algorithm allowlist; the profile never accepts SHA-1, MD5 or SHA-224",
	"rsa with SHA-224":                              "OpenSSL has no algorithm allowlist; the profile never accepts SHA-1, MD5 or SHA-224",
	"signed attributes not in DER order":            "OpenSSL verifies over the received bytes without requiring DER SET OF order",
	"signed data version 1":                         "OpenSSL does not check the SignedData version",
	"signer version 2":                              "OpenSSL does not check the SignerInfo version against the identifier form",
	"signer version 3 with issuer and serial":       "OpenSSL does not check the SignerInfo version against the identifier form",
	"signingCertificate (SHA-1) only":               "OpenSSL accepts the SHA-1 signingCertificate attribute alone; the profile never trusts SHA-1",
	"signingCertificate twice":                      "OpenSSL reads one instance of a repeated signed attribute; the profile requires exactly one",
	"tstinfo trailing bytes":                        "OpenSSL ignores bytes after the TSTInfo",
	"two digest algorithms":                         "OpenSSL does not require exactly one digestAlgorithms entry",
	"unknown signature algorithm":                   "OpenSSL takes the key type from the certificate and has no algorithm allowlist",
}

// TestDifferentialNegativeVectors replays every negative vector against OpenSSL. Each token is
// re-signed with a structural fault, so a refusal exercises parsing, not a broken signature. The
// claim is bounded to these classes: where OpenSSL accepts and Go refuses, the case is listed and
// justified; there is no case where Go accepts and OpenSSL refuses (the accepted variants below).
func TestDifferentialNegativeVectors(t *testing.T) {
	ossl := requireOpenSSL(t)
	var groups = [][]negCase{structureCases, attrCases, algCases, imprintCases, pkiCases}
	seen := map[string]bool{}
	for _, g := range groups {
		for _, c := range g {
			c := c
			t.Run(c.name, func(t *testing.T) {
				f, tok := c.build(t)
				if _, err := Verify(tok, f.expected(), f.policy()); err == nil {
					t.Fatal("the Go verifier accepts a negative vector")
				}
				ok, out := osslFor(t, ossl, f).accepts(tok)
				_, listed := opensslAccepts[c.name]
				switch {
				case !opensslMeasured(ossl):
					// The list was measured with OpenSSL 4; another version may differ (3.x accepts a
					// genTime without seconds, for example). Informational only; the rule that matters,
					// that nothing the Go verifier accepts is rejected by OpenSSL, is enforced elsewhere.
					t.Logf("openssl accepts=%v, listed=%v", ok, listed)
				case ok && !listed:
					t.Errorf("OpenSSL accepts %q, Go refuses (%s): add it to opensslAccepts with the reason, or fix the verifier", c.name, c.want)
				case !ok && listed:
					t.Errorf("%q is listed in opensslAccepts but OpenSSL rejects it: %s", c.name, firstLine(out))
				}
				seen[c.name] = true
			})
		}
	}
	for name := range opensslAccepts {
		if !seen[name] {
			t.Errorf("opensslAccepts names %q, which is not a negative vector", name)
		}
	}
}

func firstLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for _, l := range lines {
		if !strings.HasPrefix(l, "Using configuration") {
			return l
		}
	}
	return ""
}

// The accepted variants (and the PKI-assessment ones) are accepted by OpenSSL as well, except the
// forms its PKCS#7 API cannot read (subject-key-id signer identifiers, Ed25519).
func TestDifferentialAcceptedVariants(t *testing.T) {
	ossl := requireOpenSSL(t)
	now := time.Now()
	cases := []struct {
		name string
		kit  kitOpts
		mut  func(*tokSpec)
	}{
		{"p384", kitOpts{tsaKey: kEC384}, nil},
		{"no issuerSerial", kitOpts{}, nil},
		{"unsigned attrs", kitOpts{}, func(s *tokSpec) { s.unsigned = ctx(1, seq(oidDER(asn1.ObjectIdentifier{1, 2, 3}), setOf(intDER(1)))) }},
		{"v1 and v2", kitOpts{}, func(s *tokSpec) { s.attrHook = func(a [][]byte) [][]byte { return append(a, s.essV1()) } }},
		{"fraction genTime", kitOpts{}, func(s *tokSpec) { s.genTime = genTimeText(now.UTC())[:14] + ".25Z" }},
		{"tsa name = subject", kitOpts{}, func(s *tokSpec) { s.tsaRaw = ctx(0, ctx(4, s.kit.tsa.RawSubject)) }},
		{"nonRepudiation only", kitOpts{keyUsage: x509.KeyUsageContentCommitment}, nil},
		{"critical-free extension", kitOpts{}, func(s *tokSpec) {
			s.tstExt = ctx(1, seq(oidDER(asn1.ObjectIdentifier{1, 2, 3, 9}), octets([]byte{5, 0})))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.kit = newKit(t, c.kit)
			f.spec.kit = f.kit
			if c.name == "no issuerSerial" {
				f.spec.essNoIssuer = true
			}
			if c.mut != nil {
				c.mut(&f.spec)
			}
			tok := f.spec.build(t)
			mustAccept(t, tok, f.expected(), f.policy())
			if ok, out := osslFor(t, ossl, f).accepts(tok); !ok {
				t.Fatalf("Go accepts, OpenSSL rejects: %s", firstLine(out))
			}
		})
	}
}

// opensslMeasured reports whether ossl is the OpenSSL major version that opensslAccepts was measured with.
func opensslMeasured(ossl string) bool {
	out, err := exec.Command(ossl, "version").Output()
	return err == nil && strings.HasPrefix(string(out), "OpenSSL 4.")
}
