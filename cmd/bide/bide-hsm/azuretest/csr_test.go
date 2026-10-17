package azuretest

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/hsmkeys"
	"github.com/bide-ai/bide/cmd/bide/internal/csr"
)

// A certificate request for a Key Vault key (ECDSA P-256, the only scheme Key Vault has) is signed by the
// vault's key through the digest-then-sign API: it verifies under the key's public key, with openssl too,
// and a vault with no Ed25519 refuses -alg ed25519 before any request exists.
func TestCSRForKeyVaultKey(t *testing.T) {
	k := newKV()
	ref := k.add(t, "approver-csr", azkeys.KeyTypeECHSM)
	s, err := open(t, k, ref, hsmkeys.Options{})
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
		t.Fatal("the request is not for the vault key")
	}
	opensslVerifies(t, b)
	if _, err := open(t, k, ref, hsmkeys.Options{Alg: audit.AlgEd25519}); err == nil {
		t.Fatal("a Key Vault key opened as Ed25519")
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
