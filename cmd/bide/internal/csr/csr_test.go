package csr

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/audit"
)

// keySource is a stand-in for a key in an HSM or KMS: an audit.Signer whose Sign can be replaced.
type keySource struct {
	audit.Signer
	sign func(msg []byte) ([]byte, error)
}

func (k keySource) Sign(msg []byte) ([]byte, error) {
	if k.sign != nil {
		return k.sign(msg)
	}
	return k.Signer.Sign(msg)
}

func newSigner(t *testing.T, alg audit.Alg) audit.Signer {
	t.Helper()
	switch alg {
	case audit.AlgEd25519:
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return audit.Ed25519Signer{Priv: priv}
	case audit.AlgECDSAP256:
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return audit.ECDSAP256Signer{Priv: priv}
	}
	t.Fatalf("no test signer for %s", alg)
	return nil
}

var schemes = []audit.Alg{audit.AlgEd25519, audit.AlgECDSAP256}

func parse(t *testing.T, pemBytes []byte) *x509.CertificateRequest {
	t.Helper()
	blk, rest := pem.Decode(pemBytes)
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("not one CERTIFICATE REQUEST block: %q", pemBytes)
	}
	req, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// sourceSPKI is the SubjectPublicKeyInfo of the key source's own public key.
func sourceSPKI(t *testing.T, sg audit.Signer) []byte {
	t.Helper()
	pub, err := PublicKey(sg)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// TestBuildNoNames: the default request names nothing, is signed by the key (proof of possession) and
// carries the key source's public key, byte for byte, as its SubjectPublicKeyInfo.
func TestBuildNoNames(t *testing.T) {
	for _, alg := range schemes {
		t.Run(string(alg), func(t *testing.T) {
			sg := newSigner(t, alg)
			b, err := Build(sg, Names{})
			if err != nil {
				t.Fatal(err)
			}
			req := parse(t, b)
			if err := req.CheckSignature(); err != nil {
				t.Fatalf("proof of possession: %v", err)
			}
			if !bytes.Equal(req.RawSubjectPublicKeyInfo, sourceSPKI(t, sg)) {
				t.Fatal("the request's SubjectPublicKeyInfo is not the key source's")
			}
			if len(req.RawSubject) > 2 || len(req.Extensions) != 0 || len(req.Attributes) != 0 || req.EmailAddresses != nil || req.URIs != nil { //nolint:staticcheck
				t.Fatalf("the request names something: subject %x, %+v", req.RawSubject, req.Extensions)
			}
			if !bytes.HasSuffix(b, []byte("-----\n")) {
				t.Fatalf("no PEM footer and newline: %q", b)
			}
		})
	}
}

// TestBuildNames: a requested email, or URI, and key usages come back as the one SAN and the EKU,
// and the signature covers them (the request still verifies).
func TestBuildNames(t *testing.T) {
	clientAuth := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 2}
	emailProt := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 4}
	for _, alg := range schemes {
		t.Run(string(alg), func(t *testing.T) {
			sg := newSigner(t, alg)
			b, err := Build(sg, Names{Email: "ops@bank.example", EKU: []asn1.ObjectIdentifier{clientAuth, emailProt}})
			if err != nil {
				t.Fatal(err)
			}
			req := parse(t, b)
			if err := req.CheckSignature(); err != nil {
				t.Fatal(err)
			}
			if len(req.EmailAddresses) != 1 || req.EmailAddresses[0] != "ops@bank.example" || len(req.URIs) != 0 || len(req.DNSNames) != 0 {
				t.Fatalf("SAN = email %v uri %v dns %v", req.EmailAddresses, req.URIs, req.DNSNames)
			}
			var eku []asn1.ObjectIdentifier
			for _, e := range req.Extensions {
				if e.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 37}) {
					if _, err := asn1.Unmarshal(e.Value, &eku); err != nil {
						t.Fatal(err)
					}
				}
			}
			if len(eku) != 2 || !eku[0].Equal(clientAuth) || !eku[1].Equal(emailProt) {
				t.Fatalf("EKU = %v", eku)
			}
			if len(req.Extensions) != 2 {
				t.Fatalf("%d extensions, want the SAN and the EKU only", len(req.Extensions))
			}

			b, err = Build(sg, Names{URI: "https://idp.bank.example/people/42"})
			if err != nil {
				t.Fatal(err)
			}
			req = parse(t, b)
			if err := req.CheckSignature(); err != nil {
				t.Fatal(err)
			}
			if len(req.URIs) != 1 || req.URIs[0].String() != "https://idp.bank.example/people/42" || len(req.EmailAddresses) != 0 || len(req.Extensions) != 1 {
				t.Fatalf("SAN = uri %v email %v, %d extensions", req.URIs, req.EmailAddresses, len(req.Extensions))
			}
		})
	}
}

// TestBuildRefusesBadNames: a name the request cannot carry as one plain SAN is refused before the key
// is asked to sign anything.
func TestBuildRefusesBadNames(t *testing.T) {
	for name, n := range map[string]Names{
		"email and uri":       {Email: "a@b.example", URI: "https://b.example/a"},
		"display name":        {Email: "Ann <a@b.example>"},
		"no domain":           {Email: "a@"},
		"email with space":    {Email: "a b@b.example"},
		"relative uri":        {URI: "people/42"},
		"uri with space":      {URI: "https://b.example/a b"},
		"uri with fragment":   {URI: "https://b.example/a#x"},
		"uri without host":    {URI: "urn:x"},
		"empty eku":           {EKU: []asn1.ObjectIdentifier{{}}},
		"one-arc eku":         {EKU: []asn1.ObjectIdentifier{{1}}},
		"duplicate eku":       {EKU: []asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 2}, {1, 3, 6, 1, 5, 5, 7, 3, 2}}},
		"control char in uri": {URI: "https://b.example/a\x01"},
	} {
		t.Run(name, func(t *testing.T) {
			signed := false
			sg := keySource{Signer: newSigner(t, audit.AlgECDSAP256), sign: func([]byte) ([]byte, error) { signed = true; return nil, errors.New("must not sign") }}
			if _, err := Build(sg, n); err == nil {
				t.Fatal("accepted")
			}
			if signed {
				t.Fatal("the key was asked to sign a request that was refused")
			}
		})
	}
}

// TestBuildWrongKey: a key source whose signature is by another key than the public key it reports (a
// swapped key, a faulty token) yields no request.
func TestBuildWrongKey(t *testing.T) {
	for _, alg := range schemes {
		t.Run(string(alg), func(t *testing.T) {
			other := newSigner(t, alg)
			sg := keySource{Signer: newSigner(t, alg), sign: other.Sign}
			b, err := Build(sg, Names{})
			if err == nil || b != nil {
				t.Fatalf("built a request from a signature by the wrong key: %v", err)
			}
			if !strings.Contains(err.Error(), "does not verify") {
				t.Fatalf("error %q does not say the signature does not verify", err)
			}
		})
	}
}

// TestBuildJunkSignatures: a key source that returns an empty, short, long or garbled signature, or an
// error, yields no request; the key source's error is wrapped and shown.
func TestBuildJunkSignatures(t *testing.T) {
	boom := errors.New("token gone")
	for _, alg := range schemes {
		good := newSigner(t, alg)
		for name, sig := range map[string][]byte{"empty": {}, "short": make([]byte, 10), "long": make([]byte, 200), "zeros": make([]byte, 64)} {
			t.Run(string(alg)+"/"+name, func(t *testing.T) {
				sg := keySource{Signer: good, sign: func([]byte) ([]byte, error) { return sig, nil }}
				if b, err := Build(sg, Names{}); err == nil || b != nil {
					t.Fatalf("built a request from a %s signature", name)
				}
			})
		}
		t.Run(string(alg)+"/error", func(t *testing.T) {
			sg := keySource{Signer: good, sign: func([]byte) ([]byte, error) { return nil, boom }}
			b, err := Build(sg, Names{})
			if b != nil || !errors.Is(err, boom) {
				t.Fatalf("got %v, %v; want the key source's error", b, err)
			}
		})
	}
}

// TestBuildPublicKeyChecks: a key source that reports a malformed public key, or a scheme with no
// request, is refused without signing.
func TestBuildPublicKeyChecks(t *testing.T) {
	good := newSigner(t, audit.AlgECDSAP256)
	for name, sg := range map[string]audit.Signer{
		"p256 key of ed25519 length": fakeKey{good, audit.AlgECDSAP256, make([]byte, 32)},
		"p256 point off the curve":   fakeKey{good, audit.AlgECDSAP256, append([]byte{4}, make([]byte, 64)...)},
		"ed25519 short key":          fakeKey{good, audit.AlgEd25519, make([]byte, 5)},
		"no key":                     fakeKey{good, audit.AlgEd25519, nil},
		"ml-dsa-65":                  fakeKey{good, audit.AlgMLDSA65, good.PublicKey()},
		"hybrid":                     fakeKey{good, audit.AlgHybrid, good.PublicKey()},
		"unknown":                    fakeKey{good, "rsa-2048", good.PublicKey()},
	} {
		t.Run(name, func(t *testing.T) {
			if b, err := Build(sg, Names{}); err == nil || b != nil {
				t.Fatal("accepted")
			}
		})
	}
}

// fakeKey reports another scheme or key than the signer it wraps, and fails the test if it signs.
type fakeKey struct {
	audit.Signer
	alg audit.Alg
	pub []byte
}

func (f fakeKey) Alg() audit.Alg    { return f.alg }
func (f fakeKey) PublicKey() []byte { return f.pub }
func (f fakeKey) Sign([]byte) ([]byte, error) {
	panic("a refused key was asked to sign")
}

// TestPublicKey: the key source's encoded public key decodes to the standard library's key.
func TestPublicKey(t *testing.T) {
	ed := newSigner(t, audit.AlgEd25519).(audit.Ed25519Signer)
	k, err := PublicKey(ed)
	if err != nil {
		t.Fatal(err)
	}
	if !ed.Priv.Public().(ed25519.PublicKey).Equal(k) {
		t.Fatal("ed25519 key differs")
	}
	p := newSigner(t, audit.AlgECDSAP256).(audit.ECDSAP256Signer)
	k, err = PublicKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Priv.Public().(*ecdsa.PublicKey).Equal(k) {
		t.Fatal("p256 key differs")
	}
}

// TestBuildSignsTheRequestBody: the bytes the key source is asked to sign are the request's
// CertificationRequestInfo, exactly once.
func TestBuildSignsTheRequestBody(t *testing.T) {
	for _, alg := range schemes {
		t.Run(string(alg), func(t *testing.T) {
			var got [][]byte
			sg := keySource{Signer: newSigner(t, alg)}
			sg.sign = func(m []byte) ([]byte, error) { got = append(got, bytes.Clone(m)); return sg.Signer.Sign(m) }
			b, err := Build(sg, Names{Email: "ops@bank.example"})
			if err != nil {
				t.Fatal(err)
			}
			req := parse(t, b)
			if len(got) != 1 || !bytes.Equal(got[0], req.RawTBSCertificateRequest) {
				t.Fatalf("%d signatures; signed bytes are not the request's body", len(got))
			}
		})
	}
}

// TestOpenSSL: openssl verifies each request and reads the same public key and names. The Go checks in
// the other tests always run; this one is the second implementation. It is skipped when openssl is
// absent unless BIDE_REQUIRE_OPENSSL is set (the Integration job sets it), when it fails.
func TestOpenSSL(t *testing.T) {
	ossl, err := exec.LookPath("openssl")
	if err != nil {
		if os.Getenv("BIDE_REQUIRE_OPENSSL") != "" {
			t.Fatalf("BIDE_REQUIRE_OPENSSL is set and openssl is not on PATH: %v", err)
		}
		t.Skip("openssl is not on PATH")
	}
	for _, alg := range schemes {
		t.Run(string(alg), func(t *testing.T) {
			sg := newSigner(t, alg)
			b, err := Build(sg, Names{Email: "ops@bank.example", EKU: []asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 2}}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "r.csr.pem")
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(ossl, "req", "-in", path, "-noout", "-verify", "-text").CombinedOutput()
			if err != nil {
				t.Fatalf("openssl req -verify: %v\n%s", err, out)
			}
			for _, want := range []string{"email:ops@bank.example", "TLS Web Client Authentication"} {
				if !strings.Contains(string(out), want) {
					t.Errorf("openssl -text does not show %q:\n%s", want, out)
				}
			}
			pub, err := exec.Command(ossl, "req", "-in", path, "-pubkey", "-noout").Output()
			if err != nil {
				t.Fatal(err)
			}
			blk, _ := pem.Decode(pub)
			if blk == nil || !bytes.Equal(blk.Bytes, sourceSPKI(t, sg)) {
				t.Fatal("openssl reads another public key than the key source's")
			}
		})
	}
}

// TestCheck: Check refuses what Build refuses and accepts what it builds, without a key.
func TestCheck(t *testing.T) {
	if err := Check(Names{Email: "ops@bank.example"}); err != nil {
		t.Fatal(err)
	}
	if err := Check(Names{Email: "a@b.example", URI: "https://b.example/a"}); err == nil {
		t.Fatal("accepted two SANs")
	}
}

// TestSelfCheckEKU: the self-check refuses a request whose extended key usages, or whose extensions, are
// not what was asked for, so the claim that a request carries exactly the names asked for holds for the EKU too.
func TestSelfCheckEKU(t *testing.T) {
	clientAuth := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 2}
	emailProt := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 4}
	sg := newSigner(t, audit.AlgEd25519)
	b, err := Build(sg, Names{Email: "ops@bank.example", EKU: []asn1.ObjectIdentifier{clientAuth, emailProt}})
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	spki := sourceSPKI(t, sg)
	for name, n := range map[string]Names{
		"other usage":   {Email: "ops@bank.example", EKU: []asn1.ObjectIdentifier{clientAuth, {1, 2, 3, 4}}},
		"other order":   {Email: "ops@bank.example", EKU: []asn1.ObjectIdentifier{emailProt, clientAuth}},
		"fewer usages":  {Email: "ops@bank.example", EKU: []asn1.ObjectIdentifier{clientAuth}},
		"no usage":      {Email: "ops@bank.example"},
		"no san, usage": {EKU: []asn1.ObjectIdentifier{clientAuth, emailProt}},
	} {
		if err := selfCheck(blk.Bytes, spki, n); err == nil {
			t.Errorf("%s: the self-check accepted a request that does not match what was asked", name)
		}
	}
	// An extension that was not asked for (here a key usage) is refused although SAN and EKU match.
	priv := sg.(audit.Ed25519Signer).Priv
	extra, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		EmailAddresses: []string{"ops@bank.example"},
		ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 15}, Value: []byte{3, 2, 7, 0x80}}},
	}, priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := selfCheck(extra, spki, Names{Email: "ops@bank.example"}); err == nil {
		t.Error("the self-check accepted an extension that was not asked for")
	}
	if err := selfCheck(blk.Bytes, spki, Names{Email: "ops@bank.example", EKU: []asn1.ObjectIdentifier{clientAuth, emailProt}}); err != nil {
		t.Fatal(err)
	}
}
