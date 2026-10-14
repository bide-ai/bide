//go:build cgo

package pkcs11

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	p11 "github.com/miekg/pkcs11"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/signer/pkcs11/pkcs11test"
)

// These tests run against SoftHSM2, a software PKCS#11 token. They skip when it is not installed
// (brew install softhsm; apt install softhsm2), unless BIDE_REQUIRE_SOFTHSM is set, which turns
// the skip into a failure so a CI job cannot silently test nothing. BIDE_SOFTHSM2_LIB and
// BIDE_SOFTHSM2_UTIL name the library and the softhsm2-util program when they are not in a
// standard place.

const (
	testUserPIN = "246810" // the PIN pkcs11test gives its token
)

var edKeys = []struct {
	label string
	id    byte
}{{"audit", 1}, {"approver", 2}, {"dup", 3}, {"dup", 4}}

func TestMain(m *testing.M) {
	os.Exit(pkcs11test.Run(m, func(tok *pkcs11test.Token) error {
		for _, k := range edKeys {
			if err := tok.GenerateEd25519(k.label, []byte{k.id}); err != nil {
				return err
			}
		}
		if err := tok.GenerateEd25519Extractable("extractable", []byte{6}); err != nil {
			return err
		}
		if err := tok.GenerateEd25519NoSign("nosign", []byte{5}); err != nil {
			return err
		}
		return tok.GenerateP256("p256")
	}))
}

var tok *pkcs11test.Token

// needSoftHSM skips the test when SoftHSM2 is not installed (see pkcs11test).
func needSoftHSM(t *testing.T) { t.Helper(); tok = pkcs11test.Require(t) }

func hsmConfig(keyLabel string) Config {
	return Config{Module: tok.Module, TokenLabel: tok.Label, KeyLabel: keyLabel, PIN: func() (string, error) { return tok.PIN, nil }}
}

func openTest(t *testing.T, c Config) *Signer {
	t.Helper()
	s, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSoftHSMSignAndVerify(t *testing.T) {
	needSoftHSM(t)
	s := openTest(t, hsmConfig("audit"))
	v, err := audit.VerifierOf(s)
	if err != nil {
		t.Fatalf("public key read from the token is not a bide Ed25519 key: %v", err)
	}
	for _, msg := range [][]byte{{}, []byte("a"), bytes.Repeat([]byte("evidence "), 20000)} {
		sig, err := s.Sign(msg)
		if err != nil {
			t.Fatalf("sign %d bytes: %v", len(msg), err)
		}
		if len(sig) != ed25519.SignatureSize {
			t.Fatalf("signature is %d bytes", len(sig))
		}
		if !v.Verify(msg, sig) {
			t.Fatalf("audit verifier rejects the token's signature over %d bytes", len(msg))
		}
		// Pure Ed25519 (RFC 8032): the standard library agrees, so no prehash or context is hidden.
		if !ed25519.Verify(s.PublicKey(), msg, sig) {
			t.Fatal("crypto/ed25519 rejects the token's signature")
		}
	}
	if got := v.KeyIDs(); len(got) != 1 || got[0] != audit.KeyID("ed25519", s.PublicKey()) {
		t.Fatalf("KeyIDs = %v", got)
	}
}

func TestSoftHSMSelectsKeys(t *testing.T) {
	needSoftHSM(t)
	byLabel := openTest(t, hsmConfig("audit"))
	byID := openTest(t, Config{Module: tok.Module, TokenLabel: tok.Label, KeyID: []byte{1}, PIN: hsmConfig("").PIN})
	both := openTest(t, Config{Module: tok.Module, TokenLabel: tok.Label, KeyLabel: "audit", KeyID: []byte{1}, PIN: hsmConfig("").PIN})
	other := openTest(t, hsmConfig("approver"))
	if !bytes.Equal(byLabel.PublicKey(), byID.PublicKey()) || !bytes.Equal(byLabel.PublicKey(), both.PublicKey()) {
		t.Fatal("label and ID select different keys")
	}
	if bytes.Equal(byLabel.PublicKey(), other.PublicKey()) {
		t.Fatal("two keys share a public key")
	}
	// The token's slot number selects it too.
	slots := slotOf(t)
	bySlot := openTest(t, Config{Module: tok.Module, SlotID: &slots, KeyLabel: "audit", PIN: hsmConfig("").PIN})
	if !bytes.Equal(byLabel.PublicKey(), bySlot.PublicKey()) {
		t.Fatal("slot selection found another key")
	}
}

func slotOf(t *testing.T) uint {
	t.Helper()
	ctx := p11.New(tok.Module)
	if err := ctx.Initialize(); err != nil && !isErr(err, p11.CKR_CRYPTOKI_ALREADY_INITIALIZED) {
		t.Fatal(err)
	}
	slots, err := ctx.GetSlotList(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range slots {
		if ti, err := ctx.GetTokenInfo(s); err == nil && strings.TrimSpace(ti.Label) == tok.Label {
			return s
		}
	}
	t.Fatal("test token not listed")
	return 0
}

func TestSoftHSMRefusals(t *testing.T) {
	needSoftHSM(t)
	badPIN := func() (string, error) { return "000000", nil }
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want error
	}{
		{"unknown label", func(c *Config) { c.KeyLabel = "nope" }, ErrKeyNotFound},
		{"ambiguous label", func(c *Config) { c.KeyLabel = "dup" }, ErrAmbiguous},
		{"label and wrong ID", func(c *Config) { c.KeyID = []byte{2} }, ErrKeyNotFound},
		{"key that may not sign", func(c *Config) { c.KeyLabel = "nosign" }, agent.ErrConfig},
		{"P-256 key, not Ed25519", func(c *Config) { c.KeyLabel = "p256" }, ErrNoEd25519},
		{"unknown token", func(c *Config) { c.TokenLabel = "elsewhere" }, agent.ErrConfig},
		{"wrong PIN", func(c *Config) { c.PIN = badPIN }, CKRError{Code: CKRPinIncorrect}},
		{"missing module", func(c *Config) { c.Module = "/nonexistent/lib.so" }, agent.ErrConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := hsmConfig("audit")
			tc.mut(&c)
			s, err := Open(c)
			if err == nil {
				s.Close()
				t.Fatal("opened")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			for _, secret := range []string{testUserPIN, "000000"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error contains a PIN: %v", err)
				}
			}
		})
	}
}

func TestSoftHSMRequireNonExtractable(t *testing.T) {
	needSoftHSM(t)
	c := hsmConfig("audit")
	c.RequireNonExtractable = true
	openTest(t, c)
	c = hsmConfig("extractable")
	openTest(t, c) // an extractable key is fine unless the option is set
	c.RequireNonExtractable = true
	if _, err := Open(c); !errors.Is(err, ErrExtractable) {
		t.Fatalf("err = %v, want ErrExtractable", err)
	}
}

func TestSoftHSMDisjointKeysDoNotCross(t *testing.T) {
	needSoftHSM(t)
	a, b := openTest(t, hsmConfig("audit")), openTest(t, hsmConfig("approver"))
	msg := []byte("decision")
	sig, err := a.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	vb, _ := audit.VerifierOf(b)
	if vb.Verify(msg, sig) {
		t.Fatal("a signature by one key verifies under another")
	}
}

func TestSoftHSMConcurrentSigners(t *testing.T) {
	needSoftHSM(t)
	s1, s2 := openTest(t, hsmConfig("audit")), openTest(t, hsmConfig("audit"))
	var wg sync.WaitGroup
	for i := range 32 {
		s := s1
		if i%2 == 1 {
			s = s2
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := []byte{byte(i), 1, 2}
			sig, err := s.Sign(m)
			if err != nil || !ed25519.Verify(s.PublicKey(), m, sig) {
				t.Errorf("sign %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	// Closing one signer leaves the shared module usable for the other.
	s1.Close()
	if _, err := s2.Sign([]byte("still works")); err != nil {
		t.Fatalf("after closing a sibling signer: %v", err)
	}
}

func TestSoftHSMReopenAfterSessionLoss(t *testing.T) {
	needSoftHSM(t)
	s := openTest(t, hsmConfig("audit"))
	if _, err := s.Sign([]byte("before")); err != nil {
		t.Fatal(err)
	}
	// Close the session out from under the signer, as a token reset would.
	ts := s.sess.(*tokenSession)
	if err := ts.mod.ctx.CloseSession(ts.sh); err != nil {
		t.Fatal(err)
	}
	sig, err := s.Sign([]byte("after"))
	if err != nil {
		t.Fatalf("did not recover from a lost session: %v", err)
	}
	if !ed25519.Verify(s.PublicKey(), []byte("after"), sig) {
		t.Fatal("bad signature after recovery")
	}
}

func TestSoftHSMPINFromFile(t *testing.T) {
	needSoftHSM(t)
	f := filepath.Join(t.TempDir(), "pin")
	if err := os.WriteFile(f, []byte(testUserPIN+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := hsmConfig("audit")
	c.PIN = PINFromFile(f)
	openTest(t, c)
	t.Setenv("BIDE_TEST_HSM_PIN", testUserPIN)
	c.PIN = PINFromEnv("BIDE_TEST_HSM_PIN")
	openTest(t, c)
}

// A signer on the token anchors a real run: the audit store signs tree heads with it, and the
// heads verify under the verifier built from the public key alone.
func TestSoftHSMAuditedStoreAnchorAndEvidence(t *testing.T) {
	needSoftHSM(t)
	ctx := context.Background()
	s := openTest(t, hsmConfig("audit"))
	anchor := audit.NewMemAnchorLog()
	store, err := audit.NewAuditedStore(agent.NewMemStore(), s, anchor)
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(store)
	refund := agent.MustFunc("refund", "refund a payment", func(context.Context, struct{}) (string, error) { return "refunded", nil })
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.ToolTurn("call-1", "refund", `{}`), agenttest.TextTurn("done")), j, agent.WithTools(refund))
	if _, err := a.Run(ctx, "run-hsm", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}

	// A verifier that holds only the public key, as an auditor does.
	v, err := audit.ParsePublicKey(audit.FormatPublicKey(audit.AlgEd25519, s.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	entries := anchor.Entries()
	if len(entries) == 0 {
		t.Fatal("nothing was anchored")
	}
	root, err := audit.Root(ctx, j, "run-hsm")
	if err != nil {
		t.Fatal(err)
	}
	sth := entries[len(entries)-1].STH
	if err := sth.Verify(v); err != nil {
		t.Fatalf("anchored tree head does not verify: %v", err)
	}
	if !bytes.Equal(sth.Root, root) {
		t.Fatal("anchored root is not the journal root")
	}

	// An evidence package sealed by the token verifies as a whole.
	ev, err := audit.Evidence(ctx, j, "run-hsm", s, 1700000000, audit.WithLabel("hsm run"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Seal(s); err != nil {
		t.Fatal(err)
	}
	rep, err := ev.Verify(v)
	if err != nil || !rep.OK {
		t.Fatalf("evidence verification: %+v, %v", rep, err)
	}
	if len(ev.Actions) == 0 {
		t.Fatal("the evidence proves no tool call")
	}
	// Another key's verifier must not accept the seal.
	other := openTest(t, hsmConfig("approver"))
	ov, _ := audit.VerifierOf(other)
	if _, err := ev.Verify(ov); err == nil {
		t.Fatal("evidence verified under a different key")
	}
	// Tampering after the seal is caught.
	ev.Signature[0] ^= 1
	if _, err := ev.Verify(v); err == nil {
		t.Fatal("tampered seal verified")
	}
}

func TestSoftHSMApproverDecisionSignature(t *testing.T) {
	needSoftHSM(t)
	s := openTest(t, hsmConfig("approver"))
	subject := agent.ApprovalSubject{RunID: "run-1", ToolUseID: "call-1", ToolName: "refund", Args: json.RawMessage(`{"amount":10}`)}
	decidedAt := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	msg := agent.ApprovalDecisionBytes(subject, "finance", true, decidedAt)
	sig, err := s.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	var v agent.ApproverVerifier
	v, err = audit.VerifierOf(s)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Verify(msg, sig) {
		t.Fatal("approver verifier rejects the HSM-signed decision")
	}
	if v.Verify(agent.ApprovalDecisionBytes(subject, "finance", false, decidedAt), sig) {
		t.Fatal("decision signature verifies over an opposite decision")
	}
}

// The failures a fresh session cures are told apart from the ones it does not.
func TestClassify(t *testing.T) {
	for _, code := range []uint{
		p11.CKR_SESSION_HANDLE_INVALID, p11.CKR_SESSION_CLOSED, p11.CKR_DEVICE_REMOVED,
		p11.CKR_TOKEN_NOT_PRESENT, p11.CKR_USER_NOT_LOGGED_IN,
	} {
		if err := classify(p11.Error(code)); !errors.Is(err, errSessionLost) {
			t.Errorf("%v is not a lost session", p11.Error(code))
		}
	}
	for _, code := range []uint{p11.CKR_FUNCTION_FAILED, p11.CKR_KEY_FUNCTION_NOT_PERMITTED, p11.CKR_DATA_LEN_RANGE} {
		if err := classify(p11.Error(code)); errors.Is(err, errSessionLost) {
			t.Errorf("%v is classified as a lost session", p11.Error(code))
		}
	}
}
