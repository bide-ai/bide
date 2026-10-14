//go:build cgo

// Package pkcs11test gives tests a SoftHSM2 token to sign with, so code that uses the signer
// package can be tested against a real PKCS#11 library with no hardware.
//
// SoftHSM2 reads its configuration once per process, so a test binary creates its token in
// TestMain and shares it:
//
//	func TestMain(m *testing.M) {
//		os.Exit(pkcs11test.Run(m, func(tok *pkcs11test.Token) error {
//			return tok.GenerateEd25519("audit", []byte{1})
//		}))
//	}
//
//	func TestSomething(t *testing.T) {
//		tok := pkcs11test.Require(t) // skips when SoftHSM2 is not installed
//		s, err := pkcs11.Open(pkcs11.Config{
//			Module: tok.Module, TokenLabel: tok.Label, KeyLabel: "audit",
//			PIN: func() (string, error) { return tok.PIN, nil },
//		})
//		...
//	}
//
// Install SoftHSM2 with "brew install softhsm" or "apt install softhsm2". BIDE_SOFTHSM2_LIB and
// BIDE_SOFTHSM2_UTIL name the library and the softhsm2-util program when they are in a
// non-standard place. With BIDE_REQUIRE_SOFTHSM set, a missing SoftHSM2 fails the test instead
// of skipping it, so a CI job cannot silently test nothing.
package pkcs11test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	p11 "github.com/miekg/pkcs11"
)

const (
	tokenLabel = "bide-test"
	userPIN    = "246810"
	soPIN      = "13579135"

	ckmECEdwardsKeyGen = 0x1055 // CKM_EC_EDWARDS_KEY_PAIR_GEN
)

// Token is the SoftHSM2 token a test process shares.
type Token struct {
	// Module is the path of libsofthsm2.
	Module string
	// Label is the token's label.
	Label string
	// PIN is the token's user PIN (a fixed test value).
	PIN string
}

var (
	shared   *Token
	skipWhy  string
	required = os.Getenv("BIDE_REQUIRE_SOFTHSM") != ""
)

// Run is for TestMain: it creates a SoftHSM2 token in a scratch directory, calls provision with it
// so the test binary can put its keys on the token, runs the tests, and removes the directory. If
// SoftHSM2 is unavailable it still runs the tests, whose Require calls then skip. It returns the
// exit code for os.Exit.
func Run(m *testing.M, provision func(*Token) error) int {
	dir, err := os.MkdirTemp("", "bide-softhsm")
	if err != nil {
		skipWhy = err.Error()
		return m.Run()
	}
	defer os.RemoveAll(dir)
	tok, err := setup(dir)
	if err == nil && provision != nil {
		err = provision(tok)
	}
	if err != nil {
		skipWhy = err.Error()
	} else {
		shared = tok
	}
	return m.Run()
}

// Require returns the shared token, or skips the test (fails it, with BIDE_REQUIRE_SOFTHSM set)
// when SoftHSM2 is unavailable or the token could not be provisioned.
func Require(t testing.TB) *Token {
	t.Helper()
	if shared != nil {
		return shared
	}
	if required {
		t.Fatalf("SoftHSM2 is required (BIDE_REQUIRE_SOFTHSM) but unavailable: %s", skipWhy)
	}
	t.Skipf("SoftHSM2 unavailable: %s", skipWhy)
	return nil
}

func firstExisting(env string, candidates ...string) string {
	if p := os.Getenv(env); p != "" {
		return p
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func setup(dir string) (*Token, error) {
	lib := firstExisting("BIDE_SOFTHSM2_LIB",
		"/opt/homebrew/lib/softhsm/libsofthsm2.so", "/usr/local/lib/softhsm/libsofthsm2.so",
		"/usr/lib/softhsm/libsofthsm2.so", "/usr/lib/x86_64-linux-gnu/softhsm/libsofthsm2.so",
		"/usr/lib/aarch64-linux-gnu/softhsm/libsofthsm2.so", "/usr/lib64/pkcs11/libsofthsm2.so")
	if lib == "" {
		return nil, errors.New("the SoftHSM2 library was not found")
	}
	util := os.Getenv("BIDE_SOFTHSM2_UTIL")
	if util == "" {
		if p, err := exec.LookPath("softhsm2-util"); err == nil {
			util = p
		} else if _, err := os.Stat("/opt/homebrew/bin/softhsm2-util"); err == nil {
			util = "/opt/homebrew/bin/softhsm2-util"
		} else {
			return nil, errors.New("softhsm2-util was not found")
		}
	}
	tokens := filepath.Join(dir, "tokens")
	if err := os.Mkdir(tokens, 0o700); err != nil {
		return nil, err
	}
	conf := filepath.Join(dir, "softhsm2.conf")
	cfg := fmt.Sprintf("directories.tokendir = %s\nobjectstore.backend = file\nlog.level = ERROR\nslots.removable = false\n", tokens)
	if err := os.WriteFile(conf, []byte(cfg), 0o600); err != nil {
		return nil, err
	}
	os.Setenv("SOFTHSM2_CONF", conf)
	if out, err := exec.Command(util, "--init-token", "--free", "--label", tokenLabel, "--so-pin", soPIN, "--pin", userPIN).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("softhsm2-util: %v: %s", err, out)
	}
	return &Token{Module: lib, Label: tokenLabel, PIN: userPIN}, nil
}

// withSession runs fn in a read-write session on the token, logged in as the user. It is for
// provisioning, before any signer has opened the module.
func (k *Token) withSession(fn func(*p11.Ctx, p11.SessionHandle) error) error {
	ctx := p11.New(k.Module)
	if ctx == nil {
		return errors.New("cannot load the SoftHSM2 library")
	}
	defer ctx.Destroy()
	if err := ctx.Initialize(); err != nil {
		return err
	}
	defer ctx.Finalize()
	slots, err := ctx.GetSlotList(true)
	if err != nil {
		return err
	}
	for _, s := range slots {
		ti, err := ctx.GetTokenInfo(s)
		if err != nil || strings.TrimSpace(ti.Label) != k.Label {
			continue
		}
		sh, err := ctx.OpenSession(s, p11.CKF_SERIAL_SESSION|p11.CKF_RW_SESSION)
		if err != nil {
			return err
		}
		defer ctx.CloseSession(sh)
		if err := ctx.Login(sh, p11.CKU_USER, k.PIN); err != nil {
			return err
		}
		return fn(ctx, sh)
	}
	return errors.New("the test token is not listed")
}

// GenerateEd25519 creates an Ed25519 key pair on the token with the label and CKA_ID. The private
// key is not extractable. Call it from the provision function given to Run.
func (k *Token) GenerateEd25519(label string, id []byte) error {
	return k.generateEd25519(label, id, true)
}

// GenerateEd25519NoSign creates an Ed25519 key pair whose private key may not sign (CKA_SIGN
// false), a key the signer must refuse.
func (k *Token) GenerateEd25519NoSign(label string, id []byte) error {
	return k.generateEd25519(label, id, false)
}

// GenerateEd25519Extractable creates an Ed25519 key pair whose private key is extractable and not
// sensitive, a key Config.RequireNonExtractable must refuse.
func (k *Token) GenerateEd25519Extractable(label string, id []byte) error {
	return k.generate(label, id, true, true)
}

func (k *Token) generateEd25519(label string, id []byte, canSign bool) error {
	return k.generate(label, id, canSign, false)
}

func (k *Token) generate(label string, id []byte, canSign, extractable bool) error {
	return k.generateWith(label, id, canSign, extractable, false)
}

// GenerateEd25519AlwaysAuth creates an Ed25519 key pair whose private key has CKA_ALWAYS_AUTHENTICATE
// true: each signature needs a context-specific login between C_SignInit and C_Sign.
func (k *Token) GenerateEd25519AlwaysAuth(label string, id []byte) error {
	return k.generateWith(label, id, true, false, true)
}

func (k *Token) generateWith(label string, id []byte, canSign, extractable, alwaysAuth bool) error {
	return k.withSession(func(ctx *p11.Ctx, sh p11.SessionHandle) error {
		pub := []*p11.Attribute{
			p11.NewAttribute(p11.CKA_EC_PARAMS, []byte{0x06, 0x03, 0x2b, 0x65, 0x70}), p11.NewAttribute(p11.CKA_VERIFY, true),
			p11.NewAttribute(p11.CKA_TOKEN, true), p11.NewAttribute(p11.CKA_LABEL, label), p11.NewAttribute(p11.CKA_ID, id),
		}
		priv := []*p11.Attribute{
			p11.NewAttribute(p11.CKA_SIGN, canSign), p11.NewAttribute(p11.CKA_TOKEN, true), p11.NewAttribute(p11.CKA_PRIVATE, true),
			p11.NewAttribute(p11.CKA_SENSITIVE, !extractable), p11.NewAttribute(p11.CKA_EXTRACTABLE, extractable),
			p11.NewAttribute(p11.CKA_LABEL, label), p11.NewAttribute(p11.CKA_ID, id),
		}
		if alwaysAuth {
			priv = append(priv, p11.NewAttribute(p11.CKA_ALWAYS_AUTHENTICATE, true))
		}
		_, _, err := ctx.GenerateKeyPair(sh, []*p11.Mechanism{p11.NewMechanism(ckmECEdwardsKeyGen, nil)}, pub, priv)
		if err != nil {
			return fmt.Errorf("generate Ed25519 key %q (does this SoftHSM2 build support EdDSA?): %w", label, err)
		}
		return nil
	})
}

// GenerateP256 creates an ECDSA P-256 key pair on the token, a key the signer must refuse.
func (k *Token) GenerateP256(label string) error {
	return k.withSession(func(ctx *p11.Ctx, sh p11.SessionHandle) error {
		p256 := []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}
		_, _, err := ctx.GenerateKeyPair(sh, []*p11.Mechanism{p11.NewMechanism(p11.CKM_EC_KEY_PAIR_GEN, nil)},
			[]*p11.Attribute{p11.NewAttribute(p11.CKA_EC_PARAMS, p256), p11.NewAttribute(p11.CKA_VERIFY, true), p11.NewAttribute(p11.CKA_TOKEN, true), p11.NewAttribute(p11.CKA_LABEL, label)},
			[]*p11.Attribute{p11.NewAttribute(p11.CKA_SIGN, true), p11.NewAttribute(p11.CKA_TOKEN, true), p11.NewAttribute(p11.CKA_PRIVATE, true), p11.NewAttribute(p11.CKA_LABEL, label)})
		return err
	})
}

// GenerateP256Key creates an ECDSA P-256 key pair with the label and CKA_ID whose private key is
// sensitive and not extractable, the key an approver or a signer should keep on a token.
func (k *Token) GenerateP256Key(label string, id []byte) error {
	return k.generateP256(label, id, false)
}

// GenerateP256Extractable creates an ECDSA P-256 key pair whose private key is extractable and not
// sensitive, a key Config.RequireNonExtractable must refuse.
func (k *Token) GenerateP256Extractable(label string, id []byte) error {
	return k.generateP256(label, id, true)
}

func (k *Token) generateP256(label string, id []byte, extractable bool) error {
	return k.withSession(func(ctx *p11.Ctx, sh p11.SessionHandle) error {
		p256 := []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}
		_, _, err := ctx.GenerateKeyPair(sh, []*p11.Mechanism{p11.NewMechanism(p11.CKM_EC_KEY_PAIR_GEN, nil)},
			[]*p11.Attribute{p11.NewAttribute(p11.CKA_EC_PARAMS, p256), p11.NewAttribute(p11.CKA_VERIFY, true), p11.NewAttribute(p11.CKA_TOKEN, true), p11.NewAttribute(p11.CKA_LABEL, label), p11.NewAttribute(p11.CKA_ID, id)},
			[]*p11.Attribute{p11.NewAttribute(p11.CKA_SIGN, true), p11.NewAttribute(p11.CKA_TOKEN, true), p11.NewAttribute(p11.CKA_PRIVATE, true),
				p11.NewAttribute(p11.CKA_SENSITIVE, !extractable), p11.NewAttribute(p11.CKA_EXTRACTABLE, extractable),
				p11.NewAttribute(p11.CKA_LABEL, label), p11.NewAttribute(p11.CKA_ID, id)})
		return err
	})
}

// GenerateEd25519Remarked creates an Ed25519 key pair that is extractable and not sensitive, then
// marks the private key CKA_EXTRACTABLE=false and CKA_SENSITIVE=true, the only direction a token
// allows. The token now reports the key as non-extractable and sensitive, but keeps
// CKA_NEVER_EXTRACTABLE and CKA_ALWAYS_SENSITIVE false: the key could have been copied out before
// it was marked, and Config.RequireNonExtractable must refuse it.
func (k *Token) GenerateEd25519Remarked(label string, id []byte) error {
	if err := k.generate(label, id, true, true); err != nil {
		return err
	}
	return k.withSession(func(ctx *p11.Ctx, sh p11.SessionHandle) error {
		tmpl := []*p11.Attribute{p11.NewAttribute(p11.CKA_CLASS, p11.CKO_PRIVATE_KEY), p11.NewAttribute(p11.CKA_LABEL, label), p11.NewAttribute(p11.CKA_ID, id)}
		if err := ctx.FindObjectsInit(sh, tmpl); err != nil {
			return err
		}
		objs, _, err := ctx.FindObjects(sh, 2)
		ctx.FindObjectsFinal(sh)
		if err != nil || len(objs) != 1 {
			return fmt.Errorf("find the key %q to re-mark: %v (%d objects)", label, err, len(objs))
		}
		return ctx.SetAttributeValue(sh, objs[0], []*p11.Attribute{p11.NewAttribute(p11.CKA_EXTRACTABLE, false), p11.NewAttribute(p11.CKA_SENSITIVE, true)})
	})
}
