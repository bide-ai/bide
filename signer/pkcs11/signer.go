// Package pkcs11 is an audit.Signer whose private key lives in a PKCS#11 token: a hardware
// security module, a smart card, or SoftHSM2 for tests. bide hands the token each message and gets
// the signature back, so the application never holds the key. Whether the key can be exported from
// the token at all is the token's policy; Config.RequireNonExtractable makes Open check that the
// token marks the key non-extractable.
//
// It lives in its own Go module because the PKCS#11 library it uses (github.com/miekg/pkcs11)
// needs cgo and a vendor library loaded at run time. The core bide module stays pure Go and
// dependency-light, and a deployment that keeps its keys in software never builds this module.
//
// # What it signs with
//
// Config.Scheme picks the scheme, and the signer never signs with a different one in its place:
//
//   - audit.AlgEd25519 (the default): a CKK_EC_EDWARDS key and mechanism CKM_EDDSA (0x1057, PKCS#11
//     v3.0), used without parameters, which selects pure Ed25519 (RFC 8032), the scheme
//     audit.Ed25519Signer produces. A token that insists on CK_EDDSA_PARAMS for the pure variant is
//     not supported. Open reports ErrNoEd25519 when the token does not list the mechanism or the key
//     is of another type, an Ed448 key included.
//   - audit.AlgECDSAP256: a CKK_EC key on prime256v1 and mechanism CKM_ECDSA over the SHA-256 of the
//     message, for the HSMs that cannot do Ed25519. The token's raw r||s output is converted to
//     the canonical low-S form audit verifies. Open reports ErrNoP256 when the token does not list
//     CKM_ECDSA or the key is not a P-256 key.
//
// # Use
//
//	s, err := pkcs11.Open(pkcs11.Config{
//		Module:     "/usr/lib/softhsm/libsofthsm2.so",
//		TokenLabel: "bide",
//		KeyLabel:   "bide-audit-2026",
//		PIN:        pkcs11.PINFromEnv("BIDE_HSM_PIN"),
//	})
//	if err != nil { ... }
//	defer s.Close()
//	store, err := audit.NewAuditedStore(inner, s, anchor)
//
// The PIN is read from an environment variable or a file when the session opens (and again if the
// session has to be reopened); it is never a command-line argument, is not kept by the Signer,
// and does not appear in an error or in any formatted output of the Signer.
package pkcs11

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// PKCS#11 numbers that miekg/pkcs11 does not name (they come from PKCS#11 v3.0).
const (
	ckmEdDSA              = 0x1057 // CKM_EDDSA
	ckmECEdwardsKeyGen    = 0x1055 // CKM_EC_EDWARDS_KEY_PAIR_GEN
	ckkECEdwards          = 0x40   // CKK_EC_EDWARDS
	ed25519SigSize        = ed25519.SignatureSize
	ed25519PointSize      = ed25519.PublicKeySize
	maxSessionReopenTries = 1
)

// Errors a caller can test with errors.Is. Configuration errors also wrap agent.ErrConfig.
var (
	// ErrNoEd25519 means the token cannot do Ed25519: it does not list CKM_EDDSA, or the key
	// selected is not a CKK_EC_EDWARDS key.
	ErrNoEd25519 = errors.New("pkcs11: the token has no Ed25519 support")
	// ErrNoP256 means Config.Scheme is audit.AlgECDSAP256 and the token does not list CKM_ECDSA, or
	// the key selected is not an ECDSA P-256 key.
	ErrNoP256 = errors.New("pkcs11: the token has no ECDSA P-256 support")
	// ErrKeyNotFound means no private key on the token matches the label and ID in Config.
	ErrKeyNotFound = errors.New("pkcs11: no matching signing key on the token")
	// ErrAmbiguous means more than one token or key matches the Config; add a label, an ID or a
	// slot so that exactly one does.
	ErrAmbiguous = errors.New("pkcs11: the selection matches more than one object")
	// ErrNoCgo is returned by Open in a build without cgo, where no PKCS#11 library can be loaded.
	ErrNoCgo = errors.New("pkcs11: this build has no cgo, so it cannot load a PKCS#11 module")
	// ErrExtractable means Config.RequireNonExtractable is set and the token does not mark the key
	// CKA_EXTRACTABLE=false and CKA_SENSITIVE=true.
	ErrExtractable = errors.New("pkcs11: the key is not marked non-extractable and sensitive")

	// errSessionLost marks a failure that opening a fresh session may cure (the session handle is
	// stale, the token was removed and returned, or the login lapsed).
	errSessionLost = errors.New("pkcs11: the session was lost")
)

// selfTest is the message signed once when a session opens.
const selfTest = "bide.signer.selftest.v1"

// CKRError is a PKCS#11 return code (CKR_*) the token or its library reported. It carries the code
// and nothing else, so it is safe to log, and a caller can branch on it with errors.Is or
// errors.As, for example errors.Is(err, pkcs11.CKRError{Code: pkcs11.CKRPinLocked}). The message
// text a vendor library attached to the failure is not kept.
type CKRError struct {
	Code uint
}

// The CKR codes a caller is likely to act on (PKCS#11 v2.40 / v3.0 values).
const (
	CKRDeviceRemoved        = 0x00000032 // CKR_DEVICE_REMOVED
	CKRKeyHandleInvalid     = 0x00000060 // CKR_KEY_HANDLE_INVALID
	CKRPinIncorrect         = 0x000000A0 // CKR_PIN_INCORRECT
	CKRPinExpired           = 0x000000A3 // CKR_PIN_EXPIRED
	CKRPinLocked            = 0x000000A4 // CKR_PIN_LOCKED
	CKRSessionClosed        = 0x000000B0 // CKR_SESSION_CLOSED
	CKRSessionHandleInvalid = 0x000000B3 // CKR_SESSION_HANDLE_INVALID
	CKRTokenNotPresent      = 0x000000E0 // CKR_TOKEN_NOT_PRESENT
	CKRUserNotLoggedIn      = 0x00000101 // CKR_USER_NOT_LOGGED_IN
)

var ckrNames = map[uint]string{
	CKRDeviceRemoved: "CKR_DEVICE_REMOVED", CKRKeyHandleInvalid: "CKR_KEY_HANDLE_INVALID",
	CKRPinIncorrect: "CKR_PIN_INCORRECT", CKRPinExpired: "CKR_PIN_EXPIRED", CKRPinLocked: "CKR_PIN_LOCKED",
	CKRSessionClosed: "CKR_SESSION_CLOSED", CKRSessionHandleInvalid: "CKR_SESSION_HANDLE_INVALID",
	CKRTokenNotPresent: "CKR_TOKEN_NOT_PRESENT", CKRUserNotLoggedIn: "CKR_USER_NOT_LOGGED_IN",
}

// Error names the code.
func (e CKRError) Error() string {
	if n, ok := ckrNames[e.Code]; ok {
		return fmt.Sprintf("pkcs11: %s (0x%X)", n, e.Code)
	}
	return fmt.Sprintf("pkcs11: CKR 0x%X", e.Code)
}

// ckrCodeOf finds the PKCS#11 return code in an error chain. The cgo backend sets it; without cgo
// there is no PKCS#11 library to return one.
var ckrCodeOf = func(error) (uint, bool) { return 0, false }

// PINSource returns the user PIN of the token. It is called each time a session opens, so the PIN
// is not held in memory between uses and a rotated PIN is picked up on reconnect.
type PINSource func() (string, error)

// PINFromEnv reads the PIN from the environment variable name. An unset or empty variable is an
// error that names the variable, never the value.
func PINFromEnv(name string) PINSource {
	return func() (string, error) {
		v := os.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("pkcs11: the PIN environment variable %s is empty or unset: %w", name, agent.ErrConfig)
		}
		return v, nil
	}
}

// PINFromFile reads the PIN from the file at path (a mounted secret, say), dropping one trailing
// line break. An unreadable or empty file is an error that names the path, never the content.
func PINFromFile(path string) PINSource {
	return func() (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("pkcs11: read the PIN file %s: %w", path, agent.ErrConfig)
		}
		v := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		if v == "" {
			return "", fmt.Errorf("pkcs11: the PIN file %s is empty: %w", path, agent.ErrConfig)
		}
		return v, nil
	}
}

// Config selects the token and the key.
type Config struct {
	// Module is the path of the vendor's PKCS#11 library (libsofthsm2.so, a Luna, nShield,
	// CloudHSM or YubiHSM client library). Required.
	Module string
	// TokenLabel selects the token by its label. Exactly one of TokenLabel and SlotID is required;
	// the label is the stable choice, since a slot number can change when tokens are added.
	TokenLabel string
	// SlotID selects the token by slot number when TokenLabel is empty.
	SlotID *uint
	// Scheme is the signature scheme of the key: audit.AlgEd25519 (the default; a CKK_EC_EDWARDS
	// key and CKM_EDDSA) or audit.AlgECDSAP256 (a CKK_EC key on prime256v1 and CKM_ECDSA over the
	// SHA-256 of the message). A key of the other scheme is refused, never used under the wrong one.
	Scheme audit.Alg
	// KeyLabel and KeyID select the private key (CKA_LABEL, CKA_ID). At least one is required;
	// when both are set, both must match.
	KeyLabel string
	// KeyID is the key's CKA_ID bytes.
	KeyID []byte
	// PIN supplies the user PIN. Nil skips the login, for a token that is already logged in or
	// authenticates through a protected path (a PIN pad); most tokens refuse to sign then.
	PIN PINSource
	// PublicKey is the key's 32-byte Ed25519 public key, for a token that does not expose it. When
	// the token does expose it, the two must be equal. Optional.
	PublicKey []byte
	// RequireNonExtractable makes Open fail with ErrExtractable unless the token reports the key as
	// CKA_EXTRACTABLE=false and CKA_SENSITIVE=true, so the key cannot be read out of the token.
	// It checks what the token says; it is not an attestation.
	RequireNonExtractable bool
}

// session is one logged-in connection to the token, bound to the selected key.
type session interface {
	// sign returns the token's signature over msg in the encoding the scheme's audit verifier
	// reads (for ecdsa-p256, the canonical low-S r||s).
	sign(msg []byte) ([]byte, error)
	// close ends the session and releases the module.
	close() error
}

// opener opens a session for cfg and returns it with the key's public key. It is a variable of
// the Signer so that a test can supply a token that fails in chosen ways.
type opener func(cfg Config, pin string) (session, []byte, error)

// Signer signs with a key held by a PKCS#11 token (Ed25519 or ECDSA P-256, see Config.Scheme). It satisfies audit.Signer and is safe
// for concurrent use; signing calls are serialised, since a PKCS#11 session runs one signing
// operation at a time.
type Signer struct {
	cfg  Config
	open opener
	pub  []byte
	v    audit.Verifier // checks every signature against the public key

	mu     sync.Mutex
	sess   session
	closed bool
}

var _ audit.Signer = (*Signer)(nil)

// Open connects to the token, selects the key, and returns a Signer for it. It fails, naming the
// cause, when the module cannot be loaded, the token or key is not found or is ambiguous, the
// token lacks Ed25519 (ErrNoEd25519), or the PIN is refused.
func Open(cfg Config) (*Signer, error) { return openWith(cfg, openSession) }

func openWith(cfg Config, open opener) (*Signer, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	cfg.Scheme = schemeOrDefault(cfg.Scheme)
	cfg.KeyID = bytes.Clone(cfg.KeyID)
	cfg.PublicKey = bytes.Clone(cfg.PublicKey)
	s := &Signer{cfg: cfg, open: open}
	sess, pub, v, err := s.connect()
	if err != nil {
		return nil, err
	}
	s.sess, s.pub, s.v = sess, pub, v
	return s, nil
}

func (c Config) check() error {
	switch {
	case c.Module == "":
		return fmt.Errorf("pkcs11: Config.Module is required: %w", agent.ErrConfig)
	case (c.TokenLabel == "") == (c.SlotID == nil):
		return fmt.Errorf("pkcs11: set exactly one of Config.TokenLabel and Config.SlotID: %w", agent.ErrConfig)
	case c.KeyLabel == "" && len(c.KeyID) == 0:
		return fmt.Errorf("pkcs11: set Config.KeyLabel or Config.KeyID: %w", agent.ErrConfig)
	case c.Scheme != "" && c.Scheme != audit.AlgEd25519 && c.Scheme != audit.AlgECDSAP256:
		return fmt.Errorf("pkcs11: Config.Scheme %q is not ed25519 or ecdsa-p256: %w", c.Scheme, agent.ErrConfig)
	case c.PublicKey != nil && len(c.PublicKey) != pubSize(c.Scheme):
		return fmt.Errorf("pkcs11: Config.PublicKey is %d bytes, want %d for %s: %w", len(c.PublicKey), pubSize(c.Scheme), schemeOrDefault(c.Scheme), agent.ErrConfig)
	}
	return nil
}

func schemeOrDefault(a audit.Alg) audit.Alg {
	if a == "" {
		return audit.AlgEd25519
	}
	return a
}

// pubSize is the length of a public key of the scheme in the encoding audit.NewVerifier reads.
func pubSize(a audit.Alg) int {
	if a == audit.AlgECDSAP256 {
		return 65
	}
	return ed25519PointSize
}

// connect opens a session, reading the PIN just for the login.
func (s *Signer) connect() (session, []byte, audit.Verifier, error) {
	pin := ""
	if s.cfg.PIN != nil {
		p, err := s.cfg.PIN()
		if err != nil {
			return nil, nil, nil, err
		}
		pin = p
	}
	sess, pub, err := s.open(s.cfg, pin)
	if err != nil {
		return nil, nil, nil, scrub(err, pin)
	}
	ok := false
	defer func() {
		if !ok {
			sess.close()
		}
	}()
	if len(s.cfg.PublicKey) != 0 {
		if len(pub) == 0 {
			pub = s.cfg.PublicKey
		} else if !bytes.Equal(pub, s.cfg.PublicKey) {
			return nil, nil, nil, fmt.Errorf("pkcs11: the token's public key differs from Config.PublicKey: %w", agent.ErrConfig)
		}
	}
	if len(pub) == 0 {
		return nil, nil, nil, fmt.Errorf("pkcs11: the token does not expose the public key; set Config.PublicKey: %w", agent.ErrConfig)
	}
	v, err := audit.NewVerifier(s.cfg.Scheme, pub)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pkcs11: the token's public key is not a usable %s key: %w", s.cfg.Scheme, err)
	}
	// One signature proves the private key is the key pair of the public key, at Open and at every
	// reopen, so a mismatch is found here and not by the first anchor.
	sig, err := sess.sign([]byte(selfTest))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pkcs11: self-test signature: %w", err)
	}
	if !v.Verify([]byte(selfTest), sig) {
		return nil, nil, nil, fmt.Errorf("pkcs11: self-test: the private key is not the key pair of the public key the token exposes")
	}
	ok = true
	return sess, bytes.Clone(pub), v, nil
}

// scrub returns a copy of err that cannot leak the PIN: its message has the PIN removed in the
// forms a vendor library may print it (as is, quoted with %q, JSON-escaped), and its chain holds
// only this package's sentinels, so a logger that walks errors.Unwrap, or prints the error with
// %#v, finds nothing but the scrubbed message. errors.Is still answers for the sentinels.
func scrub(err error, pin string) error {
	msg := err.Error()
	if pin != "" {
		for _, form := range pinForms(pin) {
			msg = strings.ReplaceAll(msg, form, "[redacted]")
		}
	}
	e := scrubbed{msg: msg}
	if code, ok := ckrCodeOf(err); ok {
		e.is = append(e.is, CKRError{Code: code})
	}
	for _, s := range []error{ErrNoEd25519, ErrNoP256, ErrKeyNotFound, ErrAmbiguous, ErrNoCgo, ErrExtractable, errSessionLost, agent.ErrConfig, audit.ErrMalformed} {
		if errors.Is(err, s) {
			e.is = append(e.is, s)
		}
	}
	return e
}

// pinForms are the spellings of the PIN to remove, longest first.
func pinForms(pin string) []string {
	q := strconv.Quote(pin)
	j, _ := json.Marshal(pin)
	forms := []string{q, string(j), strconv.QuoteToASCII(pin), q[1 : len(q)-1], string(j[1 : len(j)-1]), pin}
	slices.SortStableFunc(forms, func(a, b string) int { return len(b) - len(a) })
	return slices.Compact(forms)
}

// scrubbed is an error whose message has had the PIN removed.
type scrubbed struct {
	msg string
	is  []error // the sentinels the original error matched
}

func (e scrubbed) Error() string   { return e.msg }
func (e scrubbed) Unwrap() []error { return e.is }

// Alg reports the signature scheme (Config.Scheme, audit.AlgEd25519 by default).
func (s *Signer) Alg() audit.Alg { return s.cfg.Scheme }

// PublicKey returns the public key of the token's key (32 bytes for Ed25519, the 65-byte uncompressed point for P-256), in the encoding
// audit.NewVerifier reads. It is read once, when the Signer opens.
func (s *Signer) PublicKey() []byte { return bytes.Clone(s.pub) }

// Sign returns the token's signature over message. It checks the signature against the
// public key before returning it, so a faulty token or a wrong key is an error here and not a
// signature that fails to verify later. If the session was lost (the token was reset or removed
// and returned) it opens a new one and signs again, once.
func (s *Signer) Sign(message []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("pkcs11: the signer is closed: %w", agent.ErrConfig)
	}
	var err error
	for try := 0; try <= maxSessionReopenTries; try++ {
		if s.sess == nil {
			if err = s.reopen(); err != nil {
				return nil, err
			}
		}
		var sig []byte
		sig, err = s.sess.sign(message)
		if err == nil {
			if !s.v.Verify(message, sig) {
				return nil, fmt.Errorf("pkcs11: the token's signature does not verify under its own public key")
			}
			return sig, nil
		}
		if !errors.Is(err, errSessionLost) {
			return nil, fmt.Errorf("pkcs11: sign: %w", scrub(err, ""))
		}
		s.sess.close()
		s.sess = nil
	}
	return nil, fmt.Errorf("pkcs11: sign: %w", scrub(err, ""))
}

// reopen opens a new session and requires it to hold the same key as before.
func (s *Signer) reopen() error {
	sess, pub, v, err := s.connect()
	if err != nil {
		return fmt.Errorf("pkcs11: reopen the session: %w", err)
	}
	if !bytes.Equal(pub, s.pub) {
		sess.close()
		return fmt.Errorf("pkcs11: reopen the session: the key now selected has a different public key: %w", agent.ErrConfig)
	}
	s.sess, s.v = sess, v
	return nil
}

// Close ends the session and releases the module. A closed Signer refuses to sign.
func (s *Signer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.sess == nil {
		return nil
	}
	err := s.sess.close()
	s.sess = nil
	return err
}

// String describes the signer by its key identity (audit.KeyID of the public key), which is not
// secret.
func (s *Signer) String() string {
	return fmt.Sprintf("pkcs11.Signer{%s}", audit.KeyID(string(s.cfg.Scheme), s.pub))
}

// Format writes String for every verb, so no fmt verb prints the configuration.
func (s *Signer) Format(f fmt.State, _ rune) { io.WriteString(f, s.String()) }
