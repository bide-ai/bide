package audit

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"time"
)

// The pieces of the status machinery a CRL refresher (package audit/crlrefresh, which does the network and
// file I/O this package must not) needs from it. They are mechanism, not policy: the verdicts stay in
// EvaluateStatus.

// CRL is one CRL as a refresher sees it: what the snapshot holds, or what VerifyCRL returned.
type CRL struct {
	Hash       [32]byte // SHA-256 of the DER
	TBSHash    [32]byte // SHA-256 of the TBSCertList: a re-signed copy of one CRL has the same value
	Number     *big.Int
	ThisUpdate time.Time
	NextUpdate time.Time
	Delta      bool
	BaseNumber *big.Int // for a delta, the base number it is cumulative since
	v          *crlView
}

func newCRL(v *crlView) *CRL {
	return &CRL{Hash: v.hash, TBSHash: v.tbs, Number: v.number, ThisUpdate: v.this, NextUpdate: v.next,
		Delta: v.delta, BaseNumber: v.baseNumber, v: v}
}

// Serials returns the decimal serial numbers the CRL lists, sorted.
func (c *CRL) Serials() []string {
	out := make([]string, 0, len(c.v.entries))
	for k := range c.v.entries {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// ErrCRLSignature is wrapped by VerifyCRL when the CRL's algorithm is outside the profile, its signature does
// not verify under the issuer's certificate, or it names another issuer.
var ErrCRLSignature = errors.New("audit: the CRL does not verify under the issuing CA")

// VerifyCRL checks der as a CRL of the given issuing CA: the signature algorithm profile (never SHA-1),
// the signature against that one certificate, the issuer name, and the structure rules of AddCRL. It adds
// nothing to any snapshot. A failure of the first three wraps ErrCRLSignature; a structure the status rule
// will not read without guessing is a *StatusError.
func VerifyCRL(der []byte, issuer *x509.Certificate, maxEntries int) (*CRL, error) {
	if err := authenticateWith(der, []*x509.Certificate{issuer}); err != nil {
		var se *StatusError
		detail := err.Error()
		if errors.As(err, &se) {
			detail = se.Detail
		}
		return nil, fmt.Errorf("%w: %s", ErrCRLSignature, detail)
	}
	v, err := parseCRL(der, sha256.Sum256(der), maxEntries)
	if err != nil {
		return nil, err
	}
	if string(v.rl.RawIssuer) != string(issuer.RawSubject) {
		return nil, fmt.Errorf("%w: the CRL's issuer name is not the issuing CA's subject", ErrCRLSignature)
	}
	return newCRL(v), nil
}

// CRLTBSHash is the SHA-256 of the TBSCertList of a CRL's DER, for comparing two CRLs without their
// (possibly randomised) signatures. It is false when der does not parse as a CertificateList.
func CRLTBSHash(der []byte) ([32]byte, bool) {
	var o outerCRL
	if rest, err := asn1.Unmarshal(der, &o); err != nil || len(rest) != 0 {
		return [32]byte{}, false
	}
	return sha256.Sum256(o.TBS.FullBytes), true
}

// HeldCRLs returns the CRLs the snapshot holds that verify under issuer, ordered by thisUpdate and then
// number.
func (s *StatusSnapshot) HeldCRLs(issuer *x509.Certificate) []*CRL {
	views, _ := s.viewsFor(issuer, time.Time{})
	out := make([]*CRL, len(views))
	for i, v := range views {
		out[i] = newCRL(v)
	}
	return out
}

// RemoveCRL drops the CRL with the given hash from the snapshot without touching the eviction count
// (Evicted), and reports whether it was held. A refresher uses it only for an operator's reset of a
// poisoned CRL number.
func (s *StatusSnapshot) RemoveCRL(hash [32]byte) bool { return s.remove(hash) }

// ErrSnapshotFull is returned by AddCRLNoEvict when adding the CRL would evict another.
var ErrSnapshotFull = errors.New("audit: the status snapshot is full")

// AddCRLNoEvict is AddCRL that never drops a CRL to make room: with the snapshot full it returns
// ErrSnapshotFull. A refresher seeds from its archive with it so that loading old CRLs cannot raise the
// eviction count and make live status non-final.
func (s *StatusSnapshot) AddCRLNoEvict(der []byte) (CRLInfo, error) {
	info, err := s.addCRL(der, false)
	if errors.Is(err, errSnapshotFull) {
		return info, ErrSnapshotFull
	}
	return info, err
}

// Limits returns the largest CRL the snapshot accepts in bytes and in entries, and how many CRLs it keeps
// (0: unbounded).
func (s *StatusSnapshot) Limits() (maxBytes, maxEntries, maxCRLs int) {
	return s.maxBytes, s.maxEntries, s.maxCRLs
}

// StatusFresh is the live freshness test the status rule applies to every CRL, so a refresher's alarm
// cannot disagree with a gate: a CRL is fresh when now is before nextUpdate, its age is at most maxAge plus
// the declared thisUpdate skew, and thisUpdate is not beyond the coverage tolerance in the future. A
// non-positive maxAge accepts nothing.
func StatusFresh(now, this, next time.Time, maxAge, skew, tolerance time.Duration) bool {
	return statusFresh(now, this, next, maxAge, skew, tolerance)
}

// IsTrustAnchor reports whether the key is one of the policy's trust anchors.
func (c *CertIdentity) IsTrustAnchor(k SPKIHash) bool { return c.anchorBy[k] != nil }

// IsPinnedIntermediate reports whether the key is a pinned intermediate.
func (c *CertIdentity) IsPinnedIntermediate(k SPKIHash) bool { return c.inter[k] }

// PinnedIntermediates returns the pinned intermediate keys, sorted.
func (c *CertIdentity) PinnedIntermediates() []SPKIHash {
	out := make([]SPKIHash, 0, len(c.inter))
	for k := range c.inter {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b SPKIHash) int { return slices.Compare(a[:], b[:]) })
	return out
}

// DistrustFrom returns when the policy ends trust in the key (zero when it does not).
func (c *CertIdentity) DistrustFrom(k SPKIHash) time.Time { return c.distrust[k] }

// TrustRoots returns the policy's trust anchors as a certificate pool.
func (c *CertIdentity) TrustRoots() *x509.CertPool { return c.anchors }

// CheckCRLURL applies the URL rules of a CRL source: http or https, a host, no userinfo, no fragment. LDAP
// is named because it is the one banks ask about.
func CheckCRLURL(raw string) (*url.URL, error) { return checkCRLURL(raw) }

// RedactURL removes the userinfo, query and fragment from a URL, so a token in one is never printed.
func RedactURL(raw string) string { return redactURL(raw) }
