package azurekv

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

const (
	testVault   = "https://myvault.vault.azure.net"
	testName    = "bide-audit"
	testVersion = "0123456789abcdef0123456789abcdef"
	testKeyID   = testVault + "/keys/" + testName + "/" + testVersion
	secret      = "SECRET-TOKEN-eyJhbGciOi"
)

var n256 = elliptic.P256().Params().N

// fakeKV stands in for Key Vault. It holds a P-256 key and answers as the service does: it signs
// only a 32-byte digest under ES256 for the pinned name and version, and returns raw r||s. No test
// calls Azure.
type fakeKV struct {
	priv   *ecdsa.PrivateKey
	bundle azkeys.KeyBundle

	mu       sync.Mutex
	signs    atomic.Int32
	getErr   error
	signErr  error
	highS    bool                            // return a high-S signature (the service does not normalise)
	mutate   func(raw, digest []byte) []byte // replaces the signature bytes
	nilKID   bool
	signKID  *azkeys.ID // overrides the kid in the Sign response
	lastAlg  azkeys.SignatureAlgorithm
	lastName string
	lastVer  string
}

func newFakeKV(t *testing.T) *fakeKV {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeKV{priv: priv}
	f.bundle = azkeys.KeyBundle{
		Attributes: &azkeys.KeyAttributes{Enabled: to.Ptr(true), HSMPlatform: to.Ptr("2")},
		Key: &azkeys.JSONWebKey{
			KID:    to.Ptr(azkeys.ID(testKeyID)),
			Kty:    to.Ptr(azkeys.KeyTypeECHSM),
			Crv:    to.Ptr(azkeys.CurveNameP256),
			KeyOps: []*azkeys.KeyOperation{to.Ptr(azkeys.KeyOperationSign), to.Ptr(azkeys.KeyOperationVerify)},
			X:      priv.PublicKey.X.FillBytes(make([]byte, 32)),
			Y:      priv.PublicKey.Y.FillBytes(make([]byte, 32)),
		},
	}
	return f
}

func (f *fakeKV) GetKey(_ context.Context, name, version string, _ *azkeys.GetKeyOptions) (azkeys.GetKeyResponse, error) {
	if f.getErr != nil {
		return azkeys.GetKeyResponse{}, f.getErr
	}
	if name != testName || version != testVersion {
		return azkeys.GetKeyResponse{}, &azcore.ResponseError{StatusCode: 404, ErrorCode: "KeyNotFound"}
	}
	return azkeys.GetKeyResponse{KeyBundle: f.bundle}, nil
}

func (f *fakeKV) Sign(_ context.Context, name, version string, p azkeys.SignParameters, _ *azkeys.SignOptions) (azkeys.SignResponse, error) {
	f.signs.Add(1)
	f.mu.Lock()
	f.lastName, f.lastVer = name, version
	f.mu.Unlock()
	if f.signErr != nil {
		return azkeys.SignResponse{}, f.signErr
	}
	if p.Algorithm == nil || *p.Algorithm != azkeys.SignatureAlgorithmES256 || len(p.Value) != sha256.Size || version == "" {
		return azkeys.SignResponse{}, &azcore.ResponseError{StatusCode: 400, ErrorCode: "BadParameter"}
	}
	f.mu.Lock()
	f.lastAlg = *p.Algorithm
	f.mu.Unlock()
	r, s, err := ecdsa.Sign(rand.Reader, f.priv, p.Value)
	if err != nil {
		panic(err)
	}
	if half := new(big.Int).Rsh(n256, 1); f.highS == (s.Cmp(half) <= 0) {
		s = new(big.Int).Sub(n256, s)
	}
	raw := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	if f.mutate != nil {
		raw = f.mutate(raw, p.Value)
	}
	kid := f.signKID
	if kid == nil && !f.nilKID {
		kid = to.Ptr(azkeys.ID(testKeyID))
	}
	return azkeys.SignResponse{KeyOperationResult: azkeys.KeyOperationResult{KID: kid, Result: raw}}, nil
}

func open(t *testing.T, f *fakeKV, mod ...func(*Config)) (*Signer, error) {
	t.Helper()
	cfg := Config{Client: f, KeyID: testKeyID}
	for _, m := range mod {
		m(&cfg)
	}
	return Open(context.Background(), cfg)
}

func mustOpen(t *testing.T, f *fakeKV, mod ...func(*Config)) *Signer {
	t.Helper()
	s, err := open(t, f, mod...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSignVerifiesUnderBideVerifier(t *testing.T) {
	f := newFakeKV(t)
	s := mustOpen(t, f)
	if s.Alg() != audit.AlgECDSAP256 || s.KeyID() != testKeyID || !s.HSMProtected() {
		t.Fatalf("Alg %s, KeyID %s, HSM %v", s.Alg(), s.KeyID(), s.HSMProtected())
	}
	if f.signs.Load() != 1 {
		t.Fatalf("Open signed %d times, want the one self-test", f.signs.Load())
	}
	if f.lastName != testName || f.lastVer != testVersion || f.lastAlg != azkeys.SignatureAlgorithmES256 {
		t.Fatalf("signed with %q %q %q", f.lastName, f.lastVer, f.lastAlg)
	}
	v, err := audit.VerifierOf(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, 100, 100000} {
		msg := bytes.Repeat([]byte{'x'}, n)
		sig, err := s.Sign(msg)
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		if len(sig) != 64 || !v.Verify(msg, sig) {
			t.Fatalf("audit verifier rejects the signature over %d bytes", n)
		}
	}
	other, _ := s.Sign([]byte("m"))
	if v.Verify([]byte("other"), other) {
		t.Fatal("verified over another message")
	}
	if !bytes.Equal(s.PublicKey(), append([]byte{4}, append(f.priv.X.FillBytes(make([]byte, 32)), f.priv.Y.FillBytes(make([]byte, 32))...)...)) {
		t.Fatal("public key is not the SEC1 point of the key")
	}
	pk := s.PublicKey()
	pk[0] ^= 0xff
	if s.PublicKey()[0] != 4 {
		t.Fatal("PublicKey returns the internal slice")
	}
}

// The service returns whatever s its signing produced; audit accepts only low-S. A high-S raw
// signature is folded to the same r and the low s.
func TestHighSFolded(t *testing.T) {
	f := newFakeKV(t)
	f.highS = true
	var raws [][]byte
	f.mutate = func(raw, _ []byte) []byte { raws = append(raws, bytes.Clone(raw)); return raw }
	s := mustOpen(t, f)
	v, _ := audit.VerifierOf(s)
	for i := 0; i < 8; i++ {
		msg := []byte(fmt.Sprint("m", i))
		sig, err := s.Sign(msg)
		if err != nil {
			t.Fatal(err)
		}
		raw := raws[len(raws)-1]
		if new(big.Int).SetBytes(raw[32:]).Cmp(new(big.Int).Rsh(n256, 1)) <= 0 {
			t.Fatal("the fake did not return a high-S signature")
		}
		if !v.Verify(msg, sig) || new(big.Int).SetBytes(sig[32:]).Cmp(new(big.Int).Rsh(n256, 1)) > 0 {
			t.Fatal("signature is not a verifying low-S signature")
		}
		if !bytes.Equal(sig[:32], raw[:32]) {
			t.Fatal("r changed")
		}
		wantS := new(big.Int).Sub(n256, new(big.Int).SetBytes(raw[32:]))
		if new(big.Int).SetBytes(sig[32:]).Cmp(wantS) != 0 {
			t.Fatal("s is not n-s")
		}
	}
}

func TestKeyChecks(t *testing.T) {
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	cases := []struct {
		name  string
		mod   func(*fakeKV)
		cfg   func(*Config)
		want  error
		opens bool
	}{
		{name: "hsm key", opens: true},
		{name: "software key refused by default", mod: func(f *fakeKV) { f.bundle.Key.Kty = to.Ptr(azkeys.KeyTypeEC) }, want: ErrNotHSM},
		{name: "software key allowed", mod: func(f *fakeKV) { f.bundle.Key.Kty = to.Ptr(azkeys.KeyTypeEC) },
			cfg: func(c *Config) { c.AllowSoftwareKey = true }, opens: true},
		{name: "hsmPlatform 0 refused", mod: func(f *fakeKV) { f.bundle.Attributes.HSMPlatform = to.Ptr("0") }, want: ErrNotHSM},
		{name: "hsmPlatform 1 ok", mod: func(f *fakeKV) { f.bundle.Attributes.HSMPlatform = to.Ptr("1") }, opens: true},
		{name: "no hsmPlatform (managed hsm) ok", mod: func(f *fakeKV) { f.bundle.Attributes.HSMPlatform = nil }, opens: true},
		{name: "wrong curve P-384", mod: func(f *fakeKV) { f.bundle.Key.Crv = to.Ptr(azkeys.CurveNameP384) }, want: ErrNoP256},
		{name: "wrong curve P-256K", mod: func(f *fakeKV) { f.bundle.Key.Crv = to.Ptr(azkeys.CurveNameP256K) }, want: ErrNoP256},
		{name: "rsa key", mod: func(f *fakeKV) { f.bundle.Key.Kty = to.Ptr(azkeys.KeyTypeRSAHSM) }, want: ErrNoP256},
		{name: "no curve", mod: func(f *fakeKV) { f.bundle.Key.Crv = nil }, want: ErrNoP256},
		{name: "no key", mod: func(f *fakeKV) { f.bundle.Key = nil }, want: ErrNoP256},
		{name: "point not on curve", mod: func(f *fakeKV) { f.bundle.Key.Y[31] ^= 1 }, want: ErrNoP256},
		{name: "coordinate too long", mod: func(f *fakeKV) { f.bundle.Key.X = append([]byte{0}, f.bundle.Key.X...) }, want: ErrNoP256},
		{name: "coordinate much too long", mod: func(f *fakeKV) { f.bundle.Key.Y = make([]byte, 40) }, want: ErrNoP256},
		{name: "x much too long", mod: func(f *fakeKV) { f.bundle.Key.X = make([]byte, 40) }, want: ErrNoP256},
		{name: "no coordinates", mod: func(f *fakeKV) { f.bundle.Key.X = nil }, want: ErrNoP256},
		{name: "disabled", mod: func(f *fakeKV) { f.bundle.Attributes.Enabled = to.Ptr(false) }, want: ErrKeyUnusable},
		{name: "enabled unset means enabled", mod: func(f *fakeKV) { f.bundle.Attributes.Enabled = nil }, opens: true},
		{name: "no attributes", mod: func(f *fakeKV) { f.bundle.Attributes = nil }, want: ErrKeyUnusable},
		{name: "expired", mod: func(f *fakeKV) { f.bundle.Attributes.Expires = &past }, want: ErrKeyUnusable},
		{name: "not yet valid", mod: func(f *fakeKV) { f.bundle.Attributes.NotBefore = &future }, want: ErrKeyUnusable},
		{name: "inside window", mod: func(f *fakeKV) { f.bundle.Attributes.NotBefore = &past; f.bundle.Attributes.Expires = &future }, opens: true},
		{name: "no sign op", mod: func(f *fakeKV) {
			f.bundle.Key.KeyOps = []*azkeys.KeyOperation{to.Ptr(azkeys.KeyOperationVerify)}
		}, want: ErrKeyUnusable},
		{name: "no key_ops", mod: func(f *fakeKV) { f.bundle.Key.KeyOps = nil }, want: ErrKeyUnusable},
		{name: "exportable", mod: func(f *fakeKV) { f.bundle.Attributes.Exportable = to.Ptr(true) }, want: ErrExportable},
		{name: "release policy", mod: func(f *fakeKV) { f.bundle.ReleasePolicy = &azkeys.KeyReleasePolicy{} }, want: ErrExportable},
		{name: "other version answered", mod: func(f *fakeKV) {
			f.bundle.Key.KID = to.Ptr(azkeys.ID(testVault + "/keys/" + testName + "/ffffffffffffffffffffffffffffffff"))
		}, want: ErrKeyMismatch},
		{name: "other key answered", mod: func(f *fakeKV) {
			f.bundle.Key.KID = to.Ptr(azkeys.ID(testVault + "/keys/other/" + testVersion))
		}, want: ErrKeyMismatch},
		{name: "other vault answered", mod: func(f *fakeKV) {
			f.bundle.Key.KID = to.Ptr(azkeys.ID("https://evil.vault.azure.net/keys/" + testName + "/" + testVersion))
		}, want: ErrKeyMismatch},
		{name: "no kid", mod: func(f *fakeKV) { f.bundle.Key.KID = nil }, want: ErrKeyMismatch},
		{name: "kid case differs", mod: func(f *fakeKV) {
			f.bundle.Key.KID = to.Ptr(azkeys.ID(strings.ToUpper(testKeyID)))
		}, opens: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeKV(t)
			if c.mod != nil {
				c.mod(f)
			}
			var mods []func(*Config)
			if c.cfg != nil {
				mods = append(mods, c.cfg)
			}
			s, err := open(t, f, mods...)
			if c.opens {
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				if s == nil {
					t.Fatal("nil signer")
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("Open = %v, want %v", err, c.want)
			}
			if f.signs.Load() != 0 {
				t.Fatal("a refused key was used to sign")
			}
		})
	}
}

func TestShortCoordinatesPadded(t *testing.T) {
	// Find a key whose X has a leading zero byte (1 in 256) and trim it as a service might.
	for i := 0; i < 20000; i++ {
		f := newFakeKV(t)
		if f.priv.X.FillBytes(make([]byte, 32))[0] != 0 {
			continue
		}
		f.bundle.Key.X = f.priv.X.Bytes()
		if len(f.bundle.Key.X) == 32 {
			t.Fatal("not trimmed")
		}
		s := mustOpen(t, f)
		if _, err := s.Sign([]byte("m")); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Skip("no key with a leading zero coordinate found")
}

func TestSignatureChecks(t *testing.T) {
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cases := map[string]func(raw, digest []byte) []byte{
		"signed by another key": func(_, d []byte) []byte {
			r, s, _ := ecdsa.Sign(rand.Reader, other, d)
			return append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
		},
		"signed another digest": func(raw, _ []byte) []byte {
			d := sha256.Sum256([]byte("other"))
			r, s, _ := ecdsa.Sign(rand.Reader, other, d[:])
			_ = raw
			return append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
		},
		"bit flipped": func(raw, _ []byte) []byte { raw[5] ^= 1; return raw },
		"short":       func(raw, _ []byte) []byte { return raw[:63] },
		"long":        func(raw, _ []byte) []byte { return append(raw, 0) },
		"empty":       func(_, _ []byte) []byte { return nil },
		"der":         func(raw, _ []byte) []byte { return append([]byte{0x30, 0x44, 0x02, 0x20}, raw...) },
		"zero r":      func(raw, _ []byte) []byte { return append(make([]byte, 32), raw[32:]...) },
		"zero s":      func(raw, _ []byte) []byte { return append(raw[:32], make([]byte, 32)...) },
		"r at n":      func(raw, _ []byte) []byte { return append(n256.FillBytes(make([]byte, 32)), raw[32:]...) },
		"s at n":      func(raw, _ []byte) []byte { return append(raw[:32], n256.FillBytes(make([]byte, 32))...) },
		"all ff":      func(_, _ []byte) []byte { return bytes.Repeat([]byte{0xff}, 64) },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeKV(t)
			s := mustOpen(t, f)
			f.mutate = mut
			if sig, err := s.Sign([]byte("m")); err == nil {
				t.Fatalf("signed %x", sig)
			}
			// Open runs the same check on its self-test.
			if _, err := open(t, f); err == nil {
				t.Fatal("Open accepted a service whose signatures do not verify")
			}
		})
	}
}

func TestSignKIDMismatch(t *testing.T) {
	f := newFakeKV(t)
	s := mustOpen(t, f)
	f.signKID = to.Ptr(azkeys.ID(testVault + "/keys/" + testName + "/ffffffffffffffffffffffffffffffff"))
	if _, err := s.Sign([]byte("m")); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestExpiryAfterOpen(t *testing.T) {
	f := newFakeKV(t)
	exp := time.Now().Add(time.Hour)
	f.bundle.Attributes.Expires = &exp
	now := time.Now()
	s := mustOpen(t, f, func(c *Config) { c.Now = func() time.Time { return now } })
	if _, err := s.Sign([]byte("m")); err != nil {
		t.Fatal(err)
	}
	now = exp // the instant of expiry is already outside the window
	calls := f.signs.Load()
	if _, err := s.Sign([]byte("m")); !errors.Is(err, ErrKeyUnusable) {
		t.Fatalf("err = %v", err)
	}
	if f.signs.Load() != calls {
		t.Fatal("an expired key was sent to the service")
	}
}

func TestParseKeyID(t *testing.T) {
	ok := []string{
		testKeyID,
		"https://MyVault.Vault.Azure.Net/keys/bide-audit/0123456789ABCDEF0123456789abcdef",
		"https://myhsm.managedhsm.azure.net/keys/k/" + testVersion,
		"https://v.vault.azure.cn/keys/k/" + testVersion,
		"https://v.vault.usgovcloudapi.net/keys/k/" + testVersion,
	}
	for _, id := range ok {
		if _, err := parseKeyID(id, nil); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
	bad := map[string]string{
		"":                      "",
		"no version":            testVault + "/keys/" + testName,
		"trailing slash":        testVault + "/keys/" + testName + "/",
		"http":                  "http://myvault.vault.azure.net/keys/k/" + testVersion,
		"other host":            "https://example.com/keys/k/" + testVersion,
		"suffix only":           "https://vault.azure.net/keys/k/" + testVersion,
		"suffix trick":          "https://myvault.vault.azure.net.evil.com/keys/k/" + testVersion,
		"lookalike":             "https://evilvault.azure.net/keys/k/" + testVersion,
		"userinfo":              "https://u:p@myvault.vault.azure.net/keys/k/" + testVersion,
		"port":                  "https://myvault.vault.azure.net:8443/keys/k/" + testVersion,
		"query":                 testKeyID + "?api-version=1",
		"fragment":              testKeyID + "#x",
		"secrets":               testVault + "/secrets/k/" + testVersion,
		"short version":         testVault + "/keys/k/abc",
		"non-hex version":       testVault + "/keys/k/zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
		"extra segment":         testKeyID + "/sign",
		"bad name":              testVault + "/keys/k_k/" + testVersion,
		"encoded slash in name": testVault + "/keys/k%2Fx/" + testVersion,
		"not a url":             "myvault/keys/k/" + testVersion,
	}
	for name, id := range bad {
		_, err := parseKeyID(id, nil)
		if !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: %q: err = %v, want ErrConfig", name, id, err)
		}
	}
	if _, err := parseKeyID("https://kv.example.test/keys/k/"+testVersion, []string{".example.test"}); err != nil {
		t.Errorf("custom suffix: %v", err)
	}
	// A suffix with a leading dot needs a label before it; one without names exactly one host.
	if _, err := parseKeyID("https://kv.example.test/keys/k/"+testVersion, []string{"kv.example.test"}); err != nil {
		t.Errorf("exact host: %v", err)
	}
	for _, c := range []struct{ suffix, host string }{
		{"azure.net", "evilazure.net"}, {"vault.azure.cn", "evilvault.azure.cn"}, {".example.test", "example.test"},
		{"kv.example.test", "xkv.example.test"}, {"kv.example.test", "a.kv.example.test"}, {"", "evil.example"},
	} {
		if _, err := parseKeyID("https://"+c.host+"/keys/k/"+testVersion, []string{c.suffix}); err == nil {
			t.Errorf("suffix %q accepted host %q", c.suffix, c.host)
		}
	}
	// An empty entry beside a good one is still an error: it would match every host.
	if _, err := parseKeyID("https://evil.example/keys/k/"+testVersion, []string{".vault.azure.net", ""}); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("empty suffix entry: %v", err)
	}
	if _, err := parseKeyID("https://myvault.vault.azure.net:/keys/k/"+testVersion, nil); err == nil {
		t.Error("an empty port was accepted")
	}
	if _, err := parseKeyID(testVault+"/keys/"+testName, nil); err == nil || !strings.Contains(err.Error(), "no version") {
		t.Errorf("a base identifier must be refused for its missing version, got %v", err)
	}
	// An unversioned identifier is rejected before any client is built.
	if _, err := Open(context.Background(), Config{Client: newFakeKV(t), KeyID: testVault + "/keys/" + testName}); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("Open(base id) = %v", err)
	}
	if _, err := Open(context.Background(), Config{Client: newFakeKV(t)}); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("Open(no id) = %v", err)
	}
}

func TestCredentialReachesClient(t *testing.T) {
	var gotURL string
	var gotCred azcore.TokenCredential
	f := newFakeKV(t)
	defer func(old func(string, azcore.TokenCredential) (API, error)) { newAPI = old }(newAPI)
	newAPI = func(u string, c azcore.TokenCredential) (API, error) { gotURL, gotCred = u, c; return f, nil }
	cred := &azidentity.ManagedIdentityCredential{}
	if _, err := Open(context.Background(), Config{KeyID: testKeyID, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	if gotURL != testVault || gotCred != azcore.TokenCredential(cred) {
		t.Fatalf("client built for %q with %v", gotURL, gotCred)
	}
	// With neither a client nor a credential the default credential chain is used, never a flag.
	gotCred = nil
	if _, err := Open(context.Background(), Config{KeyID: testKeyID}); err != nil {
		t.Fatal(err)
	}
	if gotCred == nil {
		t.Fatal("no credential was built")
	}
}

func TestConcurrentSign(t *testing.T) {
	f := newFakeKV(t)
	s := mustOpen(t, f)
	v, _ := audit.VerifierOf(s)
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() {
			msg := []byte(fmt.Sprint("m", i))
			sig, err := s.Sign(msg)
			if err == nil && !v.Verify(msg, sig) {
				err = errors.New("does not verify")
			}
			errs <- err
		}()
	}
	for i := 0; i < 16; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// --- errors carry no secrets ---

func responseErr(status int, code string) error {
	return &azcore.ResponseError{
		StatusCode: status, ErrorCode: code,
		RawResponse: &http.Response{
			StatusCode: status, Status: "denied " + secret,
			Header: http.Header{"Authorization": {"Bearer " + secret}, "X-Ms-Error-Code": {code}},
			Request: &http.Request{Method: "POST", Header: http.Header{"Authorization": {"Bearer " + secret}},
				URL: mustURL(testVault + "/keys/k/v/sign?token=" + secret)},
		},
	}
}

func allForms(v any) []string {
	return []string{fmt.Sprint(v), fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v), fmt.Sprintf("%s", v), fmt.Sprintf("%q", v)}
}

func TestErrorsCarryNoSecrets(t *testing.T) {
	authFailed := &azidentity.AuthenticationFailedError{}
	cases := []struct {
		name       string
		in         error
		wantStatus int
		wantCode   string
		is         error
	}{
		{"response error", fmt.Errorf("wrapped: %w", responseErr(403, "Forbidden")), 403, "Forbidden", nil},
		{"key disabled", responseErr(403, "KeyDisabled"), 403, "KeyDisabled", nil},
		{"hostile code", responseErr(500, secret+" with spaces"), 500, "Unclassified", nil},
		{"long code", responseErr(500, strings.Repeat("A", 200)), 500, "Unclassified", nil},
		{"auth failed", fmt.Errorf("%w: %s", authFailed, secret), 0, "AuthenticationFailed", nil},
		{"auth required", &azidentity.AuthenticationRequiredError{}, 0, "AuthenticationRequired", nil},
		{"transport", errors.New("Post \"" + testVault + "?token=" + secret + "\": EOF"), 0, "Unclassified", nil},
		{"deadline", fmt.Errorf("%s: %w", secret, context.DeadlineExceeded), 0, "Timeout", context.DeadlineExceeded},
		{"canceled", fmt.Errorf("%s: %w", secret, context.Canceled), 0, "Canceled", context.Canceled},
	}
	for _, c := range cases {
		for _, op := range []string{"get key", "sign"} {
			t.Run(c.name+"/"+op, func(t *testing.T) {
				f := newFakeKV(t)
				var err error
				if op == "get key" {
					f.getErr = c.in
					_, err = open(t, f)
				} else {
					s := mustOpen(t, f)
					f.signErr = c.in
					_, err = s.Sign([]byte("m"))
				}
				var e *Error
				if !errors.As(err, &e) {
					t.Fatalf("err = %v, want *Error", err)
				}
				if e.Op != op || e.Status != c.wantStatus || e.Code != c.wantCode {
					t.Fatalf("got %+v, want op %s status %d code %s", e, op, c.wantStatus, c.wantCode)
				}
				if c.is != nil && !errors.Is(err, c.is) {
					t.Fatalf("errors.Is(%v, %v) is false", err, c.is)
				}
				if c.is == nil && errors.Unwrap(e) != nil {
					t.Fatal("a non-context error unwraps to the raw error")
				}
				for _, s := range allForms(err) {
					if strings.Contains(s, secret) || strings.Contains(s, "Bearer") || strings.Contains(s, "token=") {
						t.Fatalf("error leaks: %s", s)
					}
				}
			})
		}
	}
}

func TestSignerStringHasNoSecrets(t *testing.T) {
	s := mustOpen(t, newFakeKV(t))
	for _, str := range allForms(s) {
		if !strings.Contains(str, testKeyID) || !strings.Contains(str, "ecdsa-p256") {
			t.Errorf("String lacks the key identity: %s", str)
		}
	}
	for _, str := range allForms(*s) {
		_ = str // formatting a copy must not panic
	}
}

func TestOpenTimesOut(t *testing.T) {
	f := &blockingAPI{fakeKV: newFakeKV(t)}
	_, err := Open(context.Background(), Config{Client: f, KeyID: testKeyID, Timeout: 20 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

type blockingAPI struct{ *fakeKV }

func (b *blockingAPI) GetKey(ctx context.Context, _, _ string, _ *azkeys.GetKeyOptions) (azkeys.GetKeyResponse, error) {
	<-ctx.Done()
	return azkeys.GetKeyResponse{}, ctx.Err()
}

func TestSignTimesOut(t *testing.T) {
	f := newFakeKV(t)
	s := mustOpen(t, f)
	s.timeout = 20 * time.Millisecond
	s.client = blockingSign{f}
	if _, err := s.Sign([]byte("m")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

type blockingSign struct{ *fakeKV }

func (b blockingSign) Sign(ctx context.Context, _, _ string, _ azkeys.SignParameters, _ *azkeys.SignOptions) (azkeys.SignResponse, error) {
	<-ctx.Done()
	return azkeys.SignResponse{}, ctx.Err()
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func TestSameKeyID(t *testing.T) {
	ref := keyRef{vaultURL: testVault, name: "bide-k", version: testVersion}
	ok := func(s string) bool { id := azkeys.ID(s); return sameKeyID(&id, ref) }
	good := testVault + "/keys/bide-k/" + testVersion
	if !ok(good) || !ok(strings.ToUpper(good)) {
		t.Fatal("the pinned identifier, in any ASCII case, must match")
	}
	for name, s := range map[string]string{
		"kelvin sign":    testVault + "/keys/bide-\u212a/" + testVersion,
		"query":          good + "?x=1",
		"empty query":    good + "?",
		"fragment":       good + "#x",
		"userinfo":       "https://u@myvault.vault.azure.net/keys/bide-k/" + testVersion,
		"port":           "https://myvault.vault.azure.net:443/keys/bide-k/" + testVersion,
		"http":           "http://myvault.vault.azure.net/keys/bide-k/" + testVersion,
		"encoded":        testVault + "/keys/bide%2dk/" + testVersion,
		"trailing slash": good + "/",
		"other vault":    "https://other.vault.azure.net/keys/bide-k/" + testVersion,
		"no version":     testVault + "/keys/bide-k",
	} {
		if ok(s) {
			t.Errorf("%s: %q matched", name, s)
		}
	}
	// Unicode case folding must not equate a Kelvin sign with "k" in the host either.
	kref := keyRef{vaultURL: "https://kv.vault.azure.net", name: "n", version: testVersion}
	if kid := azkeys.ID("https://\u212av.vault.azure.net/keys/n/" + testVersion); sameKeyID(&kid, kref) {
		t.Error("a host with U+212A matched a host with k")
	}
	if sameKeyID(nil, ref) {
		t.Error("a nil identifier matched")
	}
}

func TestSignRefusesAbsentOrDecoratedKID(t *testing.T) {
	for name, kid := range map[string]*azkeys.ID{
		"nil":   nil,
		"query": to.Ptr(azkeys.ID(testKeyID + "?api-version=1")),
	} {
		f := newFakeKV(t)
		s := mustOpen(t, f)
		f.signKID, f.nilKID = kid, kid == nil
		if _, err := s.Sign([]byte("m")); !errors.Is(err, ErrKeyMismatch) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
