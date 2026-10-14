package gcpkms

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

const testVersion = "projects/bank-prod/locations/europe-west2/keyRings/bide/cryptoKeys/audit/cryptoKeyVersions/3"

// secret stands in for a credential or a token a failing client call might put in its error text.
const secret = "ya29.SECRET-TOKEN-0123456789"

// fake stands in for Cloud KMS. It holds one key and answers as Cloud KMS documents: it checks the
// request shape and the request checksums, and fills in the response integrity fields. The hooks
// let a test corrupt a response. No test calls Google Cloud.
type fake struct {
	ed  ed25519.PrivateKey
	ec  *ecdsa.PrivateKey
	ver *kmspb.CryptoKeyVersion
	pem string // overrides the PEM when set

	onVersion func(*kmspb.CryptoKeyVersion)
	onKey     func(*kmspb.PublicKey)
	onSign    func(*kmspb.AsymmetricSignResponse)
	signErr   error
	getErr    error
	pubErr    error

	signs   atomic.Int32
	mu      sync.Mutex
	lastCtx context.Context
	lastReq *kmspb.AsymmetricSignRequest
}

func spkiPEM(pub any) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func newEd(t *testing.T) *fake {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &fake{ed: priv, ver: &kmspb.CryptoKeyVersion{Name: testVersion, State: kmspb.CryptoKeyVersion_ENABLED,
		ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE, Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_ED25519}}
}

func newP256(t *testing.T, level kmspb.ProtectionLevel) *fake {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &fake{ec: k, ver: &kmspb.CryptoKeyVersion{Name: testVersion, State: kmspb.CryptoKeyVersion_ENABLED,
		ProtectionLevel: level, Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256}}
}

func crcOf(b []byte) *wrapperspb.Int64Value { return wrapperspb.Int64(int64(crc32c(b))) }

func (f *fake) GetCryptoKeyVersion(_ context.Context, req *kmspb.GetCryptoKeyVersionRequest, _ ...gax.CallOption) (*kmspb.CryptoKeyVersion, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if req.Name != testVersion {
		return nil, status.Error(codes.NotFound, "no such version "+req.Name)
	}
	v := &kmspb.CryptoKeyVersion{Name: f.ver.Name, State: f.ver.State, ProtectionLevel: f.ver.ProtectionLevel, Algorithm: f.ver.Algorithm}
	if f.onVersion != nil {
		f.onVersion(v)
	}
	return v, nil
}

func (f *fake) GetPublicKey(_ context.Context, req *kmspb.GetPublicKeyRequest, _ ...gax.CallOption) (*kmspb.PublicKey, error) {
	if f.pubErr != nil {
		return nil, f.pubErr
	}
	if req.Name != testVersion {
		return nil, status.Error(codes.NotFound, "no such version "+req.Name)
	}
	if req.PublicKeyFormat != kmspb.PublicKey_PEM {
		return nil, status.Error(codes.InvalidArgument, "the signer asks for PEM")
	}
	p := f.pem
	if p == "" {
		if f.ec != nil {
			p = spkiPEM(&f.ec.PublicKey)
		} else {
			p = spkiPEM(f.ed.Public())
		}
	}
	pk := &kmspb.PublicKey{Name: testVersion, Pem: p, PemCrc32C: crcOf([]byte(p)), Algorithm: f.ver.Algorithm, ProtectionLevel: f.ver.ProtectionLevel}
	if f.onKey != nil {
		f.onKey(pk)
	}
	return pk, nil
}

func (f *fake) AsymmetricSign(ctx context.Context, req *kmspb.AsymmetricSignRequest, _ ...gax.CallOption) (*kmspb.AsymmetricSignResponse, error) {
	f.signs.Add(1)
	f.mu.Lock()
	f.lastCtx, f.lastReq = ctx, req
	f.mu.Unlock()
	if f.signErr != nil {
		return nil, f.signErr
	}
	if req.Name != testVersion {
		return nil, status.Error(codes.NotFound, "the key version was not pinned: "+req.Name)
	}
	resp := &kmspb.AsymmetricSignResponse{Name: req.Name, ProtectionLevel: f.ver.ProtectionLevel}
	var sig []byte
	switch f.ver.Algorithm {
	case kmspb.CryptoKeyVersion_EC_SIGN_ED25519:
		if req.Digest != nil || len(req.Data) == 0 {
			return nil, status.Error(codes.InvalidArgument, "EC_SIGN_ED25519 signs the data field")
		}
		if req.DataCrc32C == nil || req.DataCrc32C.Value != int64(crc32c(req.Data)) {
			return nil, status.Error(codes.InvalidArgument, "data checksum mismatch")
		}
		resp.VerifiedDataCrc32C = true
		sig = ed25519.Sign(f.ed, req.Data)
	case kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256:
		d := req.GetDigest().GetSha256()
		if len(req.Data) != 0 || len(d) != sha256.Size {
			return nil, status.Error(codes.InvalidArgument, "EC_SIGN_P256_SHA256 signs a SHA-256 digest")
		}
		if req.DigestCrc32C == nil || req.DigestCrc32C.Value != int64(crc32c(d)) {
			return nil, status.Error(codes.InvalidArgument, "digest checksum mismatch")
		}
		resp.VerifiedDigestCrc32C = true
		var err error
		if sig, err = ecdsa.SignASN1(rand.Reader, f.ec, d); err != nil {
			return nil, err
		}
	}
	resp.Signature, resp.SignatureCrc32C = sig, crcOf(sig)
	if f.onSign != nil {
		f.onSign(resp)
	}
	return resp, nil
}

func open(t *testing.T, f *fake, mod ...func(*Config)) *Signer {
	t.Helper()
	cfg := Config{Client: f, KeyVersion: testVersion}
	if f.ec != nil {
		cfg.Scheme = audit.AlgECDSAP256
	}
	for _, m := range mod {
		m(&cfg)
	}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSignVerifiesUnderBideVerifier(t *testing.T) {
	for name, f := range map[string]*fake{"ed25519": newEd(t), "p256": newP256(t, kmspb.ProtectionLevel_HSM)} {
		t.Run(name, func(t *testing.T) {
			s := open(t, f)
			v, err := audit.VerifierOf(s)
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range []int{1, 60, 4096, 4097, 100000} {
				msg := bytes.Repeat([]byte{'x'}, n)
				sig, err := s.Sign(msg)
				if err != nil || !v.Verify(msg, sig) {
					t.Fatalf("%d bytes: %v", n, err)
				}
			}
			if f.signs.Load() != 6 { // the self-test, then five messages
				t.Fatalf("%d sign calls, want 6", f.signs.Load())
			}
		})
	}
}

func TestRequestShape(t *testing.T) {
	// The fake rejects a request with the wrong field or checksum; also check what was sent.
	f := newEd(t)
	s := open(t, f)
	msg := []byte("tree head")
	if _, err := s.Sign(msg); err != nil {
		t.Fatal(err)
	}
	r := f.lastReq
	if r.Name != testVersion || !bytes.Equal(r.Data, msg) || r.Digest != nil || r.DataCrc32C.Value != int64(crc32c(msg)) || r.DigestCrc32C != nil {
		t.Fatalf("request %+v", r)
	}
	p := newP256(t, kmspb.ProtectionLevel_HSM)
	sp := open(t, p)
	if _, err := sp.Sign(msg); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(msg)
	r = p.lastReq
	if r.Name != testVersion || len(r.Data) != 0 || !bytes.Equal(r.Digest.GetSha256(), h[:]) || r.DigestCrc32C.Value != int64(crc32c(h[:])) || r.DataCrc32C != nil {
		t.Fatalf("request %+v", r)
	}
}

func TestOpenRefusesOtherKeys(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(ctx, Config{Client: newP256(t, kmspb.ProtectionLevel_HSM), KeyVersion: testVersion}); !errors.Is(err, ErrNoEd25519) {
		t.Fatalf("P-256 version under Ed25519: %v", err)
	}
	if _, err := Open(ctx, Config{Client: newEd(t), KeyVersion: testVersion, Scheme: audit.AlgECDSAP256}); !errors.Is(err, ErrNoP256) {
		t.Fatalf("Ed25519 version under P-256: %v", err)
	}
	for _, alg := range []kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm{
		kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384, kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256,
		kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, kmspb.CryptoKeyVersion_EC_SIGN_SECP256K1_SHA256,
		kmspb.CryptoKeyVersion_CRYPTO_KEY_VERSION_ALGORITHM_UNSPECIFIED, kmspb.CryptoKeyVersion_HMAC_SHA256,
	} {
		for _, sc := range []audit.Alg{audit.AlgEd25519, audit.AlgECDSAP256} {
			f := newEd(t)
			f.onVersion = func(v *kmspb.CryptoKeyVersion) { v.Algorithm = alg }
			if _, err := Open(ctx, Config{Client: f, KeyVersion: testVersion, Scheme: sc}); err == nil {
				t.Errorf("%s accepted under %s", alg, sc)
			}
		}
	}
}

func TestOpenRefusesStates(t *testing.T) {
	for _, st := range []kmspb.CryptoKeyVersion_CryptoKeyVersionState{
		kmspb.CryptoKeyVersion_DISABLED, kmspb.CryptoKeyVersion_DESTROYED, kmspb.CryptoKeyVersion_DESTROY_SCHEDULED,
		kmspb.CryptoKeyVersion_PENDING_GENERATION, kmspb.CryptoKeyVersion_PENDING_IMPORT, kmspb.CryptoKeyVersion_IMPORT_FAILED,
		kmspb.CryptoKeyVersion_GENERATION_FAILED, kmspb.CryptoKeyVersion_CRYPTO_KEY_VERSION_STATE_UNSPECIFIED,
	} {
		f := newEd(t)
		f.ver.State = st
		_, err := Open(context.Background(), Config{Client: f, KeyVersion: testVersion})
		if !errors.Is(err, ErrVersionState) {
			t.Errorf("state %s: err = %v", st, err)
		}
		if f.signs.Load() != 0 {
			t.Errorf("state %s: signed", st)
		}
	}
}

func TestRequireHSM(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		level kmspb.ProtectionLevel
		ok    bool
	}{
		{kmspb.ProtectionLevel_HSM, true}, {kmspb.ProtectionLevel_HSM_SINGLE_TENANT, true},
		{kmspb.ProtectionLevel_SOFTWARE, false}, {kmspb.ProtectionLevel_EXTERNAL, false}, {kmspb.ProtectionLevel_EXTERNAL_VPC, false},
	} {
		f := newP256(t, tc.level)
		s, err := Open(ctx, Config{Client: f, KeyVersion: testVersion, Scheme: audit.AlgECDSAP256, RequireHSM: true})
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, ErrNotHSM)) {
			t.Errorf("%s: err = %v", tc.level, err)
		}
		if tc.ok && s.ProtectionLevel() != tc.level.String() {
			t.Errorf("ProtectionLevel = %s", s.ProtectionLevel())
		}
		if !tc.ok && f.signs.Load() != 0 {
			t.Errorf("%s: signed before refusing", tc.level)
		}
		// Without RequireHSM every level opens.
		if _, err := Open(ctx, Config{Client: newP256(t, tc.level), KeyVersion: testVersion, Scheme: audit.AlgECDSAP256}); err != nil {
			t.Errorf("%s without RequireHSM: %v", tc.level, err)
		}
	}
	// Google offers Ed25519 at SOFTWARE only, so RequireHSM can never open an Ed25519 version.
	if _, err := Open(ctx, Config{Client: newEd(t), KeyVersion: testVersion, RequireHSM: true}); !errors.Is(err, ErrNotHSM) {
		t.Fatalf("Ed25519 with RequireHSM: %v", err)
	}
	// An unspecified level is not trusted.
	f := newP256(t, kmspb.ProtectionLevel_PROTECTION_LEVEL_UNSPECIFIED)
	if _, err := Open(ctx, Config{Client: f, KeyVersion: testVersion, Scheme: audit.AlgECDSAP256}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unspecified protection level: %v", err)
	}
}

func TestOpenConfig(t *testing.T) {
	ctx := context.Background()
	f := newEd(t)
	for name, cfg := range map[string]Config{
		"no client":      {KeyVersion: testVersion},
		"no version":     {Client: f},
		"bad scheme":     {Client: f, KeyVersion: testVersion, Scheme: "rsa"},
		"latest alias":   {Client: f, KeyVersion: "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/latest"},
		"crypto key":     {Client: f, KeyVersion: "projects/p/locations/l/keyRings/r/cryptoKeys/k"},
		"version zero":   {Client: f, KeyVersion: "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/0"},
		"leading zero":   {Client: f, KeyVersion: "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/01"},
		"trailing path":  {Client: f, KeyVersion: testVersion + "/extra"},
		"trailing nl":    {Client: f, KeyVersion: testVersion + "\n"},
		"leading space":  {Client: f, KeyVersion: " " + testVersion},
		"embedded query": {Client: f, KeyVersion: testVersion + "?x=1"},
		"relative":       {Client: f, KeyVersion: "keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"},
		"wildcard":       {Client: f, KeyVersion: "projects/-/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1 "},
	} {
		if _, err := Open(ctx, cfg); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if f.signs.Load() != 0 {
		t.Error("a bad config reached Cloud KMS")
	}
	// A project ID with a domain prefix is valid.
	g := newEd(t)
	g.ver.Name = "projects/example.com:proj/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/12"
	if !versionName.MatchString(g.ver.Name) {
		t.Error("a domain-scoped project ID is refused")
	}
}

// Every response must name the version it was asked about.
func TestResponseForAnotherResourceRefused(t *testing.T) {
	ctx := context.Background()
	const other = "projects/bank-prod/locations/europe-west2/keyRings/bide/cryptoKeys/audit/cryptoKeyVersions/4"
	f := newEd(t)
	f.onVersion = func(v *kmspb.CryptoKeyVersion) { v.Name = other }
	if _, err := Open(ctx, Config{Client: f, KeyVersion: testVersion}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("version: %v", err)
	}
	f = newEd(t)
	f.onKey = func(p *kmspb.PublicKey) { p.Name = other }
	if _, err := Open(ctx, Config{Client: f, KeyVersion: testVersion}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("public key: %v", err)
	}
	f = newEd(t)
	f.onKey = func(p *kmspb.PublicKey) { p.Name = "" }
	if _, err := Open(ctx, Config{Client: f, KeyVersion: testVersion}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("public key without name: %v", err)
	}
	f = newEd(t)
	f.onSign = func(r *kmspb.AsymmetricSignResponse) { r.Name = other }
	if _, err := Open(ctx, Config{Client: f, KeyVersion: testVersion}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("sign: %v", err)
	}
	if f.signs.Load() != 1 {
		t.Errorf("a wrong name was retried: %d calls", f.signs.Load())
	}
}

func TestPublicKeyIntegrity(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(*kmspb.PublicKey){
		"no checksum":    func(p *kmspb.PublicKey) { p.PemCrc32C = nil },
		"wrong checksum": func(p *kmspb.PublicKey) { p.PemCrc32C = wrapperspb.Int64(p.PemCrc32C.Value ^ 1) },
		"wrapped checksum": func(p *kmspb.PublicKey) { // the right checksum plus 2^32 is not the checksum
			p.PemCrc32C = wrapperspb.Int64(p.PemCrc32C.Value + 1<<32)
		},
		"pem altered": func(p *kmspb.PublicKey) { p.Pem += "\n" },
		"other level": func(p *kmspb.PublicKey) { p.ProtectionLevel = kmspb.ProtectionLevel_HSM },
	}
	for name, mut := range cases {
		f := newEd(t)
		f.onKey = mut
		if _, err := Open(ctx, Config{Client: f, KeyVersion: testVersion}); !errors.Is(err, ErrIntegrity) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	f := newEd(t)
	f.onKey = func(p *kmspb.PublicKey) { p.Algorithm = kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256 }
	if _, err := Open(ctx, Config{Client: f, KeyVersion: testVersion}); !errors.Is(err, ErrNoEd25519) {
		t.Errorf("public key algorithm: %v", err)
	}
}

func TestPublicKeyShapeRefused(t *testing.T) {
	ctx := context.Background()
	ed, _, _ := ed25519.GenerateKey(nil)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	good := spkiPEM(ed)
	der, _ := x509.MarshalPKIXPublicKey(ed)
	for name, tc := range map[string]struct {
		pem    string
		p256   bool
		sentin error
	}{
		"empty":             {"\n", false, ErrNoEd25519},
		"not pem":           {"hello", false, ErrNoEd25519},
		"wrong block type":  {string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), false, ErrNoEd25519},
		"trailing garbage":  {good + "junk", false, ErrNoEd25519},
		"second block":      {good + good, false, ErrNoEd25519},
		"garbage der":       {string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{1, 2, 3}})), false, ErrNoEd25519},
		"p256 key for ed":   {spkiPEM(&p256.PublicKey), false, ErrNoEd25519},
		"ed key for p256":   {good, true, ErrNoP256},
		"p384 key for p256": {spkiPEM(&p384.PublicKey), true, ErrNoP256},
	} {
		var f *fake
		var cfg Config
		if tc.p256 {
			f, cfg = newP256(t, kmspb.ProtectionLevel_HSM), Config{Scheme: audit.AlgECDSAP256}
		} else {
			f = newEd(t)
		}
		f.pem = tc.pem
		cfg.Client, cfg.KeyVersion = f, testVersion
		if _, err := Open(ctx, cfg); !errors.Is(err, tc.sentin) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Trailing whitespace after the block is how PEM is usually written and is accepted.
	f := newEd(t)
	f.pem = spkiPEM(f.ed.Public()) + "\n\n"
	if _, err := Open(ctx, Config{Client: f, KeyVersion: testVersion}); err != nil {
		t.Errorf("trailing newlines: %v", err)
	}
}

func TestSignResponseIntegrity(t *testing.T) {
	cases := map[string]func(*kmspb.AsymmetricSignResponse){
		"request checksum not verified": func(r *kmspb.AsymmetricSignResponse) { r.VerifiedDataCrc32C, r.VerifiedDigestCrc32C = false, false },
		"no signature checksum":         func(r *kmspb.AsymmetricSignResponse) { r.SignatureCrc32C = nil },
		"protection level changed":      func(r *kmspb.AsymmetricSignResponse) { r.ProtectionLevel = kmspb.ProtectionLevel_EXTERNAL },
		"protection level absent":       func(r *kmspb.AsymmetricSignResponse) { r.ProtectionLevel = 0 },
	}
	for _, mk := range []func() *fake{func() *fake { return newEd(t) }, func() *fake { return newP256(t, kmspb.ProtectionLevel_HSM) }} {
		for name, mut := range cases {
			f := mk()
			s := open(t, f)
			f.onSign = mut
			before := f.signs.Load()
			if sig, err := s.Sign([]byte("m")); !errors.Is(err, ErrIntegrity) {
				t.Errorf("%s: sig %x, err = %v", name, sig, err)
			}
			if f.signs.Load()-before != 1 {
				t.Errorf("%s: retried %d times, want one call", name, f.signs.Load()-before)
			}
		}
	}
}

func TestSignatureChecksumMismatchIsRetried(t *testing.T) {
	for _, mk := range []func() *fake{func() *fake { return newEd(t) }, func() *fake { return newP256(t, kmspb.ProtectionLevel_HSM) }} {
		f := mk()
		s := open(t, f)
		v, _ := audit.VerifierOf(s)

		// Corrupt the checksum of the first two answers: the third attempt succeeds.
		var n atomic.Int32
		f.onSign = func(r *kmspb.AsymmetricSignResponse) {
			if n.Add(1) <= 2 {
				r.SignatureCrc32C = wrapperspb.Int64(r.SignatureCrc32C.Value ^ 0x10)
			}
		}
		before := f.signs.Load()
		msg := []byte("m")
		sig, err := s.Sign(msg)
		if err != nil || !v.Verify(msg, sig) || f.signs.Load()-before != 3 {
			t.Fatalf("sig ok %v, err %v, %d calls", err == nil && v.Verify(msg, sig), err, f.signs.Load()-before)
		}

		// A mismatch that persists stops after three attempts and is an integrity error.
		f.onSign = func(r *kmspb.AsymmetricSignResponse) {
			r.SignatureCrc32C = wrapperspb.Int64(r.SignatureCrc32C.Value ^ 0x10)
		}
		before = f.signs.Load()
		if _, err := s.Sign(msg); !errors.Is(err, ErrIntegrity) || f.signs.Load()-before != 3 {
			t.Fatalf("err %v after %d calls", err, f.signs.Load()-before)
		}
	}
}

// The checksum protects against corruption; a signature that is intact but wrong, by another key or
// for another message, is refused by the verification against the public key.
func TestSignRefusesWrongSignatures(t *testing.T) {
	f := newEd(t)
	s := open(t, f)
	_, other, _ := ed25519.GenerateKey(nil)
	f.onSign = func(r *kmspb.AsymmetricSignResponse) {
		r.Signature = ed25519.Sign(other, []byte("m"))
		r.SignatureCrc32C = crcOf(r.Signature)
	}
	if _, err := s.Sign([]byte("m")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("replaced Ed25519 key: %v", err)
	}
	f.onSign = func(r *kmspb.AsymmetricSignResponse) {
		r.Signature = ed25519.Sign(f.ed, []byte("another message"))
		r.SignatureCrc32C = crcOf(r.Signature)
	}
	if _, err := s.Sign([]byte("m")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("signature of another message: %v", err)
	}

	p := newP256(t, kmspb.ProtectionLevel_HSM)
	sp := open(t, p)
	otherEC, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p.onSign = func(r *kmspb.AsymmetricSignResponse) {
		h := sha256.Sum256([]byte("m"))
		r.Signature, _ = ecdsa.SignASN1(rand.Reader, otherEC, h[:])
		r.SignatureCrc32C = crcOf(r.Signature)
	}
	if _, err := sp.Sign([]byte("m")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("replaced P-256 key: %v", err)
	}
}

func TestP256BadDERRefused(t *testing.T) {
	derOf := func(r, s *big.Int) []byte {
		b, _ := asn1.Marshal(struct{ R, S *big.Int }{r, s})
		return b
	}
	muts := map[string]func(good []byte) []byte{
		"trailing":  func(d []byte) []byte { return append(bytes.Clone(d), 0) },
		"truncated": func(d []byte) []byte { return d[:len(d)-1] },
		"empty":     func([]byte) []byte { return nil },
		"zero r":    func([]byte) []byte { return derOf(big.NewInt(0), big.NewInt(1)) },
		"negative":  func([]byte) []byte { return derOf(big.NewInt(-5), big.NewInt(1)) },
		"r = n":     func([]byte) []byte { return derOf(elliptic.P256().Params().N, big.NewInt(1)) },
		"padded": func(d []byte) []byte {
			var sig struct{ R, S *big.Int }
			asn1.Unmarshal(d, &sig)
			rb := append([]byte{0, 0}, sig.R.Bytes()...)
			sb := sig.S.Bytes()
			body := append(append([]byte{0x02, byte(len(rb))}, rb...), append([]byte{0x02, byte(len(sb))}, sb...)...)
			return append([]byte{0x30, byte(len(body))}, body...)
		},
	}
	for name, mut := range muts {
		f := newP256(t, kmspb.ProtectionLevel_HSM)
		s := open(t, f)
		f.onSign = func(r *kmspb.AsymmetricSignResponse) {
			r.Signature = mut(r.Signature)
			r.SignatureCrc32C = crcOf(r.Signature) // intact in transit, malformed at the source
		}
		if sig, err := s.Sign([]byte("m")); err == nil || !errors.Is(err, audit.ErrMalformed) {
			t.Errorf("%s: sig %x, err = %v", name, sig, err)
		}
	}
}

// Cloud KMS may return the high-S form of an ECDSA signature; audit verifies the low-S form.
func TestP256HighSIsNormalised(t *testing.T) {
	f := newP256(t, kmspb.ProtectionLevel_HSM)
	s := open(t, f)
	v, _ := audit.VerifierOf(s)
	n := elliptic.P256().Params().N
	f.onSign = func(r *kmspb.AsymmetricSignResponse) {
		var sig struct{ R, S *big.Int }
		asn1.Unmarshal(r.Signature, &sig)
		if sig.S.Cmp(new(big.Int).Rsh(n, 1)) <= 0 {
			sig.S = new(big.Int).Sub(n, sig.S)
		}
		r.Signature, _ = asn1.Marshal(sig)
		r.SignatureCrc32C = crcOf(r.Signature)
	}
	for range 20 {
		msg := []byte("high s")
		sig, err := s.Sign(msg)
		if err != nil || len(sig) != 64 || !v.Verify(msg, sig) {
			t.Fatalf("err %v", err)
		}
	}
}

// The signature a caller receives is its own: changing it does not change what the client returned.
func TestSignReturnsACopy(t *testing.T) {
	f := newEd(t)
	s := open(t, f)
	var held []byte
	f.onSign = func(r *kmspb.AsymmetricSignResponse) { held = r.Signature }
	sig, err := s.Sign([]byte("m"))
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Clone(held)
	sig[0] ^= 0xff
	if !bytes.Equal(held, want) {
		t.Fatal("Sign returned the response's own slice")
	}
}

func TestEmptyMessage(t *testing.T) {
	f := newEd(t)
	s := open(t, f)
	before := f.signs.Load()
	if _, err := s.Sign(nil); !errors.Is(err, ErrEmptyMessage) {
		t.Fatalf("Ed25519: %v", err)
	}
	if f.signs.Load() != before {
		t.Fatal("an empty message reached Cloud KMS")
	}
	// Under P-256 the digest of the empty message is a 32-byte digest like any other.
	sp := open(t, newP256(t, kmspb.ProtectionLevel_HSM))
	v, _ := audit.VerifierOf(sp)
	sig, err := sp.Sign(nil)
	if err != nil || !v.Verify(nil, sig) {
		t.Fatalf("P-256: %v", err)
	}
}

// A message is signed with the version Open pinned. The fake errors on any other name.
func TestSignPinsTheVersion(t *testing.T) {
	f := newEd(t)
	s := open(t, f)
	if s.KeyVersion() != testVersion {
		t.Fatalf("KeyVersion = %s", s.KeyVersion())
	}
	if _, err := s.Sign([]byte("m")); err != nil {
		t.Fatal(err)
	}
	if f.lastReq.Name != testVersion {
		t.Fatalf("signed with %s", f.lastReq.Name)
	}
}

// What a failing Cloud KMS call put in its error text must not reach the caller: only the status code does.
func TestErrorsCarryOnlyTheCode(t *testing.T) {
	leaky := []error{
		status.Error(codes.PermissionDenied, "oauth2: cannot fetch token: 401 Unauthorized Response: {\"access_token\":\""+secret+"\"}"),
		fmt.Errorf("transport: Bearer %s rejected", secret),
		fmt.Errorf("wrapped: %w", status.Error(codes.Unauthenticated, secret)),
	}
	wantCode := []codes.Code{codes.PermissionDenied, codes.Unknown, codes.Unauthenticated}
	for i, leak := range leaky {
		check := func(where string, err error) {
			t.Helper()
			if err == nil {
				t.Fatalf("%s: no error", where)
			}
			for _, out := range []string{err.Error(), fmt.Sprintf("%v %+v %#v %q", err, err, err, err.Error())} {
				if strings.Contains(out, secret) || strings.Contains(out, "Bearer") || strings.Contains(out, "access_token") {
					t.Fatalf("%s: the error leaks: %s", where, out)
				}
			}
			for e := error(err); e != nil; e = errors.Unwrap(e) {
				if strings.Contains(fmt.Sprintf("%#v", e), secret) {
					t.Fatalf("%s: the chain leaks: %#v", where, e)
				}
			}
			if !errors.Is(err, CodeError{Code: wantCode[i]}) {
				t.Fatalf("%s: err = %v, want code %s", where, err, wantCode[i])
			}
		}
		f := newEd(t)
		f.getErr = leak
		_, err := Open(context.Background(), Config{Client: f, KeyVersion: testVersion})
		check("get version", err)
		f = newEd(t)
		f.pubErr = leak
		_, err = Open(context.Background(), Config{Client: f, KeyVersion: testVersion})
		check("get public key", err)
		f = newEd(t)
		_, err = Open(context.Background(), Config{Client: &lateFail{f, leak}, KeyVersion: testVersion})
		check("self-test sign", err)
		f = newEd(t)
		s := open(t, f)
		f.signErr = leak
		_, err = s.Sign([]byte("m"))
		check("sign", err)
	}
}

type lateFail struct {
	*fake
	err error
}

func (l *lateFail) AsymmetricSign(context.Context, *kmspb.AsymmetricSignRequest, ...gax.CallOption) (*kmspb.AsymmetricSignResponse, error) {
	return nil, l.err
}

func TestCodeErrorMessage(t *testing.T) {
	if got := (CodeError{Code: codes.PermissionDenied}).Error(); got != "gcpkms: Cloud KMS returned PermissionDenied" {
		t.Fatal(got)
	}
	f := newEd(t)
	s := open(t, f)
	f.signErr = status.Error(codes.ResourceExhausted, "quota")
	if _, err := s.Sign([]byte("m")); !errors.Is(err, CodeError{Code: codes.ResourceExhausted}) || errors.Is(err, CodeError{Code: codes.NotFound}) {
		t.Fatalf("err = %v", err)
	}
}

func TestContextErrorsKeepTheirSentinel(t *testing.T) {
	f := newEd(t)
	s := open(t, f)
	for _, tc := range []struct {
		in   error
		want error
	}{
		{status.Error(codes.DeadlineExceeded, "slow"), context.DeadlineExceeded},
		{status.Error(codes.Canceled, "gone"), context.Canceled},
		{context.DeadlineExceeded, context.DeadlineExceeded},
		{fmt.Errorf("rpc: %w", context.Canceled), context.Canceled},
	} {
		f.signErr = tc.in
		if _, err := s.Sign([]byte("m")); !errors.Is(err, tc.want) {
			t.Errorf("%v: err = %v", tc.in, err)
		}
	}
}

func TestTimeout(t *testing.T) {
	f := newEd(t)
	s, err := Open(context.Background(), Config{Client: f, KeyVersion: testVersion, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	s.Sign([]byte("m"))
	dl, ok := f.lastCtx.Deadline()
	if !ok || time.Until(dl) > 3*time.Second {
		t.Fatalf("call deadline %v %v", dl, ok)
	}
	s2 := open(t, f)
	s2.Sign([]byte("m"))
	if dl, ok := f.lastCtx.Deadline(); !ok || time.Until(dl) <= 3*time.Second || time.Until(dl) > DefaultTimeout {
		t.Fatalf("default timeout not applied: %v %v", dl, ok)
	}
}

func TestOpenHonoursTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := newEd(t)
	f.getErr = ctx.Err()
	if _, err := Open(ctx, Config{Client: f, KeyVersion: testVersion}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

type ctxKey struct{}

// Open's context reaches every call it makes, the self-test signature included.
func TestOpenPassesItsContextToTheSelfTest(t *testing.T) {
	f := newEd(t)
	ctx := context.WithValue(context.Background(), ctxKey{}, "open")
	if _, err := Open(ctx, Config{Client: f, KeyVersion: testVersion}); err != nil {
		t.Fatal(err)
	}
	if f.lastCtx.Value(ctxKey{}) != "open" {
		t.Fatal("the self-test signature ran under another context")
	}
}

func TestFormatShowsNoConfiguration(t *testing.T) {
	s := open(t, newEd(t))
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%s", s)} {
		if !strings.HasPrefix(out, "gcpkms.Signer{"+testVersion+" ed25519:") || strings.Contains(out, "Client") {
			t.Fatalf("String = %s", out)
		}
	}
}

func TestPublicKeyIsACopy(t *testing.T) {
	s := open(t, newEd(t))
	a := s.PublicKey()
	a[0] ^= 0xff
	if bytes.Equal(a, s.PublicKey()) {
		t.Fatal("PublicKey returns the internal slice")
	}
	if s.Alg() != audit.AlgEd25519 || len(s.PublicKey()) != 32 {
		t.Fatalf("Alg %s, %d bytes", s.Alg(), len(s.PublicKey()))
	}
	sp := open(t, newP256(t, kmspb.ProtectionLevel_HSM))
	if sp.Alg() != audit.AlgECDSAP256 || len(sp.PublicKey()) != 65 {
		t.Fatalf("Alg %s, %d bytes", sp.Alg(), len(sp.PublicKey()))
	}
}

func TestConcurrentSign(t *testing.T) {
	for _, f := range []*fake{newEd(t), newP256(t, kmspb.ProtectionLevel_HSM)} {
		s := open(t, f)
		v, _ := audit.VerifierOf(s)
		var wg sync.WaitGroup
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				msg := []byte(fmt.Sprintf("message %d", i))
				if sig, err := s.Sign(msg); err != nil || !v.Verify(msg, sig) {
					t.Errorf("%d: %v", i, err)
				}
			}()
		}
		wg.Wait()
	}
}

func anchorRun(t *testing.T, s *Signer, runID string, tools int) (audit.Verifier, *agent.Journal, *audit.MemAnchorLog) {
	t.Helper()
	anchor := audit.NewMemAnchorLog()
	store, err := audit.NewAuditedStore(agent.NewMemStore(), s, anchor)
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(store)
	refund := agent.MustFunc("refund", "refund a payment", func(context.Context, struct{}) (string, error) { return "refunded", nil })
	var turns []agenttest.ScriptedTurn
	for i := range tools {
		turns = append(turns, agenttest.ToolTurn(fmt.Sprintf("call-%d", i), "refund", `{}`))
	}
	turns = append(turns, agenttest.TextTurn("done"))
	a := agenttest.MustNew(agenttest.NewScriptedModel(turns...), j, agent.WithTools(refund))
	if _, err := a.Run(context.Background(), runID, agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	v, err := audit.ParsePublicKey(audit.FormatPublicKey(s.Alg(), s.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	return v, j, anchor
}

// Cloud KMS signs tree heads and evidence seals; they verify with the public key alone.
func TestAuditedStoreAndEvidenceThroughKMS(t *testing.T) {
	for name, f := range map[string]*fake{"ed25519": newEd(t), "p256": newP256(t, kmspb.ProtectionLevel_HSM)} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := open(t, f)
			v, j, anchor := anchorRun(t, s, "run-kms", 12)
			entries := anchor.Entries()
			if len(entries) == 0 {
				t.Fatal("nothing anchored")
			}
			root, err := audit.Root(ctx, j, "run-kms")
			if err != nil {
				t.Fatal(err)
			}
			sth := entries[len(entries)-1].STH
			if err := sth.Verify(v); err != nil || !bytes.Equal(sth.Root, root) {
				t.Fatalf("anchored head: verify %v, root match %v", err, bytes.Equal(sth.Root, root))
			}
			ev, err := audit.Evidence(ctx, j, "run-kms", s, sth.TimestampNanos, audit.WithAllToolCalls())
			if err != nil {
				t.Fatal(err)
			}
			if rep, err := ev.Verify(v); err != nil || !rep.OK {
				t.Fatalf("%+v %v", rep, err)
			}
		})
	}
}
