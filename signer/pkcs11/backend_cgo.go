//go:build cgo

package pkcs11

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"

	p11 "github.com/miekg/pkcs11"

	"github.com/bide-ai/bide/agent"
)

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
	mod  *module
	sh   p11.SessionHandle
	key  p11.ObjectHandle
	done bool
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
	if err := requireEdDSA(ctx, slot); err != nil {
		return nil, nil, err
	}
	sh, err := ctx.OpenSession(slot, p11.CKF_SERIAL_SESSION)
	if err != nil {
		return nil, nil, fmt.Errorf("pkcs11: open a session: %w", err)
	}
	ts := &tokenSession{mod: mod, sh: sh}
	fail := func(err error) (*tokenSession, []byte, error) {
		ctx.CloseSession(sh)
		return nil, nil, err
	}
	if cfg.PIN != nil {
		if err := ctx.Login(sh, p11.CKU_USER, pin); err != nil && !isErr(err, p11.CKR_USER_ALREADY_LOGGED_IN) {
			return fail(fmt.Errorf("pkcs11: log in: %w", err))
		}
	}
	key, id, err := findPrivateKey(ctx, sh, cfg)
	if err != nil {
		return fail(err)
	}
	ts.key = key
	pub, err := readPublicKey(ctx, sh, cfg, key, id)
	if err != nil {
		return fail(err)
	}
	return ts, pub, nil
}

func (t *tokenSession) sign(msg []byte) ([]byte, error) {
	ctx := t.mod.ctx
	if err := ctx.SignInit(t.sh, []*p11.Mechanism{p11.NewMechanism(ckmEdDSA, nil)}, t.key); err != nil {
		return nil, classify(err)
	}
	sig, err := ctx.Sign(t.sh, msg)
	if err != nil {
		return nil, classify(err)
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

// requireEdDSA fails with ErrNoEd25519 unless the token lists CKM_EDDSA.
func requireEdDSA(ctx *p11.Ctx, slot uint) error {
	ms, err := ctx.GetMechanismList(slot)
	if err != nil {
		return fmt.Errorf("pkcs11: list the token's mechanisms: %w", err)
	}
	for _, m := range ms {
		if m.Mechanism == ckmEdDSA {
			return nil
		}
	}
	return fmt.Errorf("pkcs11: the token does not list CKM_EDDSA (Ed25519), which bide's audit log needs: %w", ErrNoEd25519)
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
	edwards := uint(ckkECEdwards)
	objs, err := find(ctx, sh, keyTemplate(cfg, p11.CKO_PRIVATE_KEY, &edwards))
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
		if pa, err := ctx.GetAttributeValue(sh, objs[0], []*p11.Attribute{p11.NewAttribute(p11.CKA_EC_PARAMS, nil)}); err == nil && len(pa) == 1 && len(pa[0].Value) != 0 && !isEd25519Params(pa[0].Value) {
			return 0, nil, fmt.Errorf("pkcs11: the key's CKA_EC_PARAMS do not name Ed25519 (an Ed448 key?): %w", ErrNoEd25519)
		}
		if cfg.RequireNonExtractable {
			ea, err := ctx.GetAttributeValue(sh, objs[0], []*p11.Attribute{p11.NewAttribute(p11.CKA_EXTRACTABLE, nil), p11.NewAttribute(p11.CKA_SENSITIVE, nil)})
			if err != nil || len(ea) != 2 || len(ea[0].Value) != 1 || len(ea[1].Value) != 1 || ea[0].Value[0] != 0 || ea[1].Value[0] == 0 {
				return 0, nil, fmt.Errorf("pkcs11: Config.RequireNonExtractable is set, and the token does not report CKA_EXTRACTABLE=false and CKA_SENSITIVE=true: %w", ErrExtractable)
			}
		}
		return objs[0], attrs[1].Value, nil
	case 0:
		// A key of another type under the same label tells the operator what is wrong.
		other, _ := find(ctx, sh, keyTemplate(cfg, p11.CKO_PRIVATE_KEY, nil))
		if len(other) > 0 {
			return 0, nil, fmt.Errorf("pkcs11: the selected key is not an Ed25519 key (not CKK_EC_EDWARDS); bide's audit log signs with Ed25519: %w", ErrNoEd25519)
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
		if len(attrs[1].Value) != 0 && !isEd25519Params(attrs[1].Value) {
			return nil, fmt.Errorf("pkcs11: the key's CKA_EC_PARAMS do not name Ed25519: %w", ErrNoEd25519)
		}
		return parseECPoint(attrs[0].Value)
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
