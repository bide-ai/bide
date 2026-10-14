package azurekv

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"
	kvfake "github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys/fake"

	"github.com/bide-ai/bide/audit"
)

type fakeCred struct{}

func (fakeCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "fake-token"}, nil
}

// The real azkeys client talking to the azkeys in-process fake server (Microsoft's, in the SDK's
// fake package) proves the signer's requests and the SDK's response types agree on the wire: the
// JSON key bundle, the base64url digest and the base64url r||s result. No network is used.
func TestRealClientAgainstSDKFakeServer(t *testing.T) {
	kv := newFakeKV(t)
	var gotName, gotVersion string
	srv := kvfake.Server{
		GetKey: func(_ context.Context, name, version string, _ *azkeys.GetKeyOptions) (azfake.Responder[azkeys.GetKeyResponse], azfake.ErrorResponder) {
			if n, v, ok := strings.Cut(name, "/"); ok && version == "" { // the same fake routing quirk as Sign's
				name, version = n, v
			}
			gotName, gotVersion = name, version
			var r azfake.Responder[azkeys.GetKeyResponse]
			r.SetResponse(http.StatusOK, azkeys.GetKeyResponse{KeyBundle: kv.bundle}, nil)
			return r, azfake.ErrorResponder{}
		},
		Sign: func(ctx context.Context, name, version string, p azkeys.SignParameters, o *azkeys.SignOptions) (azfake.Responder[azkeys.SignResponse], azfake.ErrorResponder) {
			// The SDK fake's route pattern lets the key name swallow "/<version>" on Sign (its
			// character class includes "/"); split it as the real service's router would.
			if n, v, ok := strings.Cut(name, "/"); ok && version == "" {
				name, version = n, v
			}
			resp, err := kv.Sign(ctx, name, version, p, o)
			var r azfake.Responder[azkeys.SignResponse]
			var e azfake.ErrorResponder
			if err != nil {
				e.SetResponseError(http.StatusBadRequest, "BadParameter")
				return r, e
			}
			r.SetResponse(http.StatusOK, resp, nil)
			return r, e
		},
	}
	c, err := azkeys.NewClient(testVault, fakeCred{}, &azkeys.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: kvfake.NewServerTransport(&srv)},
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), Config{Client: c, KeyID: testKeyID})
	if err != nil {
		t.Fatal(err)
	}
	if gotName != testName || gotVersion != testVersion {
		t.Fatalf("GetKey for %q %q", gotName, gotVersion)
	}
	v, _ := audit.VerifierOf(s)
	sig, err := s.Sign([]byte("over the wire"))
	if err != nil || !v.Verify([]byte("over the wire"), sig) {
		t.Fatalf("sig %x err %v", sig, err)
	}

	// A service error comes back as the reduced *Error.
	srv.Sign = func(context.Context, string, string, azkeys.SignParameters, *azkeys.SignOptions) (azfake.Responder[azkeys.SignResponse], azfake.ErrorResponder) {
		var e azfake.ErrorResponder
		e.SetResponseError(http.StatusForbidden, "Forbidden")
		return azfake.Responder[azkeys.SignResponse]{}, e
	}
	_, err = s.Sign([]byte("m"))
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusForbidden || e.Code != "Forbidden" {
		t.Fatalf("err = %#v", err)
	}
}
