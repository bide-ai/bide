package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"

	"github.com/bide-ai/bide/signer/azurekv"
)

const azureKeyID = "https://myvault.vault.azure.net/keys/bide-audit/0123456789abcdef0123456789abcdef"

// fakeVault answers as Key Vault does for one EC-HSM P-256 key; no test calls Azure.
type fakeVault struct{ priv *ecdsa.PrivateKey }

func (f fakeVault) GetKey(context.Context, string, string, *azkeys.GetKeyOptions) (azkeys.GetKeyResponse, error) {
	return azkeys.GetKeyResponse{KeyBundle: azkeys.KeyBundle{
		Attributes: &azkeys.KeyAttributes{Enabled: to.Ptr(true)},
		Key: &azkeys.JSONWebKey{
			KID: to.Ptr(azkeys.ID(azureKeyID)), Kty: to.Ptr(azkeys.KeyTypeECHSM), Crv: to.Ptr(azkeys.CurveNameP256),
			KeyOps: []*azkeys.KeyOperation{to.Ptr(azkeys.KeyOperationSign)},
			X:      f.priv.X.FillBytes(make([]byte, 32)), Y: f.priv.Y.FillBytes(make([]byte, 32)),
		},
	}}, nil
}

func (f fakeVault) Sign(_ context.Context, _, _ string, p azkeys.SignParameters, _ *azkeys.SignOptions) (azkeys.SignResponse, error) {
	r, s, err := ecdsa.Sign(rand.Reader, f.priv, p.Value)
	if err != nil {
		return azkeys.SignResponse{}, err
	}
	return azkeys.SignResponse{KeyOperationResult: azkeys.KeyOperationResult{
		Result: append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)}}, nil
}

func TestRunAzure(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	azureAPI = fakeVault{priv}
	defer func() { azureAPI = nil }()
	env := map[string]string{"BIDE_AZURE_KEY_ID": azureKeyID}
	var out bytes.Buffer
	if err := run(context.Background(), func(k string) string { return env[k] }, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"log public key: ecdsa-p256:", "signature valid, root is the journal's root", "verified with the public key alone"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestRunAzureConfigErrors(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unversioned id": {"BIDE_AZURE_KEY_ID": "https://myvault.vault.azure.net/keys/bide-audit"},
		"both clouds":    {"BIDE_AZURE_KEY_ID": azureKeyID, "BIDE_GCPKMS_KEY_VERSION": "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"},
	} {
		err := run(context.Background(), func(k string) string { return env[k] }, &bytes.Buffer{})
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A software key is refused unless the example is told to accept one.
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	azureAPI = softVault{fakeVault{priv}}
	defer func() { azureAPI = nil }()
	env := map[string]string{"BIDE_AZURE_KEY_ID": azureKeyID}
	err := run(context.Background(), func(k string) string { return env[k] }, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), azurekv.ErrNotHSM.Error()) {
		t.Fatalf("software key: err = %v", err)
	}
	env["BIDE_AZURE_ALLOW_SOFTWARE_KEY"] = "1"
	if err := run(context.Background(), func(k string) string { return env[k] }, &bytes.Buffer{}); err != nil {
		t.Fatalf("allowed software key: %v", err)
	}
}

type softVault struct{ fakeVault }

func (s softVault) GetKey(ctx context.Context, n, v string, o *azkeys.GetKeyOptions) (azkeys.GetKeyResponse, error) {
	r, err := s.fakeVault.GetKey(ctx, n, v, o)
	r.Key.Kty = to.Ptr(azkeys.KeyTypeEC)
	return r, err
}
