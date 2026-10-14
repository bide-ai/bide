package azuretest

// These tests run bide-hsm's azurekv: references against the Azure SDK's in-process fake Key Vault
// server, behind the real azkeys client. They drive hsmkeys (what bide-hsm opens a key with) and
// approvecmd.MainWith with a key source that loads through hsmkeys.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"
	kvfake "github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys/fake"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/hsmkeys"
	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/keyref"
	"github.com/bide-ai/bide/cmd/bide/internal/approvecmd"
	"github.com/bide-ai/bide/signer/azurekv"
	"github.com/bide-ai/bide/store/sqlite"
)

const (
	vault   = "https://bank-prod.vault.azure.net"
	version = "0123456789abcdef0123456789abcdef"
)

type cred struct{}

func (cred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "fake-token"}, nil
}

// kv is the fake Key Vault: keys by name, each an ECDSA P-256 key of a kind (EC-HSM or EC).
type kv struct {
	keys  map[string]*ecdsa.PrivateKey
	kinds map[string]azkeys.KeyType
	signs atomic.Int32
}

func newKV() *kv {
	return &kv{keys: map[string]*ecdsa.PrivateKey{}, kinds: map[string]azkeys.KeyType{}}
}

// add creates key name of kind and returns its azurekv: reference.
func (k *kv) add(t *testing.T, name string, kind azkeys.KeyType) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k.keys[name], k.kinds[name] = priv, kind
	return "azurekv:" + vault + "/keys/" + name + "/" + version
}

func (k *kv) bundle(name string) azkeys.KeyBundle {
	priv := k.keys[name]
	return azkeys.KeyBundle{
		Attributes: &azkeys.KeyAttributes{Enabled: to.Ptr(true), HSMPlatform: to.Ptr("2")},
		Key: &azkeys.JSONWebKey{
			KID:    to.Ptr(azkeys.ID(vault + "/keys/" + name + "/" + version)),
			Kty:    to.Ptr(k.kinds[name]),
			Crv:    to.Ptr(azkeys.CurveNameP256),
			KeyOps: []*azkeys.KeyOperation{to.Ptr(azkeys.KeyOperationSign)},
			X:      priv.PublicKey.X.FillBytes(make([]byte, 32)),
			Y:      priv.PublicKey.Y.FillBytes(make([]byte, 32)),
		},
	}
}

// client is the real azkeys client talking to the SDK's fake server.
func (k *kv) client(t *testing.T) azurekv.API {
	t.Helper()
	split := func(name, ver string) (string, string) {
		// The SDK fake's route lets the key name swallow "/<version>"; split it as the service would.
		if n, v, ok := strings.Cut(name, "/"); ok && ver == "" {
			return n, v
		}
		return name, ver
	}
	srv := kvfake.Server{
		GetKey: func(_ context.Context, name, ver string, _ *azkeys.GetKeyOptions) (azfake.Responder[azkeys.GetKeyResponse], azfake.ErrorResponder) {
			name, ver = split(name, ver)
			var r azfake.Responder[azkeys.GetKeyResponse]
			var e azfake.ErrorResponder
			if _, ok := k.keys[name]; !ok || ver != version {
				e.SetResponseError(http.StatusNotFound, "KeyNotFound")
				return r, e
			}
			r.SetResponse(http.StatusOK, azkeys.GetKeyResponse{KeyBundle: k.bundle(name)}, nil)
			return r, e
		},
		Sign: func(_ context.Context, name, ver string, p azkeys.SignParameters, _ *azkeys.SignOptions) (azfake.Responder[azkeys.SignResponse], azfake.ErrorResponder) {
			name, ver = split(name, ver)
			k.signs.Add(1)
			var r azfake.Responder[azkeys.SignResponse]
			var e azfake.ErrorResponder
			priv, ok := k.keys[name]
			if !ok || ver != version || p.Algorithm == nil || *p.Algorithm != azkeys.SignatureAlgorithmES256 || len(p.Value) != sha256.Size {
				e.SetResponseError(http.StatusBadRequest, "BadParameter")
				return r, e
			}
			rr, ss, err := ecdsa.Sign(rand.Reader, priv, p.Value)
			if err != nil {
				e.SetResponseError(http.StatusInternalServerError, "Internal")
				return r, e
			}
			if ss.Cmp(new(big.Int).Rsh(elliptic.P256().Params().N, 1)) > 0 {
				ss = new(big.Int).Sub(elliptic.P256().Params().N, ss)
			}
			raw := append(rr.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
			r.SetResponse(http.StatusOK, azkeys.SignResponse{KeyOperationResult: azkeys.KeyOperationResult{KID: to.Ptr(azkeys.ID(vault + "/keys/" + name + "/" + version)), Result: raw}}, nil)
			return r, e
		},
	}
	c, err := azkeys.NewClient(vault, cred{}, &azkeys.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: kvfake.NewServerTransport(&srv)}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func open(t *testing.T, k *kv, ref string, o hsmkeys.Options) (audit.Signer, error) {
	t.Helper()
	r, err := keyref.Parse(ref)
	if err != nil {
		t.Fatal(err)
	}
	o.AzureClient = k.client(t)
	return hsmkeys.Open(context.Background(), r, o)
}

// An HSM key opens by default, is shown as HSM-protected with no notice, and signs; a software key is
// refused without -allow-software-key and, with it, is shown as software with a notice; Key Vault has
// no Ed25519, so -alg ed25519 is refused and a missing key or version is the service's refusal.
func TestOpenWhereNoticeAndRefusals(t *testing.T) {
	k := newKV()
	hsmRef, softRef := k.add(t, "approver-hsm", azkeys.KeyTypeECHSM), k.add(t, "approver-soft", azkeys.KeyTypeEC)
	s, err := open(t, k, hsmRef, hsmkeys.Options{})
	if err != nil {
		t.Fatal(err)
	}
	d := s.(interface {
		Where() string
		Notice() string
	})
	if d.Where() != hsmRef+" (HSM-protected)" || d.Notice() != "" || s.Alg() != audit.AlgECDSAP256 {
		t.Fatalf("Where %q Notice %q", d.Where(), d.Notice())
	}
	v, _ := audit.VerifierOf(s)
	sig, err := s.Sign([]byte("decision"))
	if err != nil || !v.Verify([]byte("decision"), sig) {
		t.Fatalf("sign: %v", err)
	}

	if _, err := open(t, k, softRef, hsmkeys.Options{}); !errors.Is(err, azurekv.ErrNotHSM) {
		t.Fatalf("software, default: %v", err)
	}
	s, err = open(t, k, softRef, hsmkeys.Options{AllowSoftwareKey: true})
	if err != nil {
		t.Fatal(err)
	}
	d = s.(interface {
		Where() string
		Notice() string
	})
	if d.Where() != softRef+" (software-protected)" || !strings.HasPrefix(d.Notice(), "-allow-software-key: this key is software-protected") {
		t.Fatalf("Where %q Notice %q", d.Where(), d.Notice())
	}

	if _, err := open(t, k, hsmRef, hsmkeys.Options{Alg: audit.AlgEd25519}); err == nil || !strings.Contains(err.Error(), "no Ed25519") {
		t.Fatalf("-alg ed25519: %v", err)
	}
	if s, err := open(t, k, hsmRef, hsmkeys.Options{Alg: audit.AlgECDSAP256}); err != nil || s.Alg() != audit.AlgECDSAP256 {
		t.Fatalf("-alg ecdsa-p256: %v", err)
	}
	if _, err := open(t, k, "azurekv:"+vault+"/keys/nobody/"+version, hsmkeys.Options{}); err == nil {
		t.Fatal("a key that does not exist opened")
	}
	other := strings.TrimSuffix(hsmRef, version) + "ffffffffffffffffffffffffffffffff"
	if _, err := open(t, k, other, hsmkeys.Options{}); err == nil {
		t.Fatal("another version opened: the reference names exactly one")
	}
}

// A 2-of-3 gate decided by two approvers whose keys are in Key Vault (one HSM, one software), through
// the approval command with a key source that loads through hsmkeys; the gate re-verifies on resume.
func TestTwoOfThreeInKeyVault(t *testing.T) {
	k := newKV()
	refs := map[string]string{"ops": k.add(t, "ops", azkeys.KeyTypeECHSM), "finance": k.add(t, "finance", azkeys.KeyTypeEC)}
	soft := hsmkeys.Options{AllowSoftwareKey: true}
	enrol, pubs := map[string]string{}, map[string]audit.Verifier{}
	for id, ref := range refs {
		s, err := open(t, k, ref, soft)
		if err != nil {
			t.Fatal(err)
		}
		enrol[id] = audit.FormatPublicKey(s.Alg(), s.PublicKey())
		pubs[id], _ = audit.ParsePublicKey(enrol[id])
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
		Load:       func(ref string) (audit.Signer, error) { return open(t, k, ref, soft) },
		ReceiptDir: func(string) string { return dir },
	}
	for _, id := range []string{"ops", "finance"} {
		var out, errb bytes.Buffer
		code := approvecmd.MainWith([]string{"-db", db, "-color", "never", "-run", "refund-1049", "-call", "c2", "-as", id, "-key", refs[id], "-approver-keys", keysFile, "-yes"},
			strings.NewReader(""), &out, &errb, approvecmd.JournalStores(), src)
		if code != 0 || !strings.Contains(out.String(), "signed and recorded") || !strings.Contains(out.String(), "-protected") {
			t.Fatalf("%s: %d\n%s\n%s", id, code, out.String(), errb.String())
		}
	}
	if err := drive(); err != nil || charges.Load() != 1 {
		t.Fatalf("resume: %v, charges %d", err, charges.Load())
	}
}
