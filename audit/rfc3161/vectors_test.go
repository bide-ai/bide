package rfc3161

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
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
		genTime: time.Date(2026, 10, 15, 16, 35, 27, 0, time.UTC), policy: "1.2.3.4.1", serial: "02", signerCN: "bide test TSA ec256", accuracy: time.Second},
	{dir: "openssl", tokFile: "token-ec384.tok", respFile: "resp-ec384.tsr", reqFile: "req-ec384.tsq", rootFile: "root-ec384.pem",
		genTime: time.Date(2026, 10, 15, 16, 35, 27, 0, time.UTC), policy: "1.2.3.4.1", serial: "02", signerCN: "bide test TSA ec384", accuracy: time.Second},
	{dir: "openssl", tokFile: "token-rsa3072.tok", respFile: "resp-rsa3072.tsr", reqFile: "req-rsa3072.tsq", rootFile: "root-rsa3072.pem",
		genTime: time.Date(2026, 10, 15, 16, 35, 28, 0, time.UTC), policy: "1.2.3.4.1", serial: "02", signerCN: "bide test TSA rsa3072", accuracy: time.Second},
	{dir: "digicert", tokFile: "token.tok", respFile: "response.tsr", reqFile: "request.tsq", rootFile: "root.pem",
		genTime: time.Date(2026, 10, 15, 15, 59, 57, 0, time.UTC), policy: "2.16.840.1.114412.7.1", serial: "de23b3b402fe586d6396427664a53de1",
		signerCN: "DigiCert SHA256 RSA4096 Timestamp Responder 2026 1"},
	{dir: "globalsign", tokFile: "token.tok", respFile: "response.tsr", reqFile: "request.tsq", rootFile: "root.pem",
		genTime: time.Date(2026, 10, 15, 16, 42, 14, 0, time.UTC), policy: "1.3.6.1.4.1.4146.2.3.2.2", serial: "5b87383c751168801d5a9254598cf1125a807e7a",
		signerCN: "Globalsign R45 TSA for CodeSign 202510", accuracy: time.Second},
	{dir: "quovadis", tokFile: "token.tok", respFile: "response.tsr", reqFile: "request.tsq", rootFile: "root.pem",
		genTime: time.Date(2026, 10, 15, 16, 42, 23, 0, time.UTC), policy: "1.3.6.1.4.1.8024.0.2000.6", serial: "699bc991854a11cc",
		signerCN: "tsaeu2025rsa2.digicert.eu", accuracy: 997 * time.Millisecond},
	{dir: "sslcom", tokFile: "token.tok", respFile: "response.tsr", reqFile: "request.tsq", rootFile: "root.pem",
		genTime: time.Date(2026, 10, 15, 16, 42, 16, 0, time.UTC), policy: "1.3.6.1.4.1.38064.1.3.6.1", serial: "66384f692e3cd3b0",
		signerCN: "SSL.com Timestamping Unit 2025 E1", accuracy: time.Second},
	{dir: "microsoft", tokFile: "token.tok", respFile: "response.tsr", reqFile: "request.tsq", rootFile: "root.pem",
		genTime: time.Date(2026, 10, 15, 16, 42, 17, 947_000_000, time.UTC), policy: "1.3.6.1.4.1.601.10.3.1", serial: "6ac8e5fcf1c6",
		signerCN: "Microsoft Public RSA Time Stamping Authority", accuracy: 500 * time.Millisecond},
	{dir: "swisssign", tokFile: "token.tok", respFile: "response.tsr", reqFile: "request.tsq", rootFile: "root.pem",
		genTime: time.Date(2026, 10, 15, 16, 42, 22, 0, time.UTC), policy: "2.16.756.1.89.1.1.3.5", serial: "6f08ffd74547c4af",
		signerCN: "SwissSign ZertES TSA UNIT A 2024 - 2", accuracy: 500 * time.Millisecond},
}

// refusedVectors are real tokens from public TSAs that the profile refuses, with the reason. They
// are valid RFC 3161 tokens; each names something the profile never accepts.
var refusedVectors = map[string]Reason{
	"freetsa": ReasonESSV1Only,
	"sectigo": ReasonESSV1Only,
	"entrust": ReasonESSV1Only,
	"apple":   ReasonWeakHash, // SHA-1 as the signer digest algorithm
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

// The refused real tokens are valid RFC 3161 tokens that the profile does not interpret: the SHA-1
// signingCertificate attribute alone, or SHA-1 as the signer digest algorithm.
func TestRefusedRealVectors(t *testing.T) {
	for dir, want := range refusedVectors {
		t.Run(dir, func(t *testing.T) {
			tok := readTestdata(t, dir, "token.tok")
			if !bytes.Equal(tok, tokenOfResponse(t, readTestdata(t, dir, "response.tsr"))) {
				t.Fatal("the stored token is not the response's token")
			}
			v := vector{dir: dir, reqFile: "request.tsq", genTime: time.Date(2026, 10, 15, 16, 0, 0, 0, time.UTC)}
			pol := Policy{Roots: x509.NewCertPool(), CertStatus: StatusUnknown}
			mustRefuse(t, tok, v.expected(t), pol, want)
			if !want.OutsideProfile() {
				t.Error("must be classed as outside the profile")
			}
		})
	}
}

// A token that states 1.5 s of accuracy is refused under the default cap of one second, accepted
// when the policy allows it, and then bounds the head comparison and Latest.
func TestWideAccuracyVector(t *testing.T) {
	v := vector{dir: "openssl", tokFile: "token-ec256wide.tok", respFile: "resp-ec256wide.tsr", reqFile: "req-ec256wide.tsq", rootFile: "root-ec256wide.pem",
		genTime: time.Date(2026, 10, 15, 16, 35, 28, 0, time.UTC)}
	tok := readTestdata(t, v.dir, v.tokFile)
	mustRefuse(t, tok, v.expected(t), v.policyFor(t), ReasonAccuracy)
	pol := v.policyFor(t)
	pol.MaxAccuracy = 1500 * time.Millisecond
	res := mustAccept(t, tok, v.expected(t), pol)
	if res.Accuracy != 1500*time.Millisecond || !res.Latest().Equal(v.genTime.Add(1500*time.Millisecond)) {
		t.Errorf("accuracy %v latest %v", res.Accuracy, res.Latest())
	}
	pol.MaxAccuracy = 1499 * time.Millisecond
	mustRefuse(t, tok, v.expected(t), pol, ReasonAccuracy)
}
