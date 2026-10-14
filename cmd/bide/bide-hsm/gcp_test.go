package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	gax "github.com/googleapis/gax-go/v2"

	"github.com/bide-ai/bide/signer/gcpkms"
)

// stubGCP is a Cloud KMS client that reports one P-256 key version at a protection level and stops
// at the public key, so what a test sees is how far Open got: the flags that decide the protection
// level is passed through if, and only if, it changed the outcome.
// The emulator-based tests of the gcpkms: reference are in the emulatortest module.
type stubGCP struct {
	level kmspb.ProtectionLevel
}

func (s stubGCP) GetCryptoKeyVersion(_ context.Context, r *kmspb.GetCryptoKeyVersionRequest, _ ...gax.CallOption) (*kmspb.CryptoKeyVersion, error) {
	return &kmspb.CryptoKeyVersion{Name: r.Name, Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256, State: kmspb.CryptoKeyVersion_ENABLED, ProtectionLevel: s.level}, nil
}

func (stubGCP) GetPublicKey(context.Context, *kmspb.GetPublicKeyRequest, ...gax.CallOption) (*kmspb.PublicKey, error) {
	return nil, errors.New("stub: get the public key")
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

// -allow-software-key reaches the signer's Config: a software key is refused without it and goes on
// with it. (-verify-attestation is shown reaching the signer in the emulatortest module.)
func TestGCPKMSFlagsReachTheSigner(t *testing.T) {
	if r := gcpWith(t, kmspb.ProtectionLevel_SOFTWARE); r.code != 1 || !strings.Contains(r.errOut, "protection level") {
		t.Fatalf("software, default: %d\n%s", r.code, r.all())
	}
	if r := gcpWith(t, kmspb.ProtectionLevel_SOFTWARE, "-allow-software-key"); r.code != 1 || !strings.Contains(r.errOut, "get the public key") {
		t.Fatalf("software, allowed: %d\n%s", r.code, r.all())
	}
	if r := gcpWith(t, kmspb.ProtectionLevel_HSM); r.code != 1 || !strings.Contains(r.errOut, "get the public key") {
		t.Fatalf("HSM, default: %d\n%s", r.code, r.all())
	}
}

// The flags are for a gcpkms: key only, and a software key has no attestation.
func TestGCPKMSFlagRefusals(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-key", "awskms:arn:aws:kms:us-east-1:111122223333:key/abc", "-allow-software-key"}, "gcpkms: key only"},
		{[]string{"-key", "awskms:arn:aws:kms:us-east-1:111122223333:key/abc", "-verify-attestation"}, "gcpkms: key only"},
		{[]string{"-key", gcpRef, "-allow-software-key", "-verify-attestation"}, "do not go together"},
	} {
		if r := hsm(t, nil, "", append([]string{"pubkey"}, tc.args...)...); r.code != 1 || !strings.Contains(r.errOut, tc.want) {
			t.Errorf("%v: %d\n%s", tc.args, r.code, r.all())
		}
	}
}
