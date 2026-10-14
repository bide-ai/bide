//go:build cgo

package pkcs11

import (
	"errors"
	"testing"

	p11 "github.com/miekg/pkcs11"
)

// fakeObjects is a token with one private key whose attributes are chosen by the test.
type fakeObjects struct {
	attrs map[uint][]byte
}

func (f fakeObjects) FindObjectsInit(p11.SessionHandle, []*p11.Attribute) error { return nil }
func (f fakeObjects) FindObjects(p11.SessionHandle, int) ([]p11.ObjectHandle, bool, error) {
	return []p11.ObjectHandle{1}, false, nil
}
func (f fakeObjects) FindObjectsFinal(p11.SessionHandle) error { return nil }
func (f fakeObjects) GetAttributeValue(_ p11.SessionHandle, _ p11.ObjectHandle, want []*p11.Attribute) ([]*p11.Attribute, error) {
	out := make([]*p11.Attribute, len(want))
	for i, a := range want {
		out[i] = &p11.Attribute{Type: a.Type, Value: f.attrs[a.Type]}
	}
	return out, nil
}

// CKK_EC_EDWARDS covers Ed448 as well as Ed25519; a private key that reports Ed448 curve
// parameters (OID 1.3.101.113) is refused with ErrNoEd25519,.
func TestFindPrivateKeyChecksCurveParams(t *testing.T) {
	ed448 := []byte{0x06, 0x03, 0x2b, 0x65, 0x71}
	base := map[uint][]byte{p11.CKA_SIGN: {1}, p11.CKA_ID: {1}}
	with := func(params []byte) fakeObjects {
		m := map[uint][]byte{}
		for k, v := range base {
			m[k] = v
		}
		if params != nil {
			m[p11.CKA_EC_PARAMS] = params
		}
		return fakeObjects{m}
	}
	cfg := Config{KeyLabel: "k"}

	if _, _, err := findPrivateKey(with(ed448), 0, cfg); !errors.Is(err, ErrNoEd25519) {
		t.Fatalf("Ed448 parameters: err = %v, want ErrNoEd25519", err)
	}
	if _, _, err := findPrivateKey(with(ed25519OID), 0, cfg); err != nil {
		t.Fatalf("Ed25519 parameters: %v", err)
	}
	if _, _, err := findPrivateKey(with(nil), 0, cfg); err != nil {
		t.Fatalf("a token that reports no parameters is not refused: %v", err)
	}
}
