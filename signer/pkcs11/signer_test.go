package pkcs11

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// fakeToken is a token in memory: it signs with a software key, and can be told to fail.
type fakeToken struct {
	mu       sync.Mutex
	priv     ed25519.PrivateKey
	opens    int
	failNext []error // errors the next signs return, in order
	badSig   bool    // sign with another key
	pubHide  bool    // do not expose the public key
	seenPIN  string
	openErr  error
}

func newFakeToken(t *testing.T) *fakeToken {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeToken{priv: priv}
}

func (f *fakeToken) open(_ Config, pin string) (session, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	f.seenPIN = pin
	if f.openErr != nil {
		return nil, nil, f.openErr
	}
	var pub []byte
	if !f.pubHide {
		pub = f.priv.Public().(ed25519.PublicKey)
	}
	return &fakeSession{tok: f}, pub, nil
}

type fakeSession struct {
	tok    *fakeToken
	closed bool
}

func (s *fakeSession) sign(m []byte, _ func() (string, error)) ([]byte, error) {
	s.tok.mu.Lock()
	defer s.tok.mu.Unlock()
	if len(s.tok.failNext) > 0 {
		err := s.tok.failNext[0]
		s.tok.failNext = s.tok.failNext[1:]
		return nil, err
	}
	if s.tok.badSig {
		_, other, _ := ed25519.GenerateKey(nil)
		return ed25519.Sign(other, m), nil
	}
	return ed25519.Sign(s.tok.priv, m), nil
}

func (s *fakeSession) close() error { s.closed = true; return nil }

func goodConfig() Config {
	return Config{Module: "/fake/module.so", TokenLabel: "bide", KeyLabel: "audit", PIN: func() (string, error) { return "s3cret-pin", nil }}
}

func TestSignVerifiesUnderBideVerifier(t *testing.T) {
	tok := newFakeToken(t)
	s, err := openWith(goodConfig(), tok.open)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Alg() != audit.AlgEd25519 {
		t.Fatalf("Alg = %s", s.Alg())
	}
	v, err := audit.VerifierOf(s)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("tree head bytes")
	sig, err := s.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Verify(msg, sig) {
		t.Fatal("bide's verifier rejects the token's signature")
	}
	if v.Verify([]byte("other"), sig) {
		t.Fatal("verifier accepted a signature over another message")
	}
	if tok.seenPIN != "s3cret-pin" {
		t.Fatalf("PIN passed to the token = %q", tok.seenPIN)
	}
}

func TestConfigValidation(t *testing.T) {
	slot := uint(1)
	for name, c := range map[string]Config{
		"no module":        {TokenLabel: "t", KeyLabel: "k"},
		"no token":         {Module: "m", KeyLabel: "k"},
		"token and slot":   {Module: "m", TokenLabel: "t", SlotID: &slot, KeyLabel: "k"},
		"no key":           {Module: "m", TokenLabel: "t"},
		"short public key": {Module: "m", TokenLabel: "t", KeyLabel: "k", PublicKey: []byte{1}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := openWith(c, func(Config, string) (session, []byte, error) {
				t.Fatal("opened a session for an invalid config")
				return nil, nil, nil
			})
			if !errors.Is(err, agent.ErrConfig) {
				t.Fatalf("err = %v, want agent.ErrConfig", err)
			}
		})
	}
	if _, err := openWith(Config{Module: "m", SlotID: &slot, KeyID: []byte{1}}, newFakeToken(t).open); err != nil {
		t.Fatalf("slot + key ID config refused: %v", err)
	}
}

func TestPublicKeyOverride(t *testing.T) {
	tok := newFakeToken(t)
	pub := tok.priv.Public().(ed25519.PublicKey)

	tok.pubHide = true
	if _, err := openWith(goodConfig(), tok.open); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("a token that hides its public key and no Config.PublicKey: err = %v", err)
	}
	c := goodConfig()
	c.PublicKey = pub
	s, err := openWith(c, tok.open)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.PublicKey(), pub) {
		t.Fatal("public key is not the configured one")
	}
	s.Close()

	// A configured key that differs from the one the token exposes is refused.
	tok.pubHide = false
	other, _, _ := ed25519.GenerateKey(nil)
	c.PublicKey = other
	if _, err := openWith(c, tok.open); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("mismatched Config.PublicKey: err = %v", err)
	}
}

func TestWeakPublicKeyRefused(t *testing.T) {
	tok := newFakeToken(t)
	tok.pubHide = true
	c := goodConfig()
	identity := make([]byte, ed25519.PublicKeySize)
	identity[0] = 1 // the identity point: a small-order key
	c.PublicKey = identity
	if _, err := openWith(c, tok.open); !errors.Is(err, audit.ErrMalformed) {
		t.Fatalf("small-order key: err = %v, want audit.ErrMalformed", err)
	}
}

func TestSignerRefusesTokenSignatureThatDoesNotVerify(t *testing.T) {
	tok := newFakeToken(t)
	s, err := openWith(goodConfig(), tok.open)
	if err != nil {
		t.Fatal(err)
	}
	tok.badSig = true
	if sig, err := s.Sign([]byte("m")); err == nil {
		t.Fatalf("returned a signature the public key does not verify: %x", sig)
	}
}

func TestSessionLostIsReopenedOnce(t *testing.T) {
	tok := newFakeToken(t)
	s, err := openWith(goodConfig(), tok.open)
	if err != nil {
		t.Fatal(err)
	}
	tok.failNext = []error{fmt.Errorf("%w: handle", errSessionLost)}
	sig, err := s.Sign([]byte("m"))
	if err != nil {
		t.Fatalf("one lost session should be retried: %v", err)
	}
	if !ed25519.Verify(s.PublicKey(), []byte("m"), sig) {
		t.Fatal("bad signature after reopen")
	}
	if tok.opens != 2 {
		t.Fatalf("opens = %d, want 2", tok.opens)
	}

	// A second loss in a row is returned, not retried forever.
	tok.failNext = []error{errSessionLost, errSessionLost}
	if _, err := s.Sign([]byte("m")); !errors.Is(err, errSessionLost) {
		t.Fatalf("two lost sessions: err = %v", err)
	}
	if tok.opens != 3 {
		t.Fatalf("opens = %d, want 3", tok.opens)
	}
	// The signer recovers on the next call.
	if _, err := s.Sign([]byte("m")); err != nil {
		t.Fatalf("after a failed call: %v", err)
	}
}

func TestOtherSignErrorIsNotRetried(t *testing.T) {
	tok := newFakeToken(t)
	s, _ := openWith(goodConfig(), tok.open)
	boom := errors.New("CKR_FUNCTION_FAILED")
	tok.failNext = []error{boom}
	if _, err := s.Sign([]byte("m")); err == nil || !strings.Contains(err.Error(), "CKR_FUNCTION_FAILED") || errors.Is(err, errSessionLost) {
		t.Fatalf("err = %v", err)
	}
	if tok.opens != 1 {
		t.Fatalf("opens = %d: a non-session error reopened the session", tok.opens)
	}
}

func TestReopenRefusesADifferentKey(t *testing.T) {
	tok := newFakeToken(t)
	s, _ := openWith(goodConfig(), tok.open)
	_, replaced, _ := ed25519.GenerateKey(nil)
	tok.failNext = []error{errSessionLost}
	tok.priv = replaced // the label now names another key
	if _, err := s.Sign([]byte("m")); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("err = %v, want a refusal because the key changed", err)
	}
}

func TestCloseAndConcurrency(t *testing.T) {
	tok := newFakeToken(t)
	s, _ := openWith(goodConfig(), tok.open)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := []byte{byte(i)}
			sig, err := s.Sign(m)
			if err != nil || !ed25519.Verify(s.PublicKey(), m, sig) {
				t.Errorf("concurrent sign %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := s.Sign([]byte("m")); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Sign after Close: err = %v", err)
	}
}

func TestPINNeverAppears(t *testing.T) {
	tok := newFakeToken(t)
	tok.openErr = errors.New("login failed for pin s3cret-pin")
	_, err := openWith(goodConfig(), tok.open)
	if err == nil || strings.Contains(err.Error(), "s3cret-pin") {
		t.Fatalf("err = %v: must fail without the PIN in the message", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("err = %v", err)
	}
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if strings.Contains(e.Error(), "s3cret-pin") {
			t.Fatalf("the PIN is in the chain: %q", e.Error())
		}
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				walk(c)
			}
		}
	}
	walk(err)
	if out := fmt.Sprintf("%#v", err); strings.Contains(out, "s3cret-pin") {
		t.Fatalf("%%#v leaks the PIN: %s", out)
	}

	// The sentinels survive the scrub, so callers can still branch on them.
	tok.openErr = fmt.Errorf("find key for pin s3cret-pin: %w", ErrKeyNotFound)
	if _, err := openWith(goodConfig(), tok.open); !errors.Is(err, ErrKeyNotFound) || strings.Contains(err.Error(), "s3cret-pin") {
		t.Fatalf("err = %v", err)
	}

	tok.openErr = nil
	s, _ := openWith(goodConfig(), tok.open)
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%v", goodConfig().Module)} {
		if strings.Contains(out, "s3cret") || strings.Contains(out, "audit") && strings.Contains(out, "TokenLabel") {
			t.Fatalf("formatted signer leaks configuration: %s", out)
		}
	}
	if !strings.HasPrefix(fmt.Sprintf("%+v", s), "pkcs11.Signer{ed25519:") {
		t.Fatalf("String = %s", s)
	}
}

// A vendor library may print the PIN quoted or escaped.
func TestPINScrubbedInQuotedForms(t *testing.T) {
	const pin = `pa"ss\wörd`
	for name, msg := range map[string]string{
		"%q":    fmt.Sprintf("login failed for %q", pin),
		"json":  `{"pin":"pa\"ss\\wörd"}`,
		"ascii": fmt.Sprintf("login failed for %+q", pin),
		"as is": "login failed for " + pin,
	} {
		tok := newFakeToken(t)
		tok.openErr = errors.New(msg)
		cfg := goodConfig()
		cfg.PIN = func() (string, error) { return pin, nil }
		_, err := openWith(cfg, tok.open)
		if err == nil || strings.Contains(err.Error(), `ss\`) || strings.Contains(err.Error(), "ss\\") || strings.Contains(err.Error(), "wörd") || strings.Contains(err.Error(), `w\u00f6rd`) {
			t.Errorf("%s: PIN in error: %v", name, err)
		}
	}
}

// Open signs once, so a private key that is not the exposed public key's pair is found there.
func TestOpenProvesTheKeyPair(t *testing.T) {
	tok := newFakeToken(t)
	tok.badSig = true
	if _, err := openWith(goodConfig(), tok.open); err == nil {
		t.Fatal("Open accepted a token whose private key does not match its public key")
	}
	tok.badSig = false
	tok.failNext = []error{errors.New("CKR_FUNCTION_FAILED")}
	if _, err := openWith(goodConfig(), tok.open); err == nil {
		t.Fatal("Open ignored a failing self-test signature")
	}
}

func TestPINSources(t *testing.T) {
	t.Setenv("BIDE_TEST_PIN", "1234")
	if p, err := PINFromEnv("BIDE_TEST_PIN")(); err != nil || p != "1234" {
		t.Fatalf("env: %q, %v", p, err)
	}
	if _, err := PINFromEnv("BIDE_TEST_PIN_UNSET")(); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "BIDE_TEST_PIN_UNSET") {
		t.Fatalf("unset env: %v", err)
	}

	dir := t.TempDir()
	f := filepath.Join(dir, "pin")
	for in, want := range map[string]string{"5678\n": "5678", "5678\r\n": "5678", "5678": "5678", "56 78\n\n": "56 78\n"} {
		if err := os.WriteFile(f, []byte(in), 0o600); err != nil {
			t.Fatal(err)
		}
		if p, err := PINFromFile(f)(); err != nil || p != want {
			t.Fatalf("file %q: %q, %v", in, p, err)
		}
	}
	os.WriteFile(f, []byte("\n"), 0o600)
	if _, err := PINFromFile(f)(); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("empty file: %v", err)
	}
	missing := PINFromFile(filepath.Join(dir, "nope"))
	if _, err := missing(); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestPINSourceErrorStopsOpen(t *testing.T) {
	c := goodConfig()
	c.PIN = func() (string, error) { return "", errors.New("vault down") }
	_, err := openWith(c, func(Config, string) (session, []byte, error) {
		t.Fatal("opened without a PIN")
		return nil, nil, nil
	})
	if err == nil {
		t.Fatal("no error")
	}
}

func TestParseECPoint(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	for name, in := range map[string][]byte{"raw": key, "der": append([]byte{0x04, 0x20}, key...)} {
		got, err := parseECPoint(in)
		if err != nil || !bytes.Equal(got, key) {
			t.Fatalf("%s: %x, %v", name, got, err)
		}
	}
	for _, bad := range [][]byte{nil, key[:31], append([]byte{0x04, 0x21}, key...), append([]byte{0x05, 0x20}, key...), append(key, 0)} {
		if _, err := parseECPoint(bad); err == nil {
			t.Fatalf("accepted %x", bad)
		}
	}
}

func TestParseECPointEd448(t *testing.T) {
	ed448 := bytes.Repeat([]byte{7}, 57)
	for _, in := range [][]byte{ed448, append([]byte{0x04, 0x39}, ed448...)} {
		if _, err := parseECPoint(in); !errors.Is(err, ErrNoEd25519) {
			t.Fatalf("%d bytes: err = %v, want ErrNoEd25519", len(in), err)
		}
	}
}

func TestIsEd25519Params(t *testing.T) {
	if !isEd25519Params([]byte{0x06, 0x03, 0x2b, 0x65, 0x70}) || !isEd25519Params(append([]byte{0x13, 0x0c}, "edwards25519"...)) {
		t.Fatal("rejected a valid Ed25519 parameter encoding")
	}
	p256 := []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}
	if isEd25519Params(p256) || isEd25519Params(nil) {
		t.Fatal("accepted P-256 or empty parameters")
	}
}

type codeErr uint

func (e codeErr) Error() string { return "vendor text with pin s3cret-pin" }

// A caller can branch on the PKCS#11 return code, which is all that survives of the library's error.
func TestCKRErrorSurvivesScrubbing(t *testing.T) {
	old := ckrCodeOf
	t.Cleanup(func() { ckrCodeOf = old })
	ckrCodeOf = func(err error) (uint, bool) {
		var c codeErr
		if errors.As(err, &c) {
			return uint(c), true
		}
		return 0, false
	}
	tok := newFakeToken(t)
	tok.openErr = fmt.Errorf("login: %w", codeErr(CKRPinLocked))
	_, err := openWith(goodConfig(), tok.open)
	if !errors.Is(err, CKRError{Code: CKRPinLocked}) || errors.Is(err, CKRError{Code: CKRPinIncorrect}) {
		t.Fatalf("err = %v", err)
	}
	var c CKRError
	if !errors.As(err, &c) || c.Code != CKRPinLocked || strings.Contains(c.Error(), "s3cret-pin") || c.Error() != "pkcs11: CKR_PIN_LOCKED (0xA4)" {
		t.Fatalf("CKRError = %q", c.Error())
	}
	if strings.Contains(err.Error(), "s3cret-pin") {
		t.Fatalf("PIN in %q", err)
	}
	if got := (CKRError{Code: 0x1234}).Error(); got != "pkcs11: CKR 0x1234" {
		t.Fatalf("unnamed code: %q", got)
	}

	// The same holds for a failure while signing.
	tok = newFakeToken(t)
	s, _ := openWith(goodConfig(), tok.open)
	tok.failNext = []error{codeErr(CKRKeyHandleInvalid)}
	if _, err := s.Sign([]byte("m")); !errors.Is(err, CKRError{Code: CKRKeyHandleInvalid}) {
		t.Fatalf("sign err = %v", err)
	}
}

// p256Token is a token with an ECDSA P-256 key; its sessions return the canonical signature, as
// the real session does.
type p256Token struct{ signer audit.ECDSAP256Signer }

func newP256Token(t *testing.T) *p256Token {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &p256Token{audit.ECDSAP256Signer{Priv: k}}
}

func (p *p256Token) open(Config, string) (session, []byte, error) {
	return p256Session{p.signer}, p.signer.PublicKey(), nil
}

type p256Session struct{ s audit.ECDSAP256Signer }

func (s p256Session) sign(m []byte, _ func() (string, error)) ([]byte, error) { return s.s.Sign(m) }
func (p256Session) close() error                                              { return nil }

func TestP256Scheme(t *testing.T) {
	tok := newP256Token(t)
	cfg := goodConfig()
	cfg.Scheme = audit.AlgECDSAP256
	s, err := openWith(cfg, tok.open)
	if err != nil {
		t.Fatal(err)
	}
	if s.Alg() != audit.AlgECDSAP256 || len(s.PublicKey()) != 65 {
		t.Fatalf("Alg %s, key %d bytes", s.Alg(), len(s.PublicKey()))
	}
	v, err := audit.VerifierOf(s)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := s.Sign([]byte("m"))
	if err != nil || !v.Verify([]byte("m"), sig) {
		t.Fatalf("sig %x, err %v", sig, err)
	}
	if got := v.KeyIDs(); len(got) != 1 || got[0] != audit.KeyID("ecdsa-p256", s.PublicKey()) {
		t.Fatalf("KeyIDs = %v", got)
	}
	if !strings.HasPrefix(s.String(), "pkcs11.Signer{ecdsa-p256:") {
		t.Fatalf("String = %s", s)
	}

	// A token that exposes an Ed25519 key under the P-256 scheme is refused.
	ed := newFakeToken(t)
	if _, err := openWith(cfg, ed.open); err == nil {
		t.Fatal("an Ed25519 public key was accepted as P-256")
	}
	// A configured public key must be the scheme's size.
	cfg.PublicKey = make([]byte, 32)
	if _, err := openWith(cfg, tok.open); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("32-byte Config.PublicKey under P-256: err = %v", err)
	}
	cfg.PublicKey = nil
	cfg.Scheme = "rsa"
	if _, err := openWith(cfg, tok.open); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("unknown scheme: err = %v", err)
	}
}

func TestParsePointP256(t *testing.T) {
	tok := newP256Token(t)
	pt := tok.signer.PublicKey()
	for name, in := range map[string][]byte{"raw": pt, "der": append([]byte{0x04, 0x41}, pt...)} {
		got, err := parsePoint(audit.AlgECDSAP256, in)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, bad := range [][]byte{nil, pt[:64], append([]byte{0x04, 0x40}, pt...), bytes.Repeat([]byte{7}, 32)} {
		if _, err := parsePoint(audit.AlgECDSAP256, bad); !errors.Is(err, ErrNoP256) {
			t.Fatalf("accepted or wrong error for %x: %v", bad, err)
		}
	}
	if !isP256Params([]byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}) || isP256Params(ed25519OID) {
		t.Fatal("P-256 parameter check")
	}
}

// A PIN shorter than MinPINLength is refused before the token is touched, and is not shown: the
// scrubbing of error messages replaces the PIN by substring, which a one-character PIN would garble.
func TestShortPINIsRefused(t *testing.T) {
	for _, pin := range []string{"Q", "QZ", "QZq", ""} {
		tok := newFakeToken(t)
		c := goodConfig()
		c.PIN = func() (string, error) { return pin, nil }
		_, err := openWith(c, tok.open)
		if !errors.Is(err, agent.ErrConfig) || tok.opens != 0 {
			t.Fatalf("PIN of %d characters: err = %v, opens = %d", len(pin), err, tok.opens)
		}
		if pin != "" && strings.Contains(err.Error(), pin) {
			t.Fatalf("the error shows the PIN: %v", err)
		}
	}
	c := goodConfig()
	c.PIN = func() (string, error) { return "1234", nil }
	if _, err := openWith(c, newFakeToken(t).open); err != nil {
		t.Fatalf("a 4-character PIN: %v", err)
	}
}

// authSession is a key with CKA_ALWAYS_AUTHENTICATE: it signs only after the authentication the
// signer supplies between initialising the signature and signing, as a context-specific login.
type authSession struct {
	priv   ed25519.PrivateKey
	want   string // the PIN it accepts; "" for a protected path
	signs  int
	authed []string
}

func (s *authSession) demandsAuth() bool { return true }
func (s *authSession) close() error      { return nil }
func (s *authSession) sign(m []byte, auth func() (string, error)) ([]byte, error) {
	pin, err := auth()
	if err != nil {
		return nil, err
	}
	s.authed = append(s.authed, pin)
	if pin != s.want {
		return nil, fmt.Errorf("CKR_PIN_INCORRECT after %q", pin)
	}
	s.signs++
	return ed25519.Sign(s.priv, m), nil
}

func openAuth(t *testing.T, cfg Config, want string) (*Signer, *authSession, *int) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(nil)
	sess := &authSession{priv: priv, want: want}
	opens := 0
	s, err := openWith(cfg, func(Config, string) (session, []byte, error) {
		opens++
		return sess, priv.Public().(ed25519.PublicKey), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, sess, &opens
}

// A key that demands authentication for each signature is authenticated for each one, with the PIN
// source asked again, and is not signed with at Open (that would ask for a PIN to show nothing).
func TestAlwaysAuthenticateKeyIsAuthenticatedPerSignature(t *testing.T) {
	asked := 0
	c := goodConfig()
	c.PIN = func() (string, error) { asked++; return "246810", nil }
	s, sess, _ := openAuth(t, c, "246810")
	if sess.signs != 0 || len(sess.authed) != 0 || asked != 1 {
		t.Fatalf("after Open: %d signs, %d authentications, PIN asked %d times (once, for the login)", sess.signs, len(sess.authed), asked)
	}
	v, _ := audit.VerifierOf(s)
	for i := range 3 {
		sig, err := s.Sign([]byte("decision"))
		if err != nil || !v.Verify([]byte("decision"), sig) {
			t.Fatalf("signature %d: %v", i, err)
		}
	}
	if sess.signs != 3 || len(sess.authed) != 3 || asked != 4 {
		t.Fatalf("%d signs, %d authentications, PIN asked %d times; want 3, 3, 4", sess.signs, len(sess.authed), asked)
	}
}

func TestAlwaysAuthenticateWrongShortAndMissingPIN(t *testing.T) {
	// The token's rejection is reported, with the PIN scrubbed out of it.
	c := goodConfig()
	c.PIN = func() (string, error) { return "246810", nil }
	s, _, _ := openAuth(t, c, "135790")
	if _, err := s.Sign([]byte("m")); err == nil || strings.Contains(err.Error(), "246810") || !strings.Contains(err.Error(), "CKR_PIN_INCORRECT") {
		t.Fatalf("wrong PIN: %v", err)
	}
	// A PIN source that now gives a PIN too short is refused before the token sees it.
	short := false
	c.PIN = func() (string, error) {
		if short {
			return "Qz", nil
		}
		return "246810", nil
	}
	s, sess, _ := openAuth(t, c, "246810")
	short = true
	if _, err := s.Sign([]byte("m")); !errors.Is(err, agent.ErrConfig) || len(sess.authed) != 0 {
		t.Fatalf("short PIN: %v (%d authentications)", err, len(sess.authed))
	}
	// No PIN source and no protected path: a configuration error, not a signature attempt.
	c = goodConfig()
	c.PIN = nil
	s, sess, _ = openAuth(t, c, "")
	if _, err := s.Sign([]byte("m")); !errors.Is(err, agent.ErrConfig) || sess.signs != 0 {
		t.Fatalf("no PIN source: %v", err)
	}
	// A protected authentication path authenticates with no PIN.
	c = Config{Module: "m", TokenLabel: "t", KeyLabel: "k", ProtectedAuthPath: true}
	s, sess, _ = openAuth(t, c, "")
	if _, err := s.Sign([]byte("m")); err != nil || sess.signs != 1 || sess.authed[0] != "" {
		t.Fatalf("protected path: %v %q", err, sess.authed)
	}
}
