package awskms

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
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

const testARN = "arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

// fakeKMS stands in for KMS. It holds an Ed25519 key and answers as KMS does: it resolves an alias
// to the key's ARN, accepts only ED25519_SHA_512 over a RAW message of at most 4096 bytes, and
// signs with the key. No test calls AWS.
type fakeKMS struct {
	priv     ed25519.PrivateKey
	spec     types.KeySpec
	usage    types.KeyUsageType
	algs     []types.SigningAlgorithmSpec
	der      []byte // overrides the public key DER when set
	signs    atomic.Int32
	signErr  error
	wrongKey ed25519.PrivateKey // sign with this key instead (a replaced key)
	lastCtx  context.Context
	p256     *ecdsa.PrivateKey // when set, the fake is an ECC_NIST_P256 key
	highS    bool              // return a high-S signature, as KMS may
}

func newFake(t *testing.T) *fakeKMS {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeKMS{priv: priv, spec: types.KeySpecEccNistEdwards25519, usage: types.KeyUsageTypeSignVerify,
		algs: []types.SigningAlgorithmSpec{types.SigningAlgorithmSpecEd25519Sha512, types.SigningAlgorithmSpecEd25519PhSha512}}
}

func (f *fakeKMS) GetPublicKey(_ context.Context, in *kms.GetPublicKeyInput, _ ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
	if *in.KeyId != testARN && *in.KeyId != "alias/bide-audit" {
		return nil, errors.New("NotFoundException")
	}
	der := f.der
	if f.p256 != nil {
		d, _ := x509.MarshalPKIXPublicKey(&f.p256.PublicKey)
		arn := testARN
		return &kms.GetPublicKeyOutput{KeyId: &arn, KeySpec: types.KeySpecEccNistP256, KeyUsage: f.usage,
			SigningAlgorithms: []types.SigningAlgorithmSpec{types.SigningAlgorithmSpecEcdsaSha256}, PublicKey: d}, nil
	}
	if der == nil {
		der, _ = x509.MarshalPKIXPublicKey(f.priv.Public())
	}
	arn := testARN
	return &kms.GetPublicKeyOutput{KeyId: &arn, KeySpec: f.spec, KeyUsage: f.usage, SigningAlgorithms: f.algs, PublicKey: der}, nil
}

func (f *fakeKMS) Sign(ctx context.Context, in *kms.SignInput, _ ...func(*kms.Options)) (*kms.SignOutput, error) {
	f.lastCtx = ctx
	f.signs.Add(1)
	switch {
	case f.signErr != nil:
		return nil, f.signErr
	case f.p256 != nil:
		if in.SigningAlgorithm != types.SigningAlgorithmSpecEcdsaSha256 || in.MessageType != types.MessageTypeDigest || len(in.Message) != sha256.Size {
			return nil, errors.New("ValidationException: ECDSA_SHA_256 needs a 32-byte DIGEST")
		}
		r, sv, _ := ecdsa.Sign(rand.Reader, f.p256, in.Message)
		if f.highS == (sv.Cmp(new(big.Int).Rsh(elliptic.P256().Params().N, 1)) <= 0) {
			sv = new(big.Int).Sub(elliptic.P256().Params().N, sv)
		}
		der, _ := asn1.Marshal(struct{ R, S *big.Int }{r, sv})
		return &kms.SignOutput{Signature: der}, nil
	case *in.KeyId != testARN:
		return nil, errors.New("signing with " + *in.KeyId + ": the key was not resolved to its ARN")
	case in.SigningAlgorithm != types.SigningAlgorithmSpecEd25519Sha512:
		return nil, errors.New("ValidationException: algorithm " + string(in.SigningAlgorithm))
	case in.MessageType != types.MessageTypeRaw:
		return nil, errors.New("ValidationException: ED25519_SHA_512 requires MessageType RAW")
	case len(in.Message) > MaxMessageSize:
		return nil, errors.New("ValidationException: message too long")
	}
	key := f.priv
	if f.wrongKey != nil {
		key = f.wrongKey
	}
	return &kms.SignOutput{Signature: ed25519.Sign(key, in.Message)}, nil
}

func open(t *testing.T, f *fakeKMS) *Signer {
	t.Helper()
	s, err := Open(context.Background(), Config{Client: f, KeyID: "alias/bide-audit"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSignVerifiesUnderBideVerifier(t *testing.T) {
	f := newFake(t)
	s := open(t, f)
	if s.Alg() != audit.AlgEd25519 || s.KeyARN() != testARN {
		t.Fatalf("Alg %s, ARN %s", s.Alg(), s.KeyARN())
	}
	v, err := audit.VerifierOf(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, 100, MaxMessageSize} {
		msg := bytes.Repeat([]byte{'x'}, n)
		sig, err := s.Sign(msg)
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		if !v.Verify(msg, sig) {
			t.Fatalf("audit verifier rejects the KMS signature over %d bytes", n)
		}
	}
	if v.Verify([]byte("other"), mustSign(t, s, []byte("m"))) {
		t.Fatal("verified over another message")
	}
}

func mustSign(t *testing.T, s *Signer, m []byte) []byte {
	t.Helper()
	sig, err := s.Sign(m)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func TestOversizeMessageNeverReachesKMS(t *testing.T) {
	f := newFake(t)
	s := open(t, f)
	before := f.signs.Load()
	_, err := s.Sign(make([]byte, MaxMessageSize+1))
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("err = %v, want ErrMessageTooLarge", err)
	}
	if f.signs.Load() != before {
		t.Fatal("an oversize message was sent to KMS")
	}
}

func TestOpenRefusesOtherKeys(t *testing.T) {
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand32())
	p256der, _ := x509.MarshalPKIXPublicKey(&p256.PublicKey)
	for name, mut := range map[string]func(*fakeKMS){
		"P-256 key spec":     func(f *fakeKMS) { f.spec = types.KeySpecEccNistP256; f.der = p256der },
		"ML-DSA-65 key spec": func(f *fakeKMS) { f.spec = types.KeySpecMlDsa65 },
		"encrypt usage":      func(f *fakeKMS) { f.usage = types.KeyUsageTypeEncryptDecrypt },
		"no Ed25519 algo":    func(f *fakeKMS) { f.algs = []types.SigningAlgorithmSpec{types.SigningAlgorithmSpecEcdsaSha256} },
		"P-256 public key":   func(f *fakeKMS) { f.der = p256der },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			mut(f)
			if _, err := Open(context.Background(), Config{Client: f, KeyID: testARN}); !errors.Is(err, ErrNoEd25519) {
				t.Fatalf("err = %v, want ErrNoEd25519", err)
			}
		})
	}
}

func rand32() *zeroReader { return &zeroReader{} }

// zeroReader is a deterministic source for key generation in tests.
type zeroReader struct{ n byte }

func (r *zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		r.n = r.n*31 + 7
		p[i] = r.n
	}
	return len(p), nil
}

func TestOpenErrors(t *testing.T) {
	f := newFake(t)
	for name, c := range map[string]Config{"no client": {KeyID: "k"}, "no key": {Client: f}} {
		if _, err := Open(context.Background(), c); !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if _, err := Open(context.Background(), Config{Client: f, KeyID: "alias/other"}); err == nil || !strings.Contains(err.Error(), "NotFoundException") {
		t.Fatalf("unknown key: %v", err)
	}
	f.der = []byte("not der")
	if _, err := Open(context.Background(), Config{Client: f, KeyID: testARN}); err == nil {
		t.Fatal("garbage public key accepted")
	}
	// The identity point is a small-order key and is no verification key.
	f.der = nil
	idp := make([]byte, 32)
	idp[0] = 1
	f.der = append([]byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00}, idp...)
	if _, err := Open(context.Background(), Config{Client: f, KeyID: testARN}); !errors.Is(err, audit.ErrMalformed) {
		t.Fatalf("small-order key: err = %v", err)
	}
}

func TestSignPinsTheKeyARNNotTheAlias(t *testing.T) {
	// The fake refuses to sign unless it is addressed by ARN, so this fails if the alias is used.
	s := open(t, newFake(t))
	if _, err := s.Sign([]byte("m")); err != nil {
		t.Fatal(err)
	}
}

func TestSignRefusesAReplacedKey(t *testing.T) {
	f := newFake(t)
	s := open(t, f)
	_, other, _ := ed25519.GenerateKey(nil)
	f.wrongKey = other
	if sig, err := s.Sign([]byte("m")); err == nil {
		t.Fatalf("returned a signature the public key does not verify: %x", sig)
	}
}

func TestSignErrorIsReturned(t *testing.T) {
	f := newFake(t)
	s := open(t, f)
	f.signErr = errors.New("AccessDeniedException")
	_, err := s.Sign([]byte("m"))
	if !errors.Is(err, f.signErr) || !strings.Contains(err.Error(), testARN) {
		t.Fatalf("err = %v", err)
	}
}

func TestTimeout(t *testing.T) {
	f := newFake(t)
	s, err := Open(context.Background(), Config{Client: f, KeyID: testARN, Timeout: 3 * time.Second})
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
	if dl, ok := f.lastCtx.Deadline(); !ok || time.Until(dl) <= 3*time.Second {
		t.Fatalf("default timeout not applied: %v %v", dl, ok)
	}
}

func TestFormatShowsNoConfiguration(t *testing.T) {
	s := open(t, newFake(t))
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s)} {
		if !strings.HasPrefix(out, "awskms.Signer{"+testARN+" ed25519:") {
			t.Fatalf("String = %s", out)
		}
	}
}

// KMS signs tree heads, so a store anchors a run through it and the heads verify with the public key alone.
func TestAuditedStoreAnchorsThroughKMS(t *testing.T) {
	ctx := context.Background()
	s := open(t, newFake(t))
	anchor := audit.NewMemAnchorLog()
	store, err := audit.NewAuditedStore(agent.NewMemStore(), s, anchor)
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(store)
	refund := agent.MustFunc("refund", "refund a payment", func(context.Context, struct{}) (string, error) { return "refunded", nil })
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.ToolTurn("call-1", "refund", `{}`), agenttest.TextTurn("done")), j, agent.WithTools(refund))
	if _, err := a.Run(ctx, "run-kms", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	v, err := audit.ParsePublicKey(audit.FormatPublicKey(audit.AlgEd25519, s.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
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
}

// Evidence seals a fixed-size digest message, so a package of any size seals under KMS's limit: a
// one-action package and a twelve-action one (about 11 KB of JSON) both seal and verify.
func TestEvidenceSealUnderLimit(t *testing.T) {
	ctx := context.Background()
	s := open(t, newFake(t))
	store, err := audit.NewAuditedStore(agent.NewMemStore(), s, audit.NewMemAnchorLog())
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(store)
	refund := agent.MustFunc("refund", "refund a payment", func(context.Context, struct{}) (string, error) { return "refunded", nil })
	var turns []agenttest.ScriptedTurn
	for i := range 12 {
		turns = append(turns, agenttest.ToolTurn(fmt.Sprintf("call-%d", i), "refund", `{}`))
	}
	turns = append(turns, agenttest.TextTurn("done"))
	a := agenttest.MustNew(agenttest.NewScriptedModel(turns...), j, agent.WithTools(refund))
	if _, err := a.Run(ctx, "run-ev", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	v, _ := audit.VerifierOf(s)

	for name, opts := range map[string][]audit.EvidenceOption{
		"one action":  {audit.WithToolCall("call-0")},
		"all actions": {audit.WithAllToolCalls()},
	} {
		pkg, err := audit.Evidence(ctx, j, "run-ev", s, 1700000000, opts...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if raw, _ := json.Marshal(pkg); name == "all actions" && len(raw) <= MaxMessageSize {
			t.Fatalf("the package is %d bytes; the test needs one over the limit", len(raw))
		}
		if rep, err := pkg.Verify(v); err != nil || !rep.OK {
			t.Fatalf("%s: %+v %v", name, rep, err)
		}
	}
}

func newP256Fake(t *testing.T) *fakeKMS {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	f.p256 = k
	return f
}

func openP256(t *testing.T, f *fakeKMS) *Signer {
	t.Helper()
	s, err := Open(context.Background(), Config{Client: f, KeyID: "alias/bide-audit", Scheme: audit.AlgECDSAP256})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestP256Scheme(t *testing.T) {
	for _, highS := range []bool{false, true} {
		f := newP256Fake(t)
		f.highS = highS // KMS may return either S; bide verifies the low-S form
		s := openP256(t, f)
		if s.Alg() != audit.AlgECDSAP256 || len(s.PublicKey()) != 65 {
			t.Fatalf("Alg %s, key %d bytes", s.Alg(), len(s.PublicKey()))
		}
		v, err := audit.VerifierOf(s)
		if err != nil {
			t.Fatal(err)
		}
		// A message over the Ed25519 limit signs: KMS sees only the 32-byte digest.
		for _, n := range []int{0, 100, MaxMessageSize + 1, 200000} {
			msg := bytes.Repeat([]byte{'x'}, n)
			sig, err := s.Sign(msg)
			if err != nil || len(sig) != 64 || !v.Verify(msg, sig) {
				t.Fatalf("highS=%v, %d bytes: %v", highS, n, err)
			}
		}
	}
}

func TestP256RefusesOtherKeys(t *testing.T) {
	// An Ed25519 key under the P-256 scheme, and a P-256 key under the Ed25519 scheme.
	if _, err := Open(context.Background(), Config{Client: newFake(t), KeyID: testARN, Scheme: audit.AlgECDSAP256}); !errors.Is(err, ErrNoP256) {
		t.Fatalf("Ed25519 key under P-256: err = %v", err)
	}
	if _, err := Open(context.Background(), Config{Client: newP256Fake(t), KeyID: testARN}); !errors.Is(err, ErrNoEd25519) {
		t.Fatalf("P-256 key under Ed25519: err = %v", err)
	}
	if _, err := Open(context.Background(), Config{Client: newFake(t), KeyID: testARN, Scheme: "rsa"}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("unknown scheme: err = %v", err)
	}
}

func TestP256SignRefusesAReplacedKey(t *testing.T) {
	f := newP256Fake(t)
	s := openP256(t, f)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.p256 = other
	// The fake now also reports other's public key, but the signer keeps the one it read at Open.
	if _, err := s.Sign([]byte("m")); err == nil {
		t.Fatal("signature by a different key accepted")
	}
}

// With P-256 the evidence seal and large packages sign: KMS never sees more than a digest.
func TestP256EvidenceSealsAnyPackage(t *testing.T) {
	ctx := context.Background()
	s := openP256(t, newP256Fake(t))
	store, err := audit.NewAuditedStore(agent.NewMemStore(), s, audit.NewMemAnchorLog())
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(store)
	refund := agent.MustFunc("refund", "refund a payment", func(context.Context, struct{}) (string, error) { return "refunded", nil })
	var turns []agenttest.ScriptedTurn
	for i := range 12 {
		turns = append(turns, agenttest.ToolTurn(fmt.Sprintf("call-%d", i), "refund", `{}`))
	}
	turns = append(turns, agenttest.TextTurn("done"))
	a := agenttest.MustNew(agenttest.NewScriptedModel(turns...), j, agent.WithTools(refund))
	if _, err := a.Run(ctx, "run-ev", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	v, _ := audit.VerifierOf(s)
	ev, err := audit.Evidence(ctx, j, "run-ev", s, 1700000000, audit.WithAllToolCalls())
	if err != nil {
		t.Fatalf("a 12-action package: %v", err)
	}
	if rep, err := ev.Verify(v); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}
