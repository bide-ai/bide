//go:build cgo

package pkcs11

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"

	p11 "github.com/miekg/pkcs11"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

const (
	ckmECDSA = 0x1041 // CKM_ECDSA: raw ECDSA over a digest the caller computed
	ckkEC    = 0x03   // CKK_EC
)

func isP256(cfg Config) bool { return cfg.Scheme == audit.AlgECDSAP256 }

// modules are the PKCS#11 libraries this process has initialised. A module is initialised once
// per process (a second C_Initialize fails) and finalised when its last user closes, so several
// Signers over one library share it.
var modules = struct {
	sync.Mutex
	m map[string]*module
}{m: map[string]*module{}}

type module struct {
	path string
	ctx  *p11.Ctx
	refs int
}

func acquire(path string) (*module, error) {
	modules.Lock()
	defer modules.Unlock()
	if m := modules.m[path]; m != nil {
		m.refs++
		return m, nil
	}
	ctx := p11.New(path)
	if ctx == nil {
		return nil, fmt.Errorf("pkcs11: cannot load the module %s: %w", path, agent.ErrConfig)
	}
	if err := ctx.Initialize(); err != nil {
		ctx.Destroy()
		return nil, fmt.Errorf("pkcs11: initialise %s: %w", path, err)
	}
	m := &module{path: path, ctx: ctx, refs: 1}
	modules.m[path] = m
	return m, nil
}

func (m *module) release() {
	modules.Lock()
	defer modules.Unlock()
	if m.refs--; m.refs > 0 {
		return
	}
	delete(modules.m, m.path)
	m.ctx.Finalize()
	m.ctx.Destroy()
}

// tokenSession is a logged-in session on the token with the signing key found.
type tokenSession struct {
	p256 bool
	mod  *module
	sh   p11.SessionHandle
	key  p11.ObjectHandle
	done bool
	// alwaysAuth is the key's CKA_ALWAYS_AUTHENTICATE: each signature needs a context-specific login.
	alwaysAuth bool
	// poisoned is set when authenticating a signature failed: the token keeps the signature
	// operation active (CKR_OPERATION_ACTIVE for the next one), and PKCS#11 has no way to cancel it
	// but closing the session, so the next signature reports the session lost and the signer opens
	// a new one.
	poisoned bool
}

// openSession loads the module, finds the token and the key, and logs in.
func openSession(cfg Config, pin string) (session, []byte, error) {
	mod, err := acquire(cfg.Module)
	if err != nil {
		return nil, nil, err
	}
	ts, pub, err := openOn(mod, cfg, pin)
	if err != nil {
		mod.release()
		return nil, nil, err
	}
	return ts, pub, nil
}

func openOn(mod *module, cfg Config, pin string) (*tokenSession, []byte, error) {
	ctx := mod.ctx
	slot, err := findSlot(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := requireMechanism(ctx, slot, cfg); err != nil {
		return nil, nil, err
	}
	var tokenFlags uint
	if ti, err := ctx.GetTokenInfo(slot); err == nil {
		tokenFlags = ti.Flags
	}
	sh, err := ctx.OpenSession(slot, p11.CKF_SERIAL_SESSION)
	if err != nil {
		return nil, nil, fmt.Errorf("pkcs11: open a session: %w", err)
	}
	ts := &tokenSession{mod: mod, sh: sh, p256: isP256(cfg)}
	fail := func(err error) (*tokenSession, []byte, error) {
		ctx.CloseSession(sh)
		return nil, nil, err
	}
	if err := login(ctx, sh, tokenFlags, cfg, pin); err != nil {
		return fail(err)
	}
	key, id, err := findPrivateKey(ctx, sh, cfg)
	if err != nil {
		return fail(err)
	}
	ts.key = key
	// A key that demands authentication for each signature says so; a token that does not report the
	// attribute is taken to be a key that does not.
	if aa, err := ctx.GetAttributeValue(sh, key, []*p11.Attribute{p11.NewAttribute(p11.CKA_ALWAYS_AUTHENTICATE, nil)}); err == nil && len(aa) == 1 && len(aa[0].Value) == 1 && aa[0].Value[0] != 0 {
		ts.alwaysAuth = true
	}
	pub, err := readPublicKey(ctx, sh, cfg, key, id)
	if err != nil {
		return fail(err)
	}
	return ts, pub, nil
}

// demandsAuth reports that the key needs a context-specific login for each signature.
func (t *tokenSession) demandsAuth() bool { return t.alwaysAuth }

func (t *tokenSession) sign(msg []byte, auth func() (string, error)) ([]byte, error) {
	ctx := t.mod.ctx
	if t.poisoned {
		return nil, fmt.Errorf("%w: a signature was left unauthenticated", errSessionLost)
	}
	mech, in := uint(ckmEdDSA), msg
	if t.p256 {
		// CKM_ECDSA signs a digest the caller computed; bide's P-256 scheme signs SHA-256(message).
		h := sha256.Sum256(msg)
		mech, in = ckmECDSA, h[:]
	}
	if err := ctx.SignInit(t.sh, []*p11.Mechanism{p11.NewMechanism(mech, nil)}, t.key); err != nil {
		return nil, classify(err)
	}
	if t.alwaysAuth {
		pin, err := auth()
		if err != nil {
			t.poisoned = true // the signature was initialised and will not be finished
			return nil, err
		}
		if err := contextLogin(ctx, t.sh, pin); err != nil {
			t.poisoned = true
			return nil, err
		}
	}
	sig, err := ctx.Sign(t.sh, in)
	if err != nil {
		return nil, classify(err)
	}
	if t.p256 {
		// The token returns raw r||s; audit verifies the canonical low-S form.
		return audit.ECDSAP256SignatureFromRaw(sig)
	}
	return sig, nil
}

func (t *tokenSession) close() error {
	if t.done {
		return nil
	}
	t.done = true
	err := t.mod.ctx.CloseSession(t.sh)
	t.mod.release()
	if err != nil && !isErr(err, p11.CKR_SESSION_HANDLE_INVALID) && !isErr(err, p11.CKR_SESSION_CLOSED) {
		return err
	}
	return nil
}

func init() {
	ckrCodeOf = func(err error) (uint, bool) {
		var e p11.Error
		if errors.As(err, &e) {
			return uint(e), true
		}
		return 0, false
	}
}

func isErr(err error, code uint) bool {
	var e p11.Error
	return errors.As(err, &e) && uint(e) == code
}

// classify marks the failures that a fresh session can cure.
func classify(err error) error {
	for _, c := range []uint{
		p11.CKR_SESSION_HANDLE_INVALID, p11.CKR_SESSION_CLOSED, p11.CKR_DEVICE_REMOVED,
		p11.CKR_TOKEN_NOT_PRESENT, p11.CKR_USER_NOT_LOGGED_IN,
	} {
		if isErr(err, c) {
			return fmt.Errorf("%w: %w", errSessionLost, err)
		}
	}
	return err
}

func findSlot(ctx *p11.Ctx, cfg Config) (uint, error) {
	slots, err := ctx.GetSlotList(true)
	if err != nil {
		return 0, fmt.Errorf("pkcs11: list the slots: %w", err)
	}
	var match []uint
	for _, s := range slots {
		if cfg.SlotID != nil {
			if s == *cfg.SlotID {
				match = append(match, s)
			}
			continue
		}
		ti, err := ctx.GetTokenInfo(s)
		if err != nil {
			continue
		}
		if strings.TrimRight(ti.Label, " \x00") == cfg.TokenLabel {
			match = append(match, s)
		}
	}
	want := fmt.Sprintf("token %q", cfg.TokenLabel)
	if cfg.SlotID != nil {
		want = fmt.Sprintf("slot %d", *cfg.SlotID)
	}
	switch len(match) {
	case 0:
		return 0, fmt.Errorf("pkcs11: %s not found among the %d present tokens: %w", want, len(slots), agent.ErrConfig)
	case 1:
		return match[0], nil
	}
	return 0, fmt.Errorf("pkcs11: %s matches %d tokens: %w", want, len(match), ErrAmbiguous)
}

// requireMechanism fails with ErrNoEd25519 (ErrNoP256 for the P-256 scheme) unless the token lists
// the scheme's signing mechanism.
func requireMechanism(ctx *p11.Ctx, slot uint, cfg Config) error {
	want, name, sentinel := uint(ckmEdDSA), "CKM_EDDSA (Ed25519)", ErrNoEd25519
	if isP256(cfg) {
		want, name, sentinel = ckmECDSA, "CKM_ECDSA (ECDSA P-256)", ErrNoP256
	}
	ms, err := ctx.GetMechanismList(slot)
	if err != nil {
		return fmt.Errorf("pkcs11: list the token's mechanisms: %w", err)
	}
	for _, m := range ms {
		if m.Mechanism == want {
			return nil
		}
	}
	return fmt.Errorf("pkcs11: the token does not list %s, which the %s scheme needs: %w", name, cfg.Scheme, sentinel)
}

func keyTemplate(cfg Config, class uint, keyType *uint) []*p11.Attribute {
	t := []*p11.Attribute{p11.NewAttribute(p11.CKA_CLASS, class)}
	if keyType != nil {
		t = append(t, p11.NewAttribute(p11.CKA_KEY_TYPE, *keyType))
	}
	if cfg.KeyLabel != "" {
		t = append(t, p11.NewAttribute(p11.CKA_LABEL, cfg.KeyLabel))
	}
	if len(cfg.KeyID) != 0 {
		t = append(t, p11.NewAttribute(p11.CKA_ID, cfg.KeyID))
	}
	return t
}

// objectAPI is the part of the PKCS#11 context the key lookup uses; *p11.Ctx satisfies it, and a
// test supplies a token that reports chosen attributes.
type objectAPI interface {
	FindObjectsInit(sh p11.SessionHandle, temp []*p11.Attribute) error
	FindObjects(sh p11.SessionHandle, max int) ([]p11.ObjectHandle, bool, error)
	FindObjectsFinal(sh p11.SessionHandle) error
	GetAttributeValue(sh p11.SessionHandle, o p11.ObjectHandle, a []*p11.Attribute) ([]*p11.Attribute, error)
}

func find(ctx objectAPI, sh p11.SessionHandle, tmpl []*p11.Attribute) ([]p11.ObjectHandle, error) {
	if err := ctx.FindObjectsInit(sh, tmpl); err != nil {
		return nil, fmt.Errorf("pkcs11: find objects: %w", err)
	}
	objs, _, err := ctx.FindObjects(sh, 3)
	ctx.FindObjectsFinal(sh)
	if err != nil {
		return nil, fmt.Errorf("pkcs11: find objects: %w", err)
	}
	return objs, nil
}

// findPrivateKey returns the one Ed25519 private key the Config selects, and its CKA_ID.
func findPrivateKey(ctx objectAPI, sh p11.SessionHandle, cfg Config) (p11.ObjectHandle, []byte, error) {
	keyType, sentinel, scheme := uint(ckkECEdwards), ErrNoEd25519, "Ed25519 (CKK_EC_EDWARDS)"
	if isP256(cfg) {
		keyType, sentinel, scheme = ckkEC, ErrNoP256, "ECDSA P-256 (CKK_EC on prime256v1)"
	}
	objs, err := find(ctx, sh, keyTemplate(cfg, p11.CKO_PRIVATE_KEY, &keyType))
	if err != nil {
		return 0, nil, err
	}
	switch len(objs) {
	case 1:
		attrs, err := ctx.GetAttributeValue(sh, objs[0], []*p11.Attribute{p11.NewAttribute(p11.CKA_SIGN, nil), p11.NewAttribute(p11.CKA_ID, nil)})
		if err != nil {
			return 0, nil, fmt.Errorf("pkcs11: read the key's attributes: %w", err)
		}
		if len(attrs[0].Value) != 1 || attrs[0].Value[0] == 0 {
			return 0, nil, fmt.Errorf("pkcs11: the key is not usable for signing (CKA_SIGN is false): %w", agent.ErrConfig)
		}
		// CKK_EC_EDWARDS covers Ed448 too; the curve parameters tell them apart when the token
		// reports them for the private key.
		if pa, err := ctx.GetAttributeValue(sh, objs[0], []*p11.Attribute{p11.NewAttribute(p11.CKA_EC_PARAMS, nil)}); err == nil && len(pa) == 1 && len(pa[0].Value) != 0 && !paramsMatch(cfg, pa[0].Value) {
			return 0, nil, fmt.Errorf("pkcs11: the key's CKA_EC_PARAMS do not name the %s curve (for Ed25519, an Ed448 key?): %w", cfg.Scheme, sentinel)
		}
		if cfg.RequireNonExtractable {
			ea, err := ctx.GetAttributeValue(sh, objs[0], []*p11.Attribute{p11.NewAttribute(p11.CKA_EXTRACTABLE, nil), p11.NewAttribute(p11.CKA_SENSITIVE, nil)})
			if err != nil || len(ea) != 2 || len(ea[0].Value) != 1 || len(ea[1].Value) != 1 || ea[0].Value[0] != 0 || ea[1].Value[0] == 0 {
				return 0, nil, fmt.Errorf("pkcs11: Config.RequireNonExtractable is set, and the token does not report CKA_EXTRACTABLE=false and CKA_SENSITIVE=true: %w", ErrExtractable)
			}
			// A key that was extractable or not sensitive at some time may have been copied out
			// before it was marked otherwise: when the token reports the history (CKA_NEVER_EXTRACTABLE
			// and CKA_ALWAYS_SENSITIVE, which a token keeps itself and no caller can set), it must say
			// the key never was. A token that does not report them is taken at its word on the two
			// attributes above.
			if ha, err := ctx.GetAttributeValue(sh, objs[0], []*p11.Attribute{p11.NewAttribute(p11.CKA_NEVER_EXTRACTABLE, nil), p11.NewAttribute(p11.CKA_ALWAYS_SENSITIVE, nil)}); err == nil {
				for _, a := range ha {
					if len(a.Value) == 1 && a.Value[0] == 0 {
						return 0, nil, fmt.Errorf("pkcs11: Config.RequireNonExtractable is set, and the token reports the key was at some time extractable or not sensitive (CKA_NEVER_EXTRACTABLE or CKA_ALWAYS_SENSITIVE is false): %w", ErrExtractable)
					}
				}
			}
		}
		return objs[0], attrs[1].Value, nil
	case 0:
		// A key of another type under the same label tells the operator what is wrong.
		other, _ := find(ctx, sh, keyTemplate(cfg, p11.CKO_PRIVATE_KEY, nil))
		if len(other) > 0 {
			return 0, nil, fmt.Errorf("pkcs11: the selected key is not a %s key, and Config.Scheme is %s: %w", scheme, cfg.Scheme, sentinel)
		}
		return 0, nil, fmt.Errorf("pkcs11: no private key with label %q and the given ID: %w", cfg.KeyLabel, ErrKeyNotFound)
	}
	return 0, nil, fmt.Errorf("pkcs11: %d private keys match label %q and the given ID: %w", len(objs), cfg.KeyLabel, ErrAmbiguous)
}

// readPublicKey reads the key's public key: from the private key object if the token exposes it
// there, else from the public key object that shares the ID. It returns nil, nil when the token
// exposes it nowhere (Config.PublicKey then supplies it).
func readPublicKey(ctx objectAPI, sh p11.SessionHandle, cfg Config, priv p11.ObjectHandle, id []byte) ([]byte, error) {
	read := func(o p11.ObjectHandle) ([]byte, error) {
		attrs, err := ctx.GetAttributeValue(sh, o, []*p11.Attribute{p11.NewAttribute(p11.CKA_EC_POINT, nil), p11.NewAttribute(p11.CKA_EC_PARAMS, nil)})
		if err != nil || len(attrs) != 2 || len(attrs[0].Value) == 0 {
			return nil, nil
		}
		if len(attrs[1].Value) != 0 && !paramsMatch(cfg, attrs[1].Value) {
			return nil, fmt.Errorf("pkcs11: the key's CKA_EC_PARAMS do not name the %s curve: %w", cfg.Scheme, schemeSentinel(cfg))
		}
		return parsePoint(cfg.Scheme, attrs[0].Value)
	}
	if pub, err := read(priv); pub != nil || err != nil {
		return pub, err
	}
	tmpl := keyTemplate(Config{KeyLabel: cfg.KeyLabel}, p11.CKO_PUBLIC_KEY, nil)
	if len(id) != 0 {
		tmpl = append(tmpl, p11.NewAttribute(p11.CKA_ID, bytes.Clone(id)))
	}
	objs, err := find(ctx, sh, tmpl)
	if err != nil {
		return nil, err
	}
	if len(objs) != 1 {
		return nil, nil
	}
	return read(objs[0])
}

func paramsMatch(cfg Config, p []byte) bool {
	if isP256(cfg) {
		return isP256Params(p)
	}
	return isEd25519Params(p)
}

func schemeSentinel(cfg Config) error {
	if isP256(cfg) {
		return ErrNoP256
	}
	return ErrNoEd25519
}

// loginAPI is the part of the module login uses.
type loginAPI interface {
	Login(sh p11.SessionHandle, userType uint, pin string) error
}

// login logs the user in as cfg says. A token with a protected authentication path (a PIN pad)
// collects the PIN itself: C_Login is called with a NULL PIN (the binding sends one for an empty
// PIN), and only when Config.ProtectedAuthPath says that is wanted and the token has the flag. A
// PIN given to such a token, or a protected path asked of a token without one, is a configuration
// error, not a login to attempt.
func login(ctx loginAPI, sh p11.SessionHandle, tokenFlags uint, cfg Config, pin string) error {
	protected := tokenFlags&p11.CKF_PROTECTED_AUTHENTICATION_PATH != 0
	switch {
	case cfg.ProtectedAuthPath && !protected:
		return fmt.Errorf("pkcs11: Config.ProtectedAuthPath is set and the token does not report CKF_PROTECTED_AUTHENTICATION_PATH (it has no PIN pad; give the PIN instead): %w", errors.Join(ErrNoProtectedAuthPath, agent.ErrConfig))
	case cfg.ProtectedAuthPath:
		pin = ""
	case cfg.PIN != nil && protected:
		return fmt.Errorf("pkcs11: the token collects the PIN itself (CKF_PROTECTED_AUTHENTICATION_PATH), and Config.PIN is set: use Config.ProtectedAuthPath: %w", agent.ErrConfig)
	case cfg.PIN == nil:
		return nil
	}
	if err := ctx.Login(sh, p11.CKU_USER, pin); err != nil && !isErr(err, p11.CKR_USER_ALREADY_LOGGED_IN) {
		return fmt.Errorf("pkcs11: log in: %w", err)
	}
	return nil
}

// contextLogin logs in as the context-specific user, between C_SignInit and C_Sign, for a key with
// CKA_ALWAYS_AUTHENTICATE. An empty PIN is sent as NULL (the binding does), which a protected
// authentication path expects.
func contextLogin(ctx loginAPI, sh p11.SessionHandle, pin string) error {
	if err := ctx.Login(sh, p11.CKU_CONTEXT_SPECIFIC, pin); err != nil {
		return fmt.Errorf("pkcs11: authenticate the signature: %w", classify(err))
	}
	return nil
}
