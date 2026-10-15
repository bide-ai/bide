package rfc3161

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readTestdata(t testing.TB, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"testdata"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func poolFromPEM(t testing.TB, b []byte) *x509.CertPool {
	t.Helper()
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		t.Fatal("no certificate in PEM")
	}
	return p
}

// tokenOfResponse returns the timeStampToken inside a TimeStampResp: SEQUENCE { status, token }.
func tokenOfResponse(t testing.TB, resp []byte) []byte {
	t.Helper()
	var outer asn1.RawValue
	if rest, err := asn1.Unmarshal(resp, &outer); err != nil || len(rest) != 0 {
		t.Fatalf("response: %v", err)
	}
	cs, r := kids(outer)
	if r != nil || len(cs) != 2 {
		t.Fatalf("response has %d elements (%v)", len(cs), r)
	}
	return cs[1].FullBytes
}

// requestNonce returns the nonce of a TimeStampReq.
func requestNonce(t testing.TB, req []byte) *big.Int {
	t.Helper()
	var outer asn1.RawValue
	if _, err := asn1.Unmarshal(req, &outer); err != nil {
		t.Fatal(err)
	}
	cs, _ := kids(outer)
	for _, c := range cs[2:] {
		if isInt(c) {
			n, _ := bigOf(c, "nonce")
			return n
		}
	}
	t.Fatal("request has no nonce")
	return nil
}

func dataDigest(t testing.TB) []byte {
	d := sha256.Sum256(readTestdata(t, "openssl", "data.txt"))
	return d[:]
}

type vector struct {
	dir, tokFile, respFile, reqFile, rootFile string
	genTime                                   time.Time
	policy                                    string
	serial                                    string // hex
	signerCN                                  string
	accuracy                                  time.Duration
	rsa2048                                   bool
}

var realVectors = []vector{
	{dir: "openssl", tokFile: "token-ec256.tok", respFile: "resp-ec256.tsr", reqFile: "req-ec256.tsq", rootFile: "root-ec256.pem",
		genTime: time.Date(2026, 10, 15, 16, 8, 30, 0, time.UTC), policy: "1.2.3.4.1", serial: "02", signerCN: "bide test TSA ec256", accuracy: 1500 * time.Millisecond},
	{dir: "openssl", tokFile: "token-ec384.tok", respFile: "resp-ec384.tsr", reqFile: "req-ec384.tsq", rootFile: "root-ec384.pem",
		genTime: time.Date(2026, 10, 15, 16, 8, 31, 0, time.UTC), policy: "1.2.3.4.1", serial: "02", signerCN: "bide test TSA ec384", accuracy: 1500 * time.Millisecond},
	{dir: "openssl", tokFile: "token-rsa3072.tok", respFile: "resp-rsa3072.tsr", reqFile: "req-rsa3072.tsq", rootFile: "root-rsa3072.pem",
		genTime: time.Date(2026, 10, 15, 16, 8, 31, 0, time.UTC), policy: "1.2.3.4.1", serial: "02", signerCN: "bide test TSA rsa3072", accuracy: 1500 * time.Millisecond},
	{dir: "digicert", tokFile: "token.tok", respFile: "response.tsr", reqFile: "request.tsq", rootFile: "root.pem",
		genTime: time.Date(2026, 10, 15, 15, 59, 57, 0, time.UTC), policy: "2.16.840.1.114412.7.1", serial: "de23b3b402fe586d6396427664a53de1",
		signerCN: "DigiCert SHA256 RSA4096 Timestamp Responder 2026 1"},
}

func (v vector) policyFor(t testing.TB) Policy {
	return Policy{
		Roots:      poolFromPEM(t, readTestdata(t, v.dir, v.rootFile)),
		CertStatus: func(*x509.Certificate, []*x509.Certificate, time.Time) error { return nil },
	}
}

func (v vector) expected(t testing.TB) Expected {
	return Expected{Hash: crypto.SHA256, Digest: dataDigest(t), HeadTime: v.genTime.Add(-time.Minute),
		Nonce: requestNonce(t, readTestdata(t, v.dir, v.reqFile))}
}

func TestRealTSAVectors(t *testing.T) {
	for _, v := range realVectors {
		t.Run(v.dir+"/"+v.tokFile, func(t *testing.T) {
			tok := readTestdata(t, v.dir, v.tokFile)
			if !bytes.Equal(tok, tokenOfResponse(t, readTestdata(t, v.dir, v.respFile))) {
				t.Fatal("the stored token is not the response's token")
			}
			res := mustAccept(t, tok, v.expected(t), v.policyFor(t))
			if !res.GenTime.Equal(v.genTime) {
				t.Errorf("genTime %v, want %v", res.GenTime, v.genTime)
			}
			if !bytes.Equal(res.Imprint, dataDigest(t)) || res.Hash != crypto.SHA256 {
				t.Errorf("imprint %x", res.Imprint)
			}
			if res.PolicyOID != v.policy || hex.EncodeToString(res.Serial.Bytes()) != v.serial {
				t.Errorf("policy %s serial %x", res.PolicyOID, res.Serial.Bytes())
			}
			if res.Signer.Subject.CommonName != v.signerCN {
				t.Errorf("signer %q, want %q", res.Signer.Subject.CommonName, v.signerCN)
			}
			if res.Accuracy != v.accuracy {
				t.Errorf("accuracy %v, want %v", res.Accuracy, v.accuracy)
			}
			// the wrong nonce, a different digest and a later head all refuse
			e := v.expected(t)
			e.Nonce = big.NewInt(1)
			mustRefuse(t, tok, e, v.policyFor(t), ReasonNonceMismatch)
			e = v.expected(t)
			e.Digest = append([]byte{^e.Digest[0]}, e.Digest[1:]...)
			mustRefuse(t, tok, e, v.policyFor(t), ReasonImprintMismatch)
			e = v.expected(t)
			e.HeadTime = v.genTime.Add(time.Hour)
			mustRefuse(t, tok, e, v.policyFor(t), ReasonGenTimeBeforeHead)
			// without the TSA's root the token is refused
			p := v.policyFor(t)
			p.Roots = x509.NewCertPool()
			mustRefuse(t, tok, v.expected(t), p, ReasonUntrustedTSA)
		})
	}
}

// FreeTSA's token is valid RFC 3161 (OpenSSL verifies it) but identifies its signer by the SHA-1
// signingCertificate attribute only, which the profile never accepts.
func TestFreeTSATokenIsOutsideTheProfile(t *testing.T) {
	v := vector{dir: "freetsa", tokFile: "token.tok", respFile: "response.tsr", reqFile: "request.tsq", rootFile: "root.pem",
		genTime: time.Date(2026, 10, 15, 15, 59, 57, 0, time.UTC)}
	tok := readTestdata(t, v.dir, v.tokFile)
	if !bytes.Equal(tok, tokenOfResponse(t, readTestdata(t, v.dir, v.respFile))) {
		t.Fatal("the stored token is not the response's token")
	}
	e := v.expected(t)
	pol := v.policyFor(t)
	mustRefuse(t, tok, e, pol, ReasonESSV1Only)
	if !ReasonESSV1Only.OutsideProfile() {
		t.Error("v1-only must be classed as outside the profile")
	}
	// the certificate is still the one the real chain names: the refusal is about the profile
	blk, _ := pem.Decode(readTestdata(t, "freetsa", "tsa.pem"))
	if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
		t.Fatal(err)
	}
}
