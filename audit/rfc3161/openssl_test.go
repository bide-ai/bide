package rfc3161

import (
	"bytes"
	"crypto/x509"
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
	all = append(all, vector{dir: "freetsa", tokFile: "token.tok", respFile: "response.tsr", reqFile: "request.tsq", rootFile: "root.pem",
		genTime: time.Date(2026, 10, 15, 15, 59, 57, 0, time.UTC)})
	for _, v := range all {
		t.Run(v.dir+"/"+v.tokFile, func(t *testing.T) {
			c := newOsslCase(t, ossl, v)
			tok := readTestdata(t, v.dir, v.tokFile)
			ok, out := c.accepts(tok)
			if !ok {
				t.Fatalf("openssl rejects a stored vector: %s", out)
			}
			_, err := Verify(tok, v.expected(t), v.policyFor(t))
			if v.dir == "freetsa" {
				var r *Refusal
				if !asRefusal(err, &r) || !r.Reason.OutsideProfile() {
					t.Fatalf("freetsa: err = %v, want an outside-profile refusal", err)
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
