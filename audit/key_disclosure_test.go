package audit

import (
	"bytes"
	"crypto/ed25519"
	"crypto/mldsa"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A type that holds a private key must not hand the key to an encoder or a formatter: a signer
// kept in a config struct that is logged, dumped with %+v, or marshaled to JSON would otherwise
// publish the key that anchors the audit trail.
func TestSignersDoNotDiscloseKeys(t *testing.T) {
	_, edPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	mlPriv, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	ed := Ed25519Signer{Priv: edPriv}
	ml := MLDSASigner{Priv: mlPriv}
	hy := HybridSigner{Ed: ed, ML: ml}
	st, err := NewAuditedStore(agent.NewMemStore(), ed, NewMemAnchorLog())
	if err != nil {
		t.Fatal(err)
	}

	secrets := [][]byte{edPriv, edPriv.Seed(), mlPriv.Bytes()}
	leaks := func(s string) bool {
		for _, k := range secrets {
			for _, form := range []string{fmt.Sprint(k), fmt.Sprintf("%x", k), fmt.Sprintf("%X", k), string(k),
				strings.Trim(fmt.Sprintf("%q", k), `"`)} {
				if strings.Contains(s, form) {
					return true
				}
			}
			b64, _ := json.Marshal(k)
			if strings.Contains(s, strings.Trim(string(b64), `"`)) {
				return true
			}
		}
		return false
	}

	for _, c := range []struct {
		name string
		v    any
		want string
	}{
		{"Ed25519Signer", ed, "audit.Ed25519Signer{Priv:[redacted]}"},
		{"*Ed25519Signer", &ed, "audit.Ed25519Signer{Priv:[redacted]}"},
		{"MLDSASigner", ml, "audit.MLDSASigner{Priv:[redacted]}"},
		{"HybridSigner", hy, "audit.HybridSigner{Ed:[redacted], ML:[redacted]}"},
		{"*AuditedStore", st, "audit.AuditedStore{signer:[redacted]}"},
	} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%q"} {
			got := fmt.Sprintf(verb, c.v)
			if got != c.want || leaks(got) {
				t.Errorf("%s formatted with %s = %.80q, want %q", c.name, verb, got, c.want)
			}
		}
		if s, ok := c.v.(fmt.Stringer); !ok || s.String() != c.want {
			t.Errorf("%s has no String method returning %q", c.name, c.want)
		}
	}

	// A struct holding a signer, as a config would: the encoders refuse rather than write the key.
	type config struct {
		Name   string
		Signer Signer
	}
	for _, s := range []Signer{ed, ml, hy} {
		out, err := json.Marshal(config{Name: "prod", Signer: s})
		if err == nil || leaks(string(out)) {
			t.Errorf("json.Marshal of a config holding %T = %.80q, %v; want an error and no key", s, out, err)
		}
		var buf bytes.Buffer
		gob.Register(s)
		err = gob.NewEncoder(&buf).Encode(config{Name: "prod", Signer: s})
		if err == nil || leaks(buf.String()) {
			t.Errorf("gob encoding of a config holding %T succeeded or wrote the key (err %v)", s, err)
		}
	}
}
