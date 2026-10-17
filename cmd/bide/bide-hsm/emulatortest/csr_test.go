package emulatortest

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/internal/csr"
)

// A certificate request for a Cloud KMS key version is signed by the key through AsymmetricSign, for
// each scheme Cloud KMS has (P-256 with SHA-256 as a digest, Ed25519 over the message): it verifies
// under the key's public key, with openssl too.
func TestCSRForCloudKMSKey(t *testing.T) {
	e := startEmulator(t)
	for _, tc := range []struct {
		alg    kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm
		scheme audit.Alg
	}{{kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256, audit.AlgECDSAP256}, {kmspb.CryptoKeyVersion_EC_SIGN_ED25519, audit.AlgEd25519}} {
		t.Run(string(tc.scheme), func(t *testing.T) {
			s, err := e.open(t, nil, e.ref(t, tc.alg), soft)
			if err != nil {
				t.Fatal(err)
			}
			b, err := csr.Build(s, csr.Names{Email: "ops@bank.example"})
			if err != nil {
				t.Fatal(err)
			}
			blk, _ := pem.Decode(b)
			req, err := x509.ParseCertificateRequest(blk.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			if err := req.CheckSignature(); err != nil {
				t.Fatalf("proof of possession: %v", err)
			}
			pub, _ := csr.PublicKey(s)
			want, _ := x509.MarshalPKIXPublicKey(pub)
			if !bytes.Equal(req.RawSubjectPublicKeyInfo, want) {
				t.Fatal("the request is not for the key version")
			}
			opensslVerifies(t, b)
		})
	}
}

func opensslVerifies(t *testing.T, req []byte) {
	t.Helper()
	ossl, err := exec.LookPath("openssl")
	if err != nil {
		if os.Getenv("BIDE_REQUIRE_OPENSSL") != "" {
			t.Fatalf("BIDE_REQUIRE_OPENSSL is set and openssl is not on PATH: %v", err)
		}
		t.Log("openssl is not on PATH: the request was checked by crypto/x509 only")
		return
	}
	p := filepath.Join(t.TempDir(), "r.pem")
	if err := os.WriteFile(p, req, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(ossl, "req", "-in", p, "-noout", "-verify").CombinedOutput(); err != nil {
		t.Fatalf("openssl req -verify: %v\n%s", err, out)
	}
}
