package audit

import (
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// CRLSource is where the refresher fetches the CRLs of one issuing CA from. Only URLs the operator
// configured here (or, with FromPinnedCDP, those in the pinned certificate that names this CA's
// CRL) are ever fetched: never a URL from a client-supplied chain.
type CRLSource struct {
	// URLs are the CRL (base) locations, http or https. Every URL is asked on each attempt and the
	// highest-numbered valid CRL wins, so list every mirror of the same CRL, never different CRLs.
	URLs []string `json:"urls"`
	// DeltaURLs are the delta CRL locations, if the CA publishes deltas.
	DeltaURLs []string `json:"delta_urls"`
	// FromPinnedCDP adds the CRL distribution point (and freshest CRL) URLs of the supplied
	// certificate whose issuer is this CA. It helps only for a CA above the leaf's issuing CA: a leaf
	// certificate names its issuer's CRL, and leaves arrive from clients and are never a source.
	FromPinnedCDP bool `json:"from_pinned_cdp"`
}

// CRLSources maps an issuing CA, by the SHA-256 of its public key, to where its CRLs are fetched. The
// URLs are not part of the policy digest: a verdict depends on the CRL (verified against the pinned
// key), never on the URL it came from.
type CRLSources map[SPKIHash]CRLSource

// checkCRLURL applies the URL rules: http or https, a host, no userinfo, no fragment. LDAP is named
// because it is the one banks ask about.
func checkCRLURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%q is not a URL", redactURL(raw))
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "ldap", "ldaps":
		return nil, fmt.Errorf("%q: LDAP is not supported, only http and https (publish the CRL over HTTP)", redactURL(raw))
	default:
		return nil, fmt.Errorf("%q: scheme %q is not http or https", redactURL(raw), u.Scheme)
	}
	switch {
	case u.Hostname() == "":
		return nil, fmt.Errorf("%q has no host", redactURL(raw))
	case u.User != nil:
		return nil, fmt.Errorf("%q carries credentials; none may be placed in a CRL URL", redactURL(raw))
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return nil, fmt.Errorf("%q has a fragment", redactURL(raw))
	}
	return u, nil
}

// redactURL removes the userinfo, query and fragment, so a token in a URL is never printed.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if i := strings.IndexAny(raw, "?#@"); i >= 0 {
			return raw[:i] + "..."
		}
		return raw
	}
	u.User, u.RawQuery, u.Fragment, u.ForceQuery = nil, "", "", false
	return u.String()
}

// Validate checks every URL. It does not need the policy; NewCRLRefresher checks the keys.
func (s CRLSources) Validate() error {
	for _, k := range slices.SortedFunc(mapKeys(s), cmpHash) {
		src := s[k]
		if len(src.URLs) == 0 && !src.FromPinnedCDP {
			return fmt.Errorf("audit: CRL source for %x has no urls: %w", k[:4], agent.ErrConfig)
		}
		for _, list := range [][]string{src.URLs, src.DeltaURLs} {
			for _, raw := range list {
				if _, err := checkCRLURL(raw); err != nil {
					return fmt.Errorf("audit: CRL source for %x: %v: %w", k[:4], err, agent.ErrConfig)
				}
			}
		}
	}
	return nil
}

func mapKeys[K comparable, V any](m map[K]V) func(yield func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func cmpHash(a, b SPKIHash) int { return slices.Compare(a[:], b[:]) }

// ParseCRLSources reads the refresher members of a certificate policy file: "crl" (the sources, keyed by the
// lowercase hex SPKI hash of the issuing CA) and "intermediate_certs" (PEM files of the pinned
// intermediates, relative to the policy file's directory like the anchors). The same bytes are what
// ParseIdentityPolicy reads for the policy; this reads the members it ignores. readFile returns a
// file's bytes by its path relative to the policy file's directory (this package does no file I/O).
func ParseCRLSources(name string, b []byte, readFile func(rel string) ([]byte, error)) (CRLSources, []*x509.Certificate, error) {
	cfgErr := func(format string, args ...any) (CRLSources, []*x509.Certificate, error) {
		return nil, nil, fmt.Errorf("audit: identity policy file %s: "+format+": %w", append(append([]any{name}, args...), agent.ErrConfig)...)
	}
	var f identityPolicyFile
	if err := UnmarshalStrict(b, &f); err != nil {
		return cfgErr("%v", err)
	}
	src := CRLSources{}
	for k, v := range f.CRL {
		raw, err := hex.DecodeString(k)
		if err != nil || len(raw) != len(SPKIHash{}) || k != strings.ToLower(k) {
			return cfgErr("crl key %q is not a lowercase hex SHA-256", k)
		}
		src[SPKIHash(raw)] = v
	}
	if err := src.Validate(); err != nil {
		return nil, nil, err
	}
	var certs []*x509.Certificate
	for _, rel := range f.IntermediateCerts {
		if filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
			return cfgErr("intermediate certificate %q must be a file inside the policy file's directory", rel)
		}
		data, err := readFile(rel)
		if err != nil {
			return cfgErr("intermediate certificate %q: %v", rel, err)
		}
		n := 0
		for {
			var blk *pem.Block
			blk, data = pem.Decode(data)
			if blk == nil {
				break
			}
			if blk.Type != "CERTIFICATE" {
				return cfgErr("intermediate certificate %q holds a %q block, not a CERTIFICATE", rel, blk.Type)
			}
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				return cfgErr("intermediate certificate %q: %v", rel, err)
			}
			certs = append(certs, c)
			n++
		}
		if n == 0 {
			return cfgErr("intermediate certificate %q holds no PEM certificate", rel)
		}
	}
	return src, certs, nil
}
