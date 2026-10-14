package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"hash/crc32"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/bide-ai/bide/signer/gcpkms"
)

// stubGCP is a Cloud KMS client that reports one P-256 key version at a protection level, with a valid
// public key and no attestation, and stops at the self-test signature, so what a test sees is how
// far Open got: the flags that decide the protection
// level is passed through if, and only if, it changed the outcome.
// The emulator-based tests of the gcpkms: reference are in the emulatortest module.
type stubGCP struct {
	level kmspb.ProtectionLevel
}

func (s stubGCP) GetCryptoKeyVersion(_ context.Context, r *kmspb.GetCryptoKeyVersionRequest, _ ...gax.CallOption) (*kmspb.CryptoKeyVersion, error) {
	return &kmspb.CryptoKeyVersion{Name: r.Name, Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256, State: kmspb.CryptoKeyVersion_ENABLED, ProtectionLevel: s.level}, nil
}

func (s stubGCP) GetPublicKey(_ context.Context, r *kmspb.GetPublicKeyRequest, _ ...gax.CallOption) (*kmspb.PublicKey, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return nil, err
	}
	p := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	return &kmspb.PublicKey{Name: r.Name, Pem: p, PemCrc32C: wrapperspb.Int64(int64(crc32.Checksum([]byte(p), crc32.MakeTable(crc32.Castagnoli)))),
		Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256, ProtectionLevel: s.level}, nil
}

func (stubGCP) AsymmetricSign(context.Context, *kmspb.AsymmetricSignRequest, ...gax.CallOption) (*kmspb.AsymmetricSignResponse, error) {
	return nil, errors.New("stub: no signing")
}

const gcpRef = "gcpkms:projects/bank-prod/locations/europe-west2/keyRings/bide/cryptoKeys/approver/cryptoKeyVersions/1"

func gcpWith(t *testing.T, level kmspb.ProtectionLevel, args ...string) result {
	t.Helper()
	d := deps{gcp: func(context.Context) (gcpkms.API, error) { return stubGCP{level}, nil }}
	return hsmWith(t, d, "", append([]string{"pubkey", "-key", gcpRef}, args...)...)
}

// -allow-software-key and -verify-attestation reach the signer's Config: a software key is refused
// without the first and goes on with it; an HSM key goes on to the self-test signature unless the
// second makes it check the (absent) attestation first.
func TestGCPKMSFlagsReachTheSigner(t *testing.T) {
	if r := gcpWith(t, kmspb.ProtectionLevel_SOFTWARE); r.code != 1 || !strings.Contains(r.errOut, "protection level") {
		t.Fatalf("software, default: %d\n%s", r.code, r.all())
	}
	if r := gcpWith(t, kmspb.ProtectionLevel_SOFTWARE, "-allow-software-key"); r.code != 1 || !strings.Contains(r.errOut, "self-test signature") {
		t.Fatalf("software, allowed: %d\n%s", r.code, r.all())
	}
	if r := gcpWith(t, kmspb.ProtectionLevel_HSM); r.code != 1 || !strings.Contains(r.errOut, "self-test signature") {
		t.Fatalf("HSM, default: %d\n%s", r.code, r.all())
	}
	if r := gcpWith(t, kmspb.ProtectionLevel_HSM, "-verify-attestation"); r.code != 1 || strings.Contains(r.errOut, "self-test signature") || !strings.Contains(strings.ToLower(r.errOut), "attestation") {
		t.Fatalf("HSM, attestation: %d\n%s", r.code, r.all())
	}
}

// The flags are for a gcpkms: key only, and a software key has no attestation.
func TestGCPKMSFlagRefusals(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-key", "awskms:arn:aws:kms:us-east-1:111122223333:key/abc", "-allow-software-key"}, "gcpkms: or azurekv: key only"},
		{[]string{"-key", "awskms:arn:aws:kms:us-east-1:111122223333:key/abc", "-verify-attestation"}, "gcpkms: key only"},
		{[]string{"-key", gcpRef, "-allow-software-key", "-verify-attestation"}, "do not go together"},
	} {
		if r := hsm(t, nil, "", append([]string{"pubkey"}, tc.args...)...); r.code != 1 || !strings.Contains(r.errOut, tc.want) {
			t.Errorf("%v: %d\n%s", tc.args, r.code, r.all())
		}
	}
}

// -allow-software-key is for a gcpkms: or azurekv: key; -verify-attestation for a gcpkms: key only.
func TestAzureKVFlags(t *testing.T) {
	const ref = "azurekv:https://bank-prod.vault.azure.net/keys/approver/0123456789abcdef0123456789abcdef"
	if r := hsm(t, nil, "", "pubkey", "-key", ref, "-verify-attestation"); r.code != 1 || !strings.Contains(r.errOut, "gcpkms: key only") {
		t.Fatalf("attestation on azure: %d\n%s", r.code, r.all())
	}
	if r := hsm(t, nil, "", "pubkey", "-key", ref, "-alg", "ed25519"); r.code != 1 || !strings.Contains(r.errOut, "no Ed25519") {
		t.Fatalf("-alg ed25519: %d\n%s", r.code, r.all())
	}
	if r := hsm(t, nil, "", "pubkey", "-key", "azurekv:https://x.example.com/keys/k/0123456789abcdef0123456789abcdef"); r.code != 1 || !strings.Contains(r.errOut, "not a Key Vault") {
		t.Fatalf("host: %d\n%s", r.code, r.all())
	}
}
