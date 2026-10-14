package gcpkms_test

// These tests run the signer, through the real Cloud KMS Go client, against the Cloud KMS emulator
// github.com/blackwell-systems/gcp-kms-emulator, started in the test process on an in-memory gRPC
// listener. They need no Docker, network or credentials.
//
// The emulator reports protection level SOFTWARE for every key, so the RequireHSM success path is
// covered by the fake client in signer_test.go. Ed25519 tests skip, with a message, while the
// emulator version in go.mod lacks EC_SIGN_ED25519.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	emulator "github.com/blackwell-systems/gcp-kms-emulator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/signer/gcpkms"
)

// emu is a running emulator and a Cloud KMS client connected to it.
type emu struct {
	client *kms.KeyManagementClient
	ring   string
	n      int
}

// startEmulator runs the emulator on a bufconn listener. icept, when set, is a client-side unary
// interceptor that can corrupt a request or a response in transit.
func startEmulator(t *testing.T, icept grpc.UnaryClientInterceptor) *emu {
	t.Helper()
	t.Setenv("IAM_MODE", "off")
	srv := grpc.NewServer()
	if err := emulator.Register(srv); err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	opts := []grpc.DialOption{
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	if icept != nil {
		opts = append(opts, grpc.WithChainUnaryInterceptor(icept))
	}
	conn, err := grpc.NewClient("passthrough:///bufnet", opts...)
	if err != nil {
		t.Fatal(err)
	}
	client, err := kms.NewKeyManagementClient(context.Background(), option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	e := &emu{client: client}
	ring, err := client.CreateKeyRing(context.Background(), &kmspb.CreateKeyRingRequest{
		Parent: "projects/bank-prod/locations/europe-west2", KeyRingId: "bide"})
	if err != nil {
		t.Fatal(err)
	}
	e.ring = ring.Name
	return e
}

// key creates a key with the algorithm and returns the name of its first version.
func (e *emu) key(t *testing.T, purpose kmspb.CryptoKey_CryptoKeyPurpose, alg kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm) (string, error) {
	t.Helper()
	e.n++
	k, err := e.client.CreateCryptoKey(context.Background(), &kmspb.CreateCryptoKeyRequest{
		Parent: e.ring, CryptoKeyId: "k" + string(rune('a'+e.n)),
		CryptoKey: &kmspb.CryptoKey{Purpose: purpose, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: alg}},
	})
	if err != nil {
		return "", err
	}
	return k.Primary.Name, nil
}

func (e *emu) signKey(t *testing.T, alg kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm) string {
	t.Helper()
	v, err := e.key(t, kmspb.CryptoKey_ASYMMETRIC_SIGN, alg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *emu) setState(t *testing.T, version string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
	t.Helper()
	_, err := e.client.UpdateCryptoKeyVersion(context.Background(), &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: version, State: st},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ed25519Key creates an EC_SIGN_ED25519 key, or skips the test while the emulator cannot.
func (e *emu) ed25519Key(t *testing.T) string {
	t.Helper()
	v, err := e.key(t, kmspb.CryptoKey_ASYMMETRIC_SIGN, kmspb.CryptoKeyVersion_EC_SIGN_ED25519)
	if err != nil {
		if strings.Contains(err.Error(), "unsupported algorithm") {
			t.Skipf("the gcp-kms-emulator version in go.mod does not support EC_SIGN_ED25519 yet (%v); bump it once feat/ec-sign-ed25519 is released", status.Code(err))
		}
		t.Fatal(err)
	}
	return v
}

func p256(t *testing.T, e *emu, version string, mod ...func(*gcpkms.Config)) (*gcpkms.Signer, error) {
	cfg := gcpkms.Config{Client: e.client, KeyVersion: version, Scheme: audit.AlgECDSAP256}
	for _, m := range mod {
		m(&cfg)
	}
	return gcpkms.Open(context.Background(), cfg)
}

func TestEmulatorP256(t *testing.T) {
	e := startEmulator(t, nil)
	s, err := p256(t, e, e.signKey(t, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256))
	if err != nil {
		t.Fatal(err)
	}
	if s.Alg() != audit.AlgECDSAP256 || len(s.PublicKey()) != 65 || s.ProtectionLevel() != "SOFTWARE" {
		t.Fatalf("alg %s, %d-byte key, level %s", s.Alg(), len(s.PublicKey()), s.ProtectionLevel())
	}
	v, err := audit.VerifierOf(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, 60, 5000, 200000} {
		msg := bytes.Repeat([]byte{'m'}, n)
		sig, err := s.Sign(msg)
		if err != nil || len(sig) != 64 || !v.Verify(msg, sig) {
			t.Fatalf("%d bytes: %v", n, err)
		}
	}
}

func TestEmulatorEd25519(t *testing.T) {
	e := startEmulator(t, nil)
	version := e.ed25519Key(t)
	s, err := gcpkms.Open(context.Background(), gcpkms.Config{Client: e.client, KeyVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	if s.Alg() != audit.AlgEd25519 || len(s.PublicKey()) != 32 {
		t.Fatalf("alg %s, %d-byte key", s.Alg(), len(s.PublicKey()))
	}
	v, _ := audit.VerifierOf(s)
	for _, n := range []int{1, 60, 4096, 5000, 60000} {
		msg := bytes.Repeat([]byte{'m'}, n)
		sig, err := s.Sign(msg)
		if err != nil || !v.Verify(msg, sig) {
			t.Fatalf("%d bytes: %v", n, err)
		}
	}
	// RequireHSM never opens an Ed25519 version.
	if _, err := gcpkms.Open(context.Background(), gcpkms.Config{Client: e.client, KeyVersion: version, RequireHSM: true}); !errors.Is(err, gcpkms.ErrNotHSM) {
		t.Fatalf("RequireHSM: %v", err)
	}
	// An Ed25519 version is not a P-256 key.
	if _, err := p256(t, e, version); !errors.Is(err, gcpkms.ErrNoP256) {
		t.Fatalf("under P-256: %v", err)
	}
}

func TestEmulatorRefusals(t *testing.T) {
	e := startEmulator(t, nil)
	ctx := context.Background()
	ed := gcpkms.Config{Client: e.client}

	// Algorithms that are neither of the two bide uses, and keys that do not sign.
	for _, alg := range []kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm{
		kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384, kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256,
	} {
		cfg := ed
		cfg.KeyVersion = e.signKey(t, alg)
		if _, err := gcpkms.Open(ctx, cfg); !errors.Is(err, gcpkms.ErrNoEd25519) {
			t.Errorf("%s under Ed25519: %v", alg, err)
		}
		if _, err := p256(t, e, cfg.KeyVersion); !errors.Is(err, gcpkms.ErrNoP256) {
			t.Errorf("%s under P-256: %v", alg, err)
		}
	}
	enc, err := e.key(t, kmspb.CryptoKey_ENCRYPT_DECRYPT, kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p256(t, e, enc); !errors.Is(err, gcpkms.ErrNoP256) {
		t.Errorf("a symmetric key: %v", err)
	}

	// The emulator, like Cloud KMS, protects software keys: RequireHSM refuses them.
	v := e.signKey(t, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256)
	if _, err := p256(t, e, v, func(c *gcpkms.Config) { c.RequireHSM = true }); !errors.Is(err, gcpkms.ErrNotHSM) {
		t.Errorf("RequireHSM on a software key: %v", err)
	}

	// A version that does not exist is Cloud KMS's NotFound, as a code and nothing more.
	_, err = p256(t, e, e.ring+"/cryptoKeys/absent/cryptoKeyVersions/1")
	if !errors.Is(err, gcpkms.CodeError{Code: codes.NotFound}) {
		t.Errorf("a missing version: %v", err)
	}

	// A disabled version is refused at Open, and a version disabled later fails at Sign.
	d := e.signKey(t, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256)
	s, err := p256(t, e, d)
	if err != nil {
		t.Fatal(err)
	}
	e.setState(t, d, kmspb.CryptoKeyVersion_DISABLED)
	if _, err := p256(t, e, d); !errors.Is(err, gcpkms.ErrVersionState) {
		t.Errorf("a disabled version at Open: %v", err)
	}
	if sig, err := s.Sign([]byte("m")); err == nil || !errors.Is(err, gcpkms.CodeError{Code: codes.FailedPrecondition}) {
		t.Errorf("a version disabled after Open signed %x, err = %v", sig, err)
	}
	e.setState(t, d, kmspb.CryptoKeyVersion_ENABLED)
	if _, err := s.Sign([]byte("m")); err != nil {
		t.Errorf("after re-enabling: %v", err)
	}
	if _, err := e.client.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: d}); err != nil {
		t.Fatal(err)
	}
	if _, err := p256(t, e, d); !errors.Is(err, gcpkms.ErrVersionState) {
		t.Errorf("a version scheduled for destruction: %v", err)
	}
}

// Signing uses the pinned version even when another version becomes the primary.
func TestEmulatorPinnedVersion(t *testing.T) {
	e := startEmulator(t, nil)
	ctx := context.Background()
	v1 := e.signKey(t, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256)
	keyName := v1[:strings.Index(v1, "/cryptoKeyVersions/")]
	s1, err := p256(t, e, v1)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := e.client.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: keyName,
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{
		Name: keyName, CryptoKeyVersionId: v2.Name[strings.LastIndex(v2.Name, "/")+1:]}); err != nil {
		t.Fatal(err)
	}
	s2, err := p256(t, e, v2.Name)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(s1.PublicKey(), s2.PublicKey()) {
		t.Fatal("two versions share a public key")
	}
	msg := []byte("pinned")
	sig, err := s1.Sign(msg)
	v1v, _ := audit.VerifierOf(s1)
	v2v, _ := audit.VerifierOf(s2)
	if err != nil || !v1v.Verify(msg, sig) || v2v.Verify(msg, sig) {
		t.Fatalf("version 1 signed with the wrong key: %v", err)
	}
}

// The request checksum reaches the real server: a request corrupted in transit is rejected, and the
// rejection is a code and nothing else.
func TestEmulatorRejectsACorruptedRequest(t *testing.T) {
	var corrupt atomic.Bool
	e := startEmulator(t, func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, inv grpc.UnaryInvoker, o ...grpc.CallOption) error {
		if r, ok := req.(*kmspb.AsymmetricSignRequest); ok && corrupt.Load() {
			r.Digest.GetDigest().(*kmspb.Digest_Sha256).Sha256[0] ^= 1 // after the checksum was computed
		}
		return inv(ctx, method, req, reply, cc, o...)
	})
	s, err := p256(t, e, e.signKey(t, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256))
	if err != nil {
		t.Fatal(err)
	}
	corrupt.Store(true)
	if sig, err := s.Sign([]byte("m")); !errors.Is(err, gcpkms.CodeError{Code: codes.InvalidArgument}) {
		t.Fatalf("signed a corrupted request: %x, %v", sig, err)
	}
}

// A signature corrupted on the way back fails its checksum, is retried, and an intact answer is accepted.
func TestEmulatorCorruptedResponse(t *testing.T) {
	var corrupt atomic.Int32
	e := startEmulator(t, func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, inv grpc.UnaryInvoker, o ...grpc.CallOption) error {
		err := inv(ctx, method, req, reply, cc, o...)
		if r, ok := reply.(*kmspb.AsymmetricSignResponse); ok && err == nil && corrupt.Add(-1) >= 0 {
			r.Signature[len(r.Signature)-1] ^= 1
		}
		return err
	})
	s, err := p256(t, e, e.signKey(t, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := audit.VerifierOf(s)
	corrupt.Store(2)
	if sig, err := s.Sign([]byte("m")); err != nil || !v.Verify([]byte("m"), sig) {
		t.Fatalf("two corrupted answers then an intact one: %v", err)
	}
	corrupt.Store(100)
	if _, err := s.Sign([]byte("m")); !errors.Is(err, gcpkms.ErrIntegrity) {
		t.Fatalf("persistent corruption: %v", err)
	}
}

// The request checksum is set: an emulator that verifies it reports so in its response.
func TestEmulatorVerifiesRequestChecksums(t *testing.T) {
	e := startEmulator(t, nil)
	version := e.signKey(t, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256)
	h := sha256.Sum256([]byte("x"))
	resp, err := e.client.AsymmetricSign(context.Background(), &kmspb.AsymmetricSignRequest{Name: version,
		Digest:       &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: h[:]}},
		DigestCrc32C: wrapperspb.Int64(1)})
	if err == nil {
		t.Fatalf("the emulator accepted a wrong checksum: %+v", resp)
	}
}

// The emulator signs a real run's tree heads and evidence seal.
func TestEmulatorAuditedStoreAndEvidence(t *testing.T) {
	e := startEmulator(t, nil)
	ctx := context.Background()
	s, err := p256(t, e, e.signKey(t, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256))
	if err != nil {
		t.Fatal(err)
	}
	anchor := audit.NewMemAnchorLog()
	store, err := audit.NewAuditedStore(agent.NewMemStore(), s, anchor)
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(store)
	refund := agent.MustFunc("refund", "refund a payment", func(context.Context, struct{}) (string, error) { return "refunded", nil })
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.ToolTurn("refund-1", "refund", `{}`), agenttest.TextTurn("done")), j, agent.WithTools(refund))
	if _, err := a.Run(ctx, "run-1", agent.UserText("refund order 1042")); err != nil {
		t.Fatal(err)
	}
	v, err := audit.ParsePublicKey(audit.FormatPublicKey(s.Alg(), s.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	entries := anchor.Entries()
	if len(entries) == 0 {
		t.Fatal("nothing anchored")
	}
	sth := entries[len(entries)-1].STH
	if err := sth.Verify(v); err != nil {
		t.Fatal(err)
	}
	ev, err := audit.Evidence(ctx, j, "run-1", s, sth.TimestampNanos, audit.WithToolCall("refund-1"))
	if err != nil {
		t.Fatal(err)
	}
	if rep, err := ev.Verify(v); err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestEmulatorEd25519AuditedStore(t *testing.T) {
	e := startEmulator(t, nil)
	s, err := gcpkms.Open(context.Background(), gcpkms.Config{Client: e.client, KeyVersion: e.ed25519Key(t)})
	if err != nil {
		t.Fatal(err)
	}
	anchor := audit.NewMemAnchorLog()
	store, err := audit.NewAuditedStore(agent.NewMemStore(), s, anchor)
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(store)
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("done")), j)
	if _, err := a.Run(context.Background(), "run-ed", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	v, _ := audit.VerifierOf(s)
	entries := anchor.Entries()
	if len(entries) == 0 || entries[len(entries)-1].STH.Verify(v) != nil {
		t.Fatal("the anchored head does not verify")
	}
}
