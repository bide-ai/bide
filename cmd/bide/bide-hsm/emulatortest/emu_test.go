package emulatortest

// These tests run bide-hsm's gcpkms: references against the Cloud KMS emulator
// github.com/blackwell-systems/gcp-kms-emulator, started in the test process on an in-memory gRPC
// listener. They drive hsmkeys (what bide-hsm opens a key with) and approvecmd.MainWith with a key
// source that loads through hsmkeys (what bide-hsm approve is), with the options bide-hsm's flags set.
// The emulator reports protection level SOFTWARE for every key, so every use needs
// AllowSoftwareKey (-allow-software-key); a wrapper that reports HSM shows the default policy.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	emulator "github.com/blackwell-systems/gcp-kms-emulator"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/hsmkeys"
	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/keyref"
	"github.com/bide-ai/bide/cmd/bide/internal/approvecmd"
	"github.com/bide-ai/bide/signer/gcpkms"
	"github.com/bide-ai/bide/store/sqlite"
)

type emu struct {
	client *kms.KeyManagementClient
	ring   string
	n      int
}

func startEmulator(t *testing.T) *emu {
	t.Helper()
	t.Setenv("IAM_MODE", "off")
	srv := grpc.NewServer()
	if err := emulator.Register(srv); err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	client, err := kms.NewKeyManagementClient(context.Background(), option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	ring, err := client.CreateKeyRing(context.Background(), &kmspb.CreateKeyRingRequest{Parent: "projects/bank-prod/locations/europe-west2", KeyRingId: "bide"})
	if err != nil {
		t.Fatal(err)
	}
	return &emu{client: client, ring: ring.Name}
}

// ref creates a key of the algorithm and returns a gcpkms: reference to its first version.
func (e *emu) ref(t *testing.T, alg kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm) string {
	t.Helper()
	e.n++
	k, err := e.client.CreateCryptoKey(context.Background(), &kmspb.CreateCryptoKeyRequest{
		Parent: e.ring, CryptoKeyId: "approver" + string(rune('a'+e.n)),
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: alg}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "gcpkms:" + k.Primary.Name
}

// open parses ref and opens it as bide-hsm does, with the emulator as the Cloud KMS client.
func (e *emu) open(t *testing.T, client gcpkms.API, ref string, o hsmkeys.Options) (audit.Signer, error) {
	t.Helper()
	r, err := keyref.Parse(ref)
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		client = e.client
	}
	o.GCPClient = func(context.Context) (gcpkms.API, error) { return client, nil }
	return hsmkeys.Open(context.Background(), r, o)
}

var soft = hsmkeys.Options{AllowSoftwareKey: true}

// The scheme is detected, P-256 first, and the signer says where the key is held and, for a software
// key, says so.
func TestDetectionWhereAndNotice(t *testing.T) {
	e := startEmulator(t)
	for _, tc := range []struct {
		alg    kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm
		scheme audit.Alg
	}{{kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256, audit.AlgECDSAP256}, {kmspb.CryptoKeyVersion_EC_SIGN_ED25519, audit.AlgEd25519}} {
		ref := e.ref(t, tc.alg)
		s, err := e.open(t, nil, ref, soft)
		if err != nil || s.Alg() != tc.scheme {
			t.Fatalf("%s: %v %v", tc.scheme, s, err)
		}
		d := s.(interface {
			Where() string
			Notice() string
		})
		if d.Where() != ref+" (protection level SOFTWARE)" || !strings.HasPrefix(d.Notice(), "-allow-software-key: this key version is at protection level SOFTWARE") {
			t.Fatalf("Where %q, Notice %q", d.Where(), d.Notice())
		}
		// Naming the other scheme is the signer's refusal, not a downgrade.
		other := audit.AlgEd25519
		if tc.scheme == audit.AlgEd25519 {
			other = audit.AlgECDSAP256
		}
		if _, err := e.open(t, nil, ref, hsmkeys.Options{AllowSoftwareKey: true, Alg: other}); err == nil {
			t.Fatalf("a %s key opened as %s", tc.scheme, other)
		}
	}
}

// The default refuses a software key (all of the emulator's are), a version that does not exist is
// refused, and a key of neither scheme is neither.
func TestRefusals(t *testing.T) {
	e := startEmulator(t)
	ref := e.ref(t, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256)
	if _, err := e.open(t, nil, ref, hsmkeys.Options{}); !errors.Is(err, gcpkms.ErrProtectionLevel) {
		t.Fatalf("default: %v", err)
	}
	edRef := e.ref(t, kmspb.CryptoKeyVersion_EC_SIGN_ED25519)
	if _, err := e.open(t, nil, edRef, hsmkeys.Options{}); !errors.Is(err, gcpkms.ErrProtectionLevel) {
		t.Fatalf("Ed25519, default: %v", err)
	}
	if _, err := e.open(t, nil, ref[:strings.LastIndex(ref, "/")+1]+"2", soft); err == nil {
		t.Fatal("version 2 of a key with one version opened")
	}
	other := e.ref(t, kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384)
	if _, err := e.open(t, nil, other, soft); err == nil || !strings.Contains(err.Error(), "neither") {
		t.Fatalf("P-384: %v", err)
	}
}

// hsmLevel reports every key version at protection level HSM, which the emulator cannot, and counts
// the key versions read.
type hsmLevel struct {
	gcpkms.API
	reads atomic.Int32
}

func (h *hsmLevel) GetCryptoKeyVersion(ctx context.Context, req *kmspb.GetCryptoKeyVersionRequest, opts ...gax.CallOption) (*kmspb.CryptoKeyVersion, error) {
	h.reads.Add(1)
	v, err := h.API.GetCryptoKeyVersion(ctx, req, opts...)
	if err == nil {
		v.ProtectionLevel = kmspb.ProtectionLevel_HSM
	}
	return v, err
}

func (h *hsmLevel) GetPublicKey(ctx context.Context, req *kmspb.GetPublicKeyRequest, opts ...gax.CallOption) (*kmspb.PublicKey, error) {
	pk, err := h.API.GetPublicKey(ctx, req, opts...)
	if err == nil {
		pk.ProtectionLevel = kmspb.ProtectionLevel_HSM
	}
	return pk, err
}

func (h *hsmLevel) AsymmetricSign(ctx context.Context, req *kmspb.AsymmetricSignRequest, opts ...gax.CallOption) (*kmspb.AsymmetricSignResponse, error) {
	r, err := h.API.AsymmetricSign(ctx, req, opts...)
	if err == nil {
		r.ProtectionLevel = kmspb.ProtectionLevel_HSM
	}
	return r, err
}

// A key version at protection level HSM is used by default, shown as HSM with no software notice, and
// found with one read of the version (P-256 first: no wasted attempt); VerifyAttestation reaches the
// signer (this version carries no attestation, so it is refused), and Ed25519 still needs the flag.
func TestHSMDefaultAndAttestation(t *testing.T) {
	e := startEmulator(t)
	h := &hsmLevel{API: e.client}
	ref := e.ref(t, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256)
	s, err := e.open(t, h, ref, hsmkeys.Options{})
	if err != nil {
		t.Fatal(err)
	}
	d := s.(interface {
		Where() string
		Notice() string
	})
	if !strings.HasSuffix(d.Where(), "(protection level HSM)") || d.Notice() != "" || h.reads.Load() != 1 {
		t.Fatalf("Where %q, Notice %q, %d reads", d.Where(), d.Notice(), h.reads.Load())
	}
	if _, err := e.open(t, h, ref, hsmkeys.Options{VerifyAttestation: true}); !errors.Is(err, gcpkms.ErrAttestation) {
		t.Fatalf("attestation: %v", err)
	}
}

// A 2-of-3 gate decided by two approvers whose keys are in Cloud KMS, one P-256 and one Ed25519,
// through the approval command with a key source that loads through hsmkeys; the gate re-verifies both
// signatures when the run resumes.
func TestTwoOfThreeInCloudKMS(t *testing.T) {
	e := startEmulator(t)
	refs := map[string]string{
		"ops":     e.ref(t, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256),
		"finance": e.ref(t, kmspb.CryptoKeyVersion_EC_SIGN_ED25519),
	}
	enrol := map[string]string{}
	pubs := map[string]audit.Verifier{}
	for id, ref := range refs {
		s, err := e.open(t, nil, ref, soft)
		if err != nil {
			t.Fatal(err)
		}
		text := audit.FormatPublicKey(s.Alg(), s.PublicKey())
		enrol[id] = text
		if pubs[id], err = audit.ParsePublicKey(text); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.HasPrefix(enrol["ops"], "ecdsa-p256:") || !strings.HasPrefix(enrol["finance"], "ed25519:") {
		t.Fatalf("detected schemes: %v", enrol)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	enrol["risk"] = audit.FormatPublicKey(audit.AlgEd25519, other.Public().(ed25519.PublicKey))
	pubs["risk"], _ = audit.ParsePublicKey(enrol["risk"])

	dir := t.TempDir()
	db, keysFile := filepath.Join(dir, "j.db"), filepath.Join(dir, "keys.json")
	b, _ := json.Marshal(enrol)
	if err := os.WriteFile(keysFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	var charges atomic.Int32
	policy := &agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}
	drive := func() error {
		s, err := sqlite.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		refund := agent.MustFunc("refund", "refund an order", func(context.Context, json.RawMessage) (string, error) {
			charges.Add(1)
			return "refunded", nil
		}, agent.WithApproval(policy))
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c2", "refund", `{"order":1049}`), agenttest.TextTurn("done"))
		verify := func(id string) (agent.ApproverVerifier, bool) { v, ok := pubs[id]; return v, ok }
		_, err = agenttest.MustNew(m, agenttest.MustJournal(s), agent.WithTools(refund), agent.WithApproverVerifiers(verify)).Run(context.Background(), "refund-1049", agent.UserText("refund"))
		return err
	}
	if _, ok := errors.AsType[*agent.ApprovalPending](drive()); !ok {
		t.Fatal("the first drive did not pause")
	}

	src := approvecmd.KeySource{
		Prog: "bide-hsm approve", Usage: "usage\n", KeyHelp: "key", KeyNoun: "a key reference", EnrolCmd: "bide-hsm pubkey -as",
		ReceiptWhere: "in the working directory", Placeholder: "<key-ref>",
		Load:       func(ref string) (audit.Signer, error) { return e.open(t, nil, ref, soft) },
		ReceiptDir: func(string) string { return dir },
	}
	for _, id := range []string{"ops", "finance"} {
		var out, errb bytes.Buffer
		code := approvecmd.MainWith([]string{"-db", db, "-color", "never", "-run", "refund-1049", "-call", "c2", "-as", id, "-key", refs[id], "-approver-keys", keysFile, "-yes"},
			strings.NewReader(""), &out, &errb, approvecmd.JournalStores(), src)
		if code != 0 || !strings.Contains(out.String(), "signed and recorded") || !strings.Contains(out.String(), "protection level SOFTWARE") {
			t.Fatalf("%s: %d\n%s\n%s", id, code, out.String(), errb.String())
		}
	}
	if err := drive(); err != nil || charges.Load() != 1 {
		t.Fatalf("resume: %v, charges %d", err, charges.Load())
	}
}
