package audit

import (
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// identityPolicyFile is the JSON form of an IdentityPolicy, for the commands that check a
// certificate-bound decision outside a running gate (bide approve -verify-receipt, bide audit
// verify-receipts). Unknown members are refused. Durations are Go duration strings, times RFC 3339,
// hashes lowercase hex SHA-256 of a SubjectPublicKeyInfo, OIDs dotted decimal.
type identityPolicyFile struct {
	// Anchors are PEM files of the trusted CA certificates, relative to the policy file's directory
	// (no path may leave it).
	Anchors       []string          `json:"anchors"`
	Intermediates []string          `json:"intermediates"`
	Distrust      map[string]string `json:"distrust"`
	Identity      map[string]string `json:"identity"`
	IDKind        string            `json:"id_kind"`
	EKU           string            `json:"eku"`
	AllowEmailEKU bool              `json:"allow_email_protection_eku"`
	PolicyOID     string            `json:"policy_oid"`
	Canonical     struct {
		Scheme string `json:"scheme"`
		Host   string `json:"host"`
		Path   string `json:"path"`
	} `json:"canonical"`
	MaxChain           int    `json:"max_chain"`
	Skew               string `json:"skew"`
	AnchorGrace        string `json:"anchor_grace"`
	RSA2048Until       string `json:"rsa2048_until"`
	MaxLeafLifetime    string `json:"max_leaf_lifetime"`
	RequireNameConstr  bool   `json:"require_name_constraints"`
	RequirePolicyOnCAs bool   `json:"require_policy_on_cas"`
}

// ReadIdentityPolicy reads a certificate policy file (see identityPolicyFile for its members) and
// compiles it. Every failure wraps agent.ErrConfig: a check against a policy that did not load must
// not fall back to a weaker one.
func ReadIdentityPolicy(path string) (*CertIdentity, error) {
	cfgErr := func(format string, args ...any) (*CertIdentity, error) {
		return nil, fmt.Errorf("audit: identity policy file %s: "+format+": %w", append(append([]any{path}, args...), agent.ErrConfig)...)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfgErr("%v", err)
	}
	var f identityPolicyFile
	if err := UnmarshalStrict(b, &f); err != nil {
		return cfgErr("%v", err)
	}
	p := IdentityPolicy{
		Identity: f.Identity, IDKind: IDKind(f.IDKind), AllowEmailProtectionEKU: f.AllowEmailEKU, MaxChain: f.MaxChain,
		RequireNameConstraints: f.RequireNameConstr, RequirePolicyOnCAs: f.RequirePolicyOnCAs,
	}
	dir := filepath.Dir(path)
	for _, a := range f.Anchors {
		if filepath.IsAbs(a) || !filepath.IsLocal(a) {
			return cfgErr("anchor %q must be a file inside the policy file's directory", a)
		}
		pemBytes, err := os.ReadFile(filepath.Join(dir, a))
		if err != nil {
			return cfgErr("%v", err)
		}
		n := 0
		for blk, rest := pem.Decode(pemBytes); blk != nil; blk, rest = pem.Decode(rest) {
			if blk.Type != "CERTIFICATE" {
				return cfgErr("anchor file %s holds a %q block, not a certificate", a, blk.Type)
			}
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				return cfgErr("anchor file %s: %v", a, err)
			}
			p.Anchors = append(p.Anchors, c)
			n++
		}
		if n == 0 {
			return cfgErr("anchor file %s holds no PEM certificate", a)
		}
	}
	spki := func(s string) (SPKIHash, error) {
		var h SPKIHash
		raw, err := hex.DecodeString(s)
		if err != nil || len(raw) != len(h) {
			return h, fmt.Errorf("%q is not a SHA-256 hash in hex", s)
		}
		copy(h[:], raw)
		return h, nil
	}
	for _, s := range f.Intermediates {
		h, err := spki(s)
		if err != nil {
			return cfgErr("intermediates: %v", err)
		}
		p.Inter = append(p.Inter, h)
	}
	if len(f.Distrust) > 0 {
		p.Distrust = map[SPKIHash]time.Time{}
	}
	for s, ts := range f.Distrust {
		h, err := spki(s)
		if err != nil {
			return cfgErr("distrust: %v", err)
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return cfgErr("distrust %s: %v", s[:min(8, len(s))], err)
		}
		p.Distrust[h] = t
	}
	if p.EKU, err = parseOID(f.EKU); err != nil {
		return cfgErr("eku: %v", err)
	}
	if f.PolicyOID != "" {
		if p.Policy, err = x509.ParseOID(f.PolicyOID); err != nil {
			return cfgErr("policy_oid: %v", err)
		}
	}
	cc := CanonicalConfig{Scheme: f.Canonical.Scheme, Host: f.Canonical.Host}
	if f.Canonical.Path != "" {
		if cc.Path, err = regexp.Compile(f.Canonical.Path); err != nil {
			return cfgErr("canonical.path: %v", err)
		}
	}
	p.Canonical = NewCanonicalizer(cc)
	for _, d := range []struct {
		name string
		s    string
		into *time.Duration
	}{{"skew", f.Skew, &p.Skew}, {"anchor_grace", f.AnchorGrace, &p.AnchorGrace}, {"max_leaf_lifetime", f.MaxLeafLifetime, &p.MaxLeafLifetime}} {
		if d.s == "" {
			continue
		}
		if *d.into, err = time.ParseDuration(d.s); err != nil {
			return cfgErr("%s: %v", d.name, err)
		}
	}
	if f.RSA2048Until != "" {
		if p.RSA2048Until, err = time.Parse(time.RFC3339, f.RSA2048Until); err != nil {
			return cfgErr("rsa2048_until: %v", err)
		}
	}
	return p.Compile()
}

// parseOID reads a dotted-decimal object identifier.
func parseOID(s string) (asn1.ObjectIdentifier, error) {
	o, err := x509.ParseOID(s)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(o.String(), ".")
	out := make(asn1.ObjectIdentifier, len(parts))
	for i, p := range parts {
		if _, err := fmt.Sscanf(p, "%d", &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}
