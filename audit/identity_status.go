package audit

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Certificate status from CRLs (docs/design/approver-identity.md, "Revocation" and "Status rule").
//
// The model: a StatusSnapshot holds CRLs, parsed and refused when they are outside what bide can read
// without guessing. Nothing in it is trusted yet: a CRL counts for a certificate only when it
// verifies under that certificate's issuing CA, which is checked when a chain is evaluated
// (CertIdentity.EvaluateStatus), against the issuer certificate of the accepted chain. The
// evaluation answers three questions per certificate: was it revoked with an effective time at or
// before the moment the decision became provable (A), is there evidence that it was not, and how
// strong is that evidence (a tier).

const (
	defaultMaxCRLBytes   = 16 << 20 // a larger CRL is refused before parsing (WithLimits changes it)
	defaultMaxCRLEntries = 1_000_000
	defaultMaxCRLs       = 1024 // CRLs a snapshot keeps; the oldest by thisUpdate are evicted beyond it

	defaultCoverageTolerance = time.Minute

	defaultW    = 24 * time.Hour   // compromise-reporting window
	defaultMinW = 30 * time.Minute // floor on the window for a final verdict
	// maxThisUpdateSkew caps a declared ThisUpdateSkew: a larger value would make a stale CRL look fresh.
	maxThisUpdateSkew = 30 * time.Minute
)

// finalityNeedsLC chooses how a certificate that is still unexpired at the latest usable status is
// judged. false (the user's decision of 2026-10-15): no CA can have pruned an entry of an unexpired
// certificate, so L(C) is not needed and the status is final once it reaches A+W' with time
// coverage from A. true: the design note's earlier literal rule, under which a chain through an
// unexpired CA certificate is provisional until that CA certificate has expired and a CRL beyond
// it exists. A variable only so a test can exercise both.
var finalityNeedsLC = false

// statusCaveat is stated with every verdict that rests on status: later reports are outside it.
const statusCaveat = "revocations first reported after S are not covered"

var (
	oidCRLNumber           = asn1.ObjectIdentifier{2, 5, 29, 20}
	oidIssuerAltName       = asn1.ObjectIdentifier{2, 5, 29, 18}
	oidAuthorityKeyID      = asn1.ObjectIdentifier{2, 5, 29, 35}
	oidFreshestCRL         = asn1.ObjectIdentifier{2, 5, 29, 46}
	oidExpiredCertsOnCRL   = asn1.ObjectIdentifier{2, 5, 29, 60}
	oidIssuingDistPoint    = asn1.ObjectIdentifier{2, 5, 29, 28}
	oidDeltaIndicator      = asn1.ObjectIdentifier{2, 5, 29, 27}
	oidEntryReasonCode     = asn1.ObjectIdentifier{2, 5, 29, 21}
	oidEntryHoldInstr      = asn1.ObjectIdentifier{2, 5, 29, 23}
	oidEntryInvalidityDate = asn1.ObjectIdentifier{2, 5, 29, 24}
	oidEntryCertIssuer     = asn1.ObjectIdentifier{2, 5, 29, 29}
)

// StatusError is a status failure. Reason is agent.ReasonStatusUnknown, agent.ReasonStatusStale or
// agent.ReasonRevoked; Detail says what failed.
type StatusError struct {
	Reason string
	Detail string
}

// Error reports the reason and the detail.
func (e *StatusError) Error() string { return "certificate status: " + e.Reason + ": " + e.Detail }

func statusErr(reason, format string, args ...any) error {
	return &StatusError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// crlEntry is one revocation, with the raw facts the effective time is computed from.
type crlEntry struct {
	revoked time.Time
	invalid time.Time // zero when the entry has no invalidity date
	code    int
	hasCode bool
}

// crlView is one CRL after structural checks. It is not yet tied to an issuer.
type crlView struct {
	hash        [32]byte
	rl          *x509.RevocationList
	number      *big.Int
	this, next  time.Time
	entries     map[string][]crlEntry // by serial number (decimal)
	expiredFrom time.Time             // expiredCertsOnCRL date; zero when absent
	delta       bool                  // a delta CRL: merged with a base for evidence of absence
	baseNumber  *big.Int              // for a delta: the CRL number of the base it is cumulative since
	parts       [][32]byte            // for a merged view: the hashes of the base and the delta
}

// hashes are the CRLs this view rests on.
func (v *crlView) hashes() [][32]byte {
	if v.parts != nil {
		return v.parts
	}
	return [][32]byte{v.hash}
}

// CRLInfo describes a CRL accepted into a StatusSnapshot.
type CRLInfo struct {
	Hash       [32]byte // SHA-256 of the DER
	Number     *big.Int
	ThisUpdate time.Time
	NextUpdate time.Time
	Entries    int
}

type sigKey struct{ crl, issuer [32]byte }

// StatusSnapshot is a set of CRLs: what a gate fetched, or an auditor's archived series. It is safe
// for concurrent use.
type StatusSnapshot struct {
	mu         sync.Mutex
	views      []*crlView
	by         map[[32]byte]*crlView
	sigs       map[sigKey]bool // memo: does this CRL verify under this issuer certificate
	issuers    []*x509.Certificate
	maxBytes   int
	maxEntries int
	maxCRLs    int // 0: unbounded
	evicted    int
	evictedNew time.Time // the latest thisUpdate among evicted CRLs
}

// SnapshotOption configures NewStatusSnapshot.
type SnapshotOption func(*StatusSnapshot)

// WithIssuers names the CA certificates whose CRLs the snapshot is for. A CRL is then authenticated
// before its entries are built: its signature must verify under one of them or AddCRL refuses it.
// Without issuers the snapshot accepts any well-formed CRL and authenticates only when a chain is
// evaluated, which costs memory for CRLs that are later discarded; a refresher that knows its issuer
// should always set it.
func WithIssuers(certs ...*x509.Certificate) SnapshotOption {
	return func(s *StatusSnapshot) { s.issuers = append(s.issuers, certs...) }
}

// WithLimits sets the largest CRL accepted in bytes and in entries (defaults 16 MiB and 1,000,000).
func WithLimits(maxBytes, maxEntries int) SnapshotOption {
	return func(s *StatusSnapshot) { s.maxBytes, s.maxEntries = maxBytes, maxEntries }
}

// WithMaxCRLs bounds how many CRLs the snapshot keeps (default 1024; a value below 1 keeps the default). Beyond it the CRL with the
// earliest thisUpdate is evicted, so a long-running refresher does not grow without limit. A policy
// that needs a longer series for a decision (the journal's evidence) keeps it in the archive, not
// here.
func WithMaxCRLs(n int) SnapshotOption {
	return func(s *StatusSnapshot) {
		if n >= 1 {
			s.maxCRLs = n
		}
	}
}

// WithUnboundedCRLs removes the bound on how many CRLs the snapshot keeps, for an audit tool that
// loads an archived series and must not lose any of it. A long-running refresher should not use it.
func WithUnboundedCRLs() SnapshotOption { return func(s *StatusSnapshot) { s.maxCRLs = 0 } }

// Evicted is the number of CRLs the snapshot has dropped to stay within WithMaxCRLs. An evaluation
// that needs a dropped CRL says so (CertStatus.EvictionGap) instead of reading the absence as
// silence.
func (s *StatusSnapshot) Evicted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evicted
}

// NewStatusSnapshot returns an empty snapshot.
func NewStatusSnapshot(opts ...SnapshotOption) *StatusSnapshot {
	s := &StatusSnapshot{by: map[[32]byte]*crlView{}, sigs: map[sigKey]bool{},
		maxBytes: defaultMaxCRLBytes, maxEntries: defaultMaxCRLEntries, maxCRLs: defaultMaxCRLs}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Len is the number of distinct CRLs held.
func (s *StatusSnapshot) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.views)
}

// AddCRL parses a CRL and keeps it, or refuses it with a *StatusError (ReasonStatusUnknown) naming why.
// Adding the same CRL twice keeps one.
//
// A CRL is refused when bide cannot read it without guessing, because a wrong reading is a wrong
// "not revoked": a signature algorithm outside the profile (never SHA-1), no CRL number, no
// nextUpdate (RFC 5280 requires it; absent means unknown), an issuing distribution point (partitioned
// CRLs), an indirect CRL (a certificateIssuer entry extension), any critical extension bide does
// not process, removeFromCRL in a full CRL, a malformed invalidity date, and a malformed delta
// indicator (a delta CRL must carry a critical delta CRL indicator, a DER INTEGER: the number of the
// base it is cumulative since; it is merged with a base when a chain is evaluated and never gives
// evidence of absence alone). The signature is not checked here: that needs the issuing certificate,
// which belongs to a chain (see CertIdentity.EvaluateStatus).
func (s *StatusSnapshot) AddCRL(der []byte) (CRLInfo, error) {
	if len(der) == 0 || len(der) > s.maxBytes {
		return CRLInfo{}, statusErr(agent.ReasonStatusUnknown, "the CRL is %d bytes, outside 1 to %d", len(der), s.maxBytes)
	}
	hash := sha256.Sum256(der)
	s.mu.Lock()
	if v, ok := s.by[hash]; ok {
		s.mu.Unlock()
		return v.info(), nil
	}
	s.mu.Unlock()

	if err := s.authenticate(der); err != nil {
		return CRLInfo{}, err
	}
	v, err := parseCRL(der, hash, s.maxEntries)
	if err != nil {
		return CRLInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.by[hash]; ok {
		return old.info(), nil
	}
	s.by[hash] = v
	s.views = append(s.views, v)
	for s.maxCRLs > 0 && len(s.views) > s.maxCRLs {
		oldest := 0
		for i, x := range s.views {
			if x.this.Before(s.views[oldest].this) {
				oldest = i
			}
		}
		gone := s.views[oldest]
		s.evicted++
		if gone.this.After(s.evictedNew) {
			s.evictedNew = gone.this
		}
		s.views = slices.Delete(s.views, oldest, oldest+1)
		delete(s.by, gone.hash)
		for k := range s.sigs {
			if k.crl == gone.hash {
				delete(s.sigs, k)
			}
		}
	}
	return v.info(), nil
}

// outerCRL is the CertificateList envelope: enough to check the signature without reading entries.
type outerCRL struct {
	TBS asn1.RawValue
	Alg pkix.AlgorithmIdentifier
	Sig asn1.BitString
}

// crlSigAlgs maps the algorithm identifiers whose OID alone fixes the algorithm and which the profile
// allows. RSA-PSS carries parameters (see pssAlgorithm); anything else is outside the profile.
var crlSigAlgs = map[string]x509.SignatureAlgorithm{
	"1.2.840.10045.4.3.2":     x509.ECDSAWithSHA256,
	"1.2.840.10045.4.3.3":     x509.ECDSAWithSHA384,
	"1.2.840.113549.1.1.11":   x509.SHA256WithRSA,
	"1.2.840.113549.1.1.12":   x509.SHA384WithRSA,
	"1.2.840.113549.1.1.13":   x509.SHA512WithRSA,
	"1.3.101.112":             x509.PureEd25519,
	"2.16.840.1.101.3.4.3.18": x509.MLDSA65,
}

var (
	oidRSAPSS = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 10}
	oidMGF1   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 8}
	pssHashes = map[string]struct {
		alg  x509.SignatureAlgorithm
		size int
	}{
		"2.16.840.1.101.3.4.2.1": {x509.SHA256WithRSAPSS, 32},
		"2.16.840.1.101.3.4.2.2": {x509.SHA384WithRSAPSS, 48},
		"2.16.840.1.101.3.4.2.3": {x509.SHA512WithRSAPSS, 64},
	}
)

// pssAlgorithm reads RSASSA-PSS parameters (RFC 4055) strictly: the received parameters must be
// byte-for-byte the canonical DER encoding for a SHA-2 hash, MGF1 over the same hash, a salt as long
// as the digest and the default trailer (the NULL parameters of the two hash identifiers may be
// present or absent). The ASN.1 decoder ignores extra elements, so the check is by re-encoding, not by
// decoding: a duplicated field, an unknown trailing element or garbage inside an identifier all fail.
func pssAlgorithm(params asn1.RawValue) (x509.SignatureAlgorithm, bool) {
	var p struct {
		Hash pkix.AlgorithmIdentifier `asn1:"explicit,tag:0,optional"`
	}
	if _, err := asn1.Unmarshal(params.FullBytes, &p); err != nil {
		return 0, false
	}
	h, ok := pssHashes[p.Hash.Algorithm.String()]
	if !ok {
		return 0, false
	}
	for _, nullHash := range []bool{true, false} {
		for _, nullMGF := range []bool{true, false} {
			if bytes.Equal(params.FullBytes, canonicalPSS(p.Hash.Algorithm, h.size, nullHash, nullMGF)) {
				return h.alg, true
			}
		}
	}
	return 0, false
}

// canonicalPSS is the DER encoding of the PSS parameters pssAlgorithm accepts.
func canonicalPSS(hash asn1.ObjectIdentifier, salt int, nullHash, nullMGF bool) []byte {
	must := func(b []byte, err error) []byte {
		if err != nil {
			panic(err)
		}
		return b
	}
	seq := func(content ...[]byte) []byte {
		return must(asn1.Marshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: bytes.Join(content, nil)}))
	}
	explicit := func(tag int, content []byte) []byte {
		return must(asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: tag, IsCompound: true, Bytes: content}))
	}
	alg := func(oid asn1.ObjectIdentifier, withNull bool) []byte {
		if withNull {
			return seq(must(asn1.Marshal(oid)), []byte{0x05, 0x00})
		}
		return seq(must(asn1.Marshal(oid)))
	}
	hashAI := alg(hash, nullHash)
	mgfAI := seq(must(asn1.Marshal(oidMGF1)), alg(hash, nullMGF))
	return seq(explicit(0, hashAI), explicit(1, mgfAI), explicit(2, must(asn1.Marshal(salt))))
}

// authenticate checks the CRL signature against the configured issuers before the entries are built.
// With no issuers it does nothing (the signature is checked when a chain is evaluated). With issuers,
// an algorithm outside the profile (SHA-1 and MD5 included) is refused here, before any entry is
// parsed, and so is a signature no issuer made.
func (s *StatusSnapshot) authenticate(der []byte) error {
	if len(s.issuers) == 0 {
		return nil
	}
	var o outerCRL
	if rest, err := asn1.Unmarshal(der, &o); err != nil || len(rest) != 0 {
		return statusErr(agent.ReasonStatusUnknown, "the CRL envelope does not parse")
	}
	algo, ok := crlSigAlgs[o.Alg.Algorithm.String()]
	if !ok && o.Alg.Algorithm.Equal(oidRSAPSS) {
		algo, ok = pssAlgorithm(o.Alg.Parameters)
	}
	if !ok {
		return statusErr(agent.ReasonStatusUnknown, "the CRL signature algorithm %s is outside the profile", o.Alg.Algorithm)
	}
	if !slices.ContainsFunc(s.issuers, func(c *x509.Certificate) bool {
		return c.CheckSignature(algo, o.TBS.FullBytes, o.Sig.RightAlign()) == nil
	}) {
		return statusErr(agent.ReasonStatusUnknown, "the CRL signature does not verify under any configured issuer")
	}
	return nil
}

func (v *crlView) info() CRLInfo {
	n := 0
	for _, es := range v.entries {
		n += len(es)
	}
	return CRLInfo{Hash: v.hash, Number: new(big.Int).Set(v.number), ThisUpdate: v.this, NextUpdate: v.next, Entries: n}
}

func parseCRL(der []byte, hash [32]byte, maxEntries int) (*crlView, error) {
	unreadable := func(format string, args ...any) (*crlView, error) {
		return nil, statusErr(agent.ReasonStatusUnknown, format, args...)
	}
	rl, err := x509.ParseRevocationList(der)
	if err != nil {
		return unreadable("the CRL does not parse: %v", err)
	}
	if len(rl.Raw) != len(der) {
		return unreadable("the CRL has %d trailing bytes", len(der)-len(rl.Raw))
	}
	if err := sigAlgAllowed(rl.SignatureAlgorithm); err != nil {
		return unreadable("the CRL signature: %v", err)
	}
	if rl.Number == nil || rl.Number.Sign() < 0 {
		return unreadable("the CRL has no CRL number")
	}
	if rl.ThisUpdate.IsZero() || rl.NextUpdate.IsZero() || !rl.NextUpdate.After(rl.ThisUpdate) {
		return unreadable("the CRL has no usable thisUpdate and nextUpdate (nextUpdate absent means status unknown)")
	}
	v := &crlView{hash: hash, rl: rl, number: rl.Number, this: rl.ThisUpdate, next: rl.NextUpdate, entries: map[string][]crlEntry{}}
	seen := map[string]bool{}
	for _, e := range rl.Extensions {
		if seen[e.Id.String()] {
			return unreadable("the CRL carries the extension %s twice", e.Id)
		}
		seen[e.Id.String()] = true
		switch {
		case e.Id.Equal(oidIssuingDistPoint):
			return unreadable("the CRL carries an issuing distribution point (a partitioned CRL cannot be matched correctly)")
		case e.Id.Equal(oidDeltaIndicator):
			base := new(big.Int)
			if rest, err := asn1.Unmarshal(e.Value, &base); err != nil || len(rest) != 0 || base.Sign() < 0 || !e.Critical {
				return unreadable("the delta CRL indicator must be a critical, non-negative DER INTEGER (the base CRL number)")
			}
			v.delta, v.baseNumber = true, base
		case e.Id.Equal(oidExpiredCertsOnCRL):
			var t time.Time
			if rest, err := asn1.Unmarshal(e.Value, &t); err != nil || len(rest) != 0 {
				return unreadable("the expiredCertsOnCRL extension is malformed")
			}
			v.expiredFrom = t
		case (e.Id.Equal(oidCRLNumber) || e.Id.Equal(oidAuthorityKeyID) || e.Id.Equal(oidIssuerAltName) || e.Id.Equal(oidFreshestCRL)) && !e.Critical:
		case e.Critical:
			return unreadable("the CRL has a critical extension %s that bide does not process", e.Id)
		}
	}
	if len(rl.RevokedCertificateEntries) > maxEntries {
		return unreadable("the CRL has %d entries, over the limit of %d", len(rl.RevokedCertificateEntries), maxEntries)
	}
	for i, re := range rl.RevokedCertificateEntries {
		if re.SerialNumber == nil || re.RevocationTime.IsZero() {
			return unreadable("entry %d has no serial number or revocation time", i)
		}
		ce := crlEntry{revoked: re.RevocationTime, code: re.ReasonCode}
		switch {
		case re.ReasonCode == 7 || re.ReasonCode < 0 || re.ReasonCode > 10:
			return unreadable("entry %d has the undefined reason code %d", i, re.ReasonCode)
		case re.ReasonCode == 8 && !v.delta:
			return unreadable("entry %d is removeFromCRL in a full CRL", i)
		}
		entrySeen := map[string]bool{}
		for _, x := range re.Extensions {
			if entrySeen[x.Id.String()] {
				return unreadable("entry %d carries the extension %s twice", i, x.Id)
			}
			entrySeen[x.Id.String()] = true
			switch {
			case x.Id.Equal(oidEntryCertIssuer):
				return unreadable("entry %d carries certificateIssuer (an indirect CRL)", i)
			case x.Id.Equal(oidEntryInvalidityDate):
				var t time.Time
				if rest, err := asn1.Unmarshal(x.Value, &t); err != nil || len(rest) != 0 {
					return unreadable("entry %d has a malformed invalidity date", i)
				}
				ce.invalid = t
			case x.Id.Equal(oidEntryReasonCode), x.Id.Equal(oidEntryHoldInstr):
			case x.Critical:
				return unreadable("entry %d has a critical extension %s that bide does not process", i, x.Id)
			}
		}
		k := re.SerialNumber.String()
		v.entries[k] = append(v.entries[k], ce)
	}
	return v, nil
}

// StatusTier is how strong the evidence of absence from the CRLs is.
type StatusTier int

// The tiers, weakest first.
const (
	// TierNoInformation: no usable status. A silent CRL that cannot be shown to cover the time
	// after the decision is no information, never "not revoked".
	TierNoInformation StatusTier = iota
	// TierProvisional: status exists but a later one could change the answer (it ends before
	// A+W', the window is short, or the series is not shown complete to L(C)).
	TierProvisional
	// TierFinal: status reaches A+W' over a window of at least MinW, and the series is complete.
	TierFinal
)

// String names the tier.
func (t StatusTier) String() string {
	switch t {
	case TierNoInformation:
		return "no information"
	case TierProvisional:
		return "provisional"
	case TierFinal:
		return "final"
	}
	return fmt.Sprintf("StatusTier(%d)", int(t))
}

// StatusParams are the knobs of the status rule. Zero values select the defaults.
type StatusParams struct {
	// W is the compromise-reporting window the bank commits to (default 24 hours). W' = min(W, L(C)
	// - A(d)) is what a certificate's own life leaves.
	W time.Duration
	// MinW is the smallest W' for a final verdict (default 30 minutes).
	MinW time.Duration
	// RoutineGrace: for routine reasons (affiliationChanged, superseded, cessationOfOperation,
	// privilegeWithdrawn) a decision with A < E + RoutineGrace still counts. Compromise reasons,
	// unspecified and certificateHold never get grace. Default 0.
	RoutineGrace time.Duration
	// Now, when non-zero, applies freshness: a CRL is evidence of absence only while Now < nextUpdate
	// and Now - thisUpdate <= MaxStatusAge, and not when dated in the future. It does not relax the
	// rule that evidence is issued at or after the decision (see CountLive). A freshness check without
	// a positive MaxStatusAge accepts no evidence (a configuration mistake fails closed). Leave Now
	// zero for an offline audit, which judges the series against the decision, not the clock.
	Now          time.Time
	MaxStatusAge time.Duration
	// CountLive is the live gate's counting mode and needs Now: a fresh CRL issued before the decision
	// is accepted, so a decision is not held for a CRL period. A verdict resting on status issued
	// before the decision is provisional, never final (its Basis says so): a revocation effective
	// before the decision may be reported after that CRL. A live count that finds a CRL at or after
	// the decision and its window can be final like any other. An audit never sets it.
	CountLive bool
	// Distrust adds distrust dates to the policy's own, by issuer key: status signed by that key with
	// thisUpdate at or after the date is no information. It is the auditor's overlay.
	Distrust map[SPKIHash]time.Time
	// Retained says the bank's CA keeps the entries of expired certificates for as long as audits
	// need them (recorded retention in the policy), as expiredCertsOnCRL does per CRL. It is the
	// bank's recorded statement, not something the CRLs show.
	Retained bool
	// CAPeriod is the CA's declared issuance period (the design note's CAPeriod, the same value that
	// bounds ExecStatusAge): consecutive CRLs of the series are issued no further apart than this.
	// Time coverage, and so the first CRL beyond notAfter (L(C)), is shown by it, not by nextUpdate: a
	// CA may issue earlier than nextUpdate (hourly issue, 24 hour validity), so a producer who omits a
	// CRL cannot be caught by nextUpdate alone. Zero declares nothing: with no CAPeriod and no
	// ConsecutiveNumbers no verdict is final. A series whose observed gap exceeds it carries a Warning.
	CAPeriod time.Duration
	// CAPeriodFor overrides CAPeriod for the issuing CA with the given key.
	CAPeriodFor map[SPKIHash]time.Duration
	// ConsecutiveNumbers is for a CA whose CRL numbers increase by exactly one with every CRL it
	// issues (bases and deltas alike): consecutive numbers then show that no CRL lies between two.
	// Off by default; a CA that skips numbers, shares a counter oddly or resets after a restore would
	// otherwise never reach final.
	ConsecutiveNumbers bool
	// CoverageTolerance absorbs publication jitter in time coverage (default one minute).
	CoverageTolerance time.Duration
	// ThisUpdateSkew is how much earlier than its real issue a CA stamps thisUpdate (Microsoft AD CS back-dates
	// it by about ten minutes). A CRL is then really issued at thisUpdate plus the skew: a hold released
	// before the next CRL is released no later than that, and a CRL's age is measured from it.
	// ThisUpdateSkewFor overrides it per issuing CA. Zero takes thisUpdate at face value; it is never
	// assumed. It is the CA's own configuration (AD CS: ClockSkewMinutes), declared from a capture of
	// that CA, with room for the variation of a few seconds (the release bound adds CoverageTolerance).
	// A negative value or one over 30 minutes is refused: the certificate has no information.
	ThisUpdateSkew    time.Duration
	ThisUpdateSkewFor map[SPKIHash]time.Duration
	// AllowHolds accepts certificates that have been on hold (certificateHold, removeFromCRL). Off by
	// default: a removeFromCRL entry's date is the CA's own (RFC 5280 does not define it as the
	// release time), so a CA that stamps the original hold date makes a release look old and could hide
	// a decision made during the hold when the CRLs that list the hold are missing. Without it any
	// hold or release entry for the certificate refuses it wholesale. Banks should not use holds on
	// approver certificates (revoke and re-issue); opt in only for a CA whose release dates have been
	// checked (the conformance captures record what EJBCA and AD CS write).
	AllowHolds bool
}

// CertStatus is the status finding for one certificate of a chain.
type CertStatus struct {
	Serial *big.Int
	// Revoked: a CRL lists the certificate with an effective time E at or before A(d) (outside any
	// routine grace). E, ReasonCode, Compromise and RevokedBy describe the listing with the earliest E.
	Revoked    bool
	E          time.Time
	ReasonCode int
	Compromise bool     // keyCompromise, cACompromise or aACompromise: the bank's process treats a counted decision as disputed
	RevokedBy  [32]byte // hash of the CRL that lists it
	// Tier is the evidence of absence when not Revoked.
	Tier StatusTier
	// S is the thisUpdate of the latest usable CRL, LC that of the first CRL beyond notAfter when
	// one is established (LCKnown), Unbounded when expired entries are retained.
	S, LC     time.Time
	LCKnown   bool
	Unbounded bool
	// Stale: usable-in-time CRLs exist but fail the live freshness rules.
	Stale bool
	// Window is W' for this certificate.
	Window time.Duration
	// Used are the hashes of the CRLs that count as evidence of absence, in thisUpdate order.
	Used [][32]byte
	// Note says why the tier is not final.
	Note string
	// Caveat is stated with the verdict: what the status does not cover. Empty only when there is no
	// status.
	Caveat string
	// PolicyRefusal: the finding is the status policy refusing a certificate that was ever on hold
	// (StatusParams.AllowHolds off), not a revocation the CA recorded for the decision's time. It
	// refuses even a decision made before a later hold, so it must not be read as "revoked at E".
	PolicyRefusal bool
	// Basis says what the status rests on: "status issued at or after the decision", or "live count:
	// status issued before the decision (provisional only)". A tier is not a verification: only Final
	// is, and a live count never is. Empty when there is no status.
	Basis string
	// Warning is set when the series shows something the declared CAPeriod does not allow (a gap
	// longer than the CA said it would leave), or the snapshot evicted CRLs the verdict needed
	// (EvictionGap). It does not change the tier by itself; the gap already limits it.
	Warning string
	// EvictionGap: the snapshot evicted CRLs from the range the verdict needs, so it is at most
	// provisional.
	EvictionGap bool
}

// IsFinal reports whether the finding is a verified one: not revoked, and final under the status rule.
// Provisional and no-information findings are not, and neither is a live count.
func (c CertStatus) IsFinal() bool { return !c.Revoked && c.Tier == TierFinal }

// ChainStatus is the finding for every non-anchor certificate of an accepted chain, leaf first.
type ChainStatus struct {
	Certs   []CertStatus
	Revoked bool       // any certificate revoked with E <= A(d)
	Tier    StatusTier // the weakest certificate's tier (meaningful when not Revoked)
}

// IsFinal reports whether every certificate of the chain is final and none is revoked. Err() == nil does
// not mean that: it means the status is usable (a live count may proceed on it) and nothing is
// revoked. Only IsFinal is verification.
func (cs ChainStatus) IsFinal() bool {
	if cs.Revoked || len(cs.Certs) == 0 {
		return false
	}
	for _, c := range cs.Certs {
		if !c.IsFinal() {
			return false
		}
	}
	return true
}

// Err is nil when the chain has usable status and no revocation (a provisional or live-count finding
// included: check IsFinal before calling a decision verified). Otherwise it is a *StatusError:
// ReasonRevoked, ReasonStatusStale (usable-in-time status exists but is too old) or
// ReasonStatusUnknown.
func (cs ChainStatus) Err() error {
	for i, c := range cs.Certs {
		if c.Revoked && c.PolicyRefusal {
			return statusErr(agent.ReasonRevoked, "certificate %d of the chain (serial %s) is refused by the status policy, not by a recorded revocation: %s", i, c.Serial, c.Note)
		}
		if c.Revoked {
			return statusErr(agent.ReasonRevoked, "certificate %d of the chain (serial %s) is revoked with effective time %s, reason code %d",
				i, c.Serial, c.E.UTC().Format(time.RFC3339), c.ReasonCode)
		}
	}
	for i, c := range cs.Certs {
		if c.Tier == TierNoInformation {
			if c.Stale {
				return statusErr(agent.ReasonStatusStale, "certificate %d of the chain (serial %s): %s", i, c.Serial, c.Note)
			}
		}
	}
	for i, c := range cs.Certs {
		if c.Tier == TierNoInformation {
			return statusErr(agent.ReasonStatusUnknown, "certificate %d of the chain (serial %s): %s", i, c.Serial, c.Note)
		}
	}
	return nil
}

// EvaluateStatus applies the status rule to every non-anchor certificate of ac's chain at A(d) = a,
// over the CRLs in s. A CRL counts for a certificate only if it verifies under that certificate's
// issuing CA in the chain; one signed by any other key is ignored, and so are entries for the same
// serial number from another issuer.
func (c *CertIdentity) EvaluateStatus(ac *ApproverCert, a time.Time, s *StatusSnapshot, p StatusParams) ChainStatus {
	if p.W <= 0 {
		p.W = defaultW
	}
	if p.MinW <= 0 {
		p.MinW = defaultMinW
	}
	if p.CoverageTolerance <= 0 {
		p.CoverageTolerance = defaultCoverageTolerance
	}
	out := ChainStatus{Tier: TierFinal}
	for i := 0; i+1 < len(ac.Chain); i++ {
		cert, issuer := ac.Chain[i], ac.Chain[i+1]
		views, evictedNew := s.viewsFor(issuer, c.distrustFor(issuer, p.Distrust))
		st := evalCert(cert, issuer, a, views, evictedNew, p)
		out.Certs = append(out.Certs, st)
		if st.Revoked {
			out.Revoked = true
		}
		out.Tier = min(out.Tier, st.Tier)
	}
	if len(out.Certs) == 0 {
		out.Tier = TierNoInformation
	}
	return out
}

// distrustFor is the earliest distrust date for issuer's key, from the policy and the overlay.
func (c *CertIdentity) distrustFor(issuer *x509.Certificate, overlay map[SPKIHash]time.Time) time.Time {
	k := SPKIHashOf(issuer)
	var t time.Time
	for _, m := range []map[SPKIHash]time.Time{c.distrust, overlay} {
		if d, ok := m[k]; ok && (t.IsZero() || d.Before(t)) {
			t = d
		}
	}
	return t
}

// viewsFor returns the CRLs that verify under issuer and are not from a distrusted time, ordered by
// thisUpdate and then CRL number.
func (s *StatusSnapshot) viewsFor(issuer *x509.Certificate, distrust time.Time) ([]*crlView, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ih := sha256.Sum256(issuer.Raw)
	var out []*crlView
	for _, v := range s.views {
		if !distrust.IsZero() && !v.this.Before(distrust) {
			continue
		}
		k := sigKey{v.hash, ih}
		ok, seen := s.sigs[k]
		if !seen {
			ok = signedBy(v.rl, issuer)
			s.sigs[k] = ok
		}
		if ok {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].this.Equal(out[j].this) {
			return out[i].this.Before(out[j].this)
		}
		return out[i].number.Cmp(out[j].number) < 0
	})
	return out, s.evictedNew
}

func signedBy(rl *x509.RevocationList, issuer *x509.Certificate) bool {
	if !bytes.Equal(rl.RawIssuer, issuer.RawSubject) {
		return false
	}
	if len(rl.AuthorityKeyId) > 0 && len(issuer.SubjectKeyId) > 0 && !bytes.Equal(rl.AuthorityKeyId, issuer.SubjectKeyId) {
		return false
	}
	return rl.CheckSignatureFrom(issuer) == nil
}

func isRoutine(code int) bool    { return code == 3 || code == 4 || code == 5 || code == 9 }
func isCompromise(code int) bool { return code == 1 || code == 2 || code == 10 }

// effective is E for an entry of cert: the earlier of the invalidity date and the revocation time;
// for a compromise reason with no invalidity date, the certificate's notBefore.
func effective(cert *x509.Certificate, e crlEntry) time.Time {
	t := e.revoked
	if !e.invalid.IsZero() && e.invalid.Before(t) {
		t = e.invalid
	}
	if isCompromise(e.code) && e.invalid.IsZero() {
		t = cert.NotBefore
	}
	return t
}

func evalCert(cert, issuer *x509.Certificate, a time.Time, all []*crlView, evictedNew time.Time, p StatusParams) CertStatus {
	st := CertStatus{Serial: cert.SerialNumber, Tier: TierNoInformation}
	key := cert.SerialNumber.String()

	// Number order: one sequence across bases and deltas. A CRL dated later than another with a
	// higher number is not a legitimate successor.
	var ordered []*crlView
	maxNum := big.NewInt(-1)
	for _, v := range all {
		if v.number.Cmp(maxNum) < 0 {
			continue // a rollback: kept for its listings only
		}
		if v.number.Cmp(maxNum) == 0 && len(ordered) > 0 && ordered[len(ordered)-1].hash != v.hash {
			continue // two different CRLs, one number
		}
		maxNum = v.number
		ordered = append(ordered, v)
	}
	// How consecutive CRLs of the series are shown to have nothing between them: issued within the
	// predecessor's nextUpdate, and within the declared issuance interval of it (or with the next
	// CRL number). nextUpdate alone is not enough: a CA may issue earlier than it.
	interval := p.CAPeriod
	if iv, ok := p.CAPeriodFor[SPKIHashOf(issuer)]; ok {
		interval = iv
	}
	contig := func(prev, v *crlView) bool {
		if v.this.After(prev.next.Add(p.CoverageTolerance)) {
			return false
		}
		if p.ConsecutiveNumbers && v.number.Cmp(new(big.Int).Add(prev.number, big.NewInt(1))) == 0 {
			return true
		}
		return interval > 0 && v.this.Sub(prev.this) <= interval+p.CoverageTolerance
	}
	skew := p.ThisUpdateSkew
	if sk, ok := p.ThisUpdateSkewFor[SPKIHashOf(issuer)]; ok {
		skew = sk
	}
	if skew < 0 || skew > maxThisUpdateSkew {
		st.Note = fmt.Sprintf("the declared ThisUpdateSkew %s is outside 0 to %s", skew, maxThisUpdateSkew)
		return st
	}
	retains := func(v *crlView) bool {
		return p.Retained || (!v.expiredFrom.IsZero() && !cert.NotAfter.Before(v.expiredFrom))
	}

	// A listing in any CRL of the series is a revocation from its time on, whatever the CRL's date;
	// a later CRL that no longer lists the serial does not undo it. Every CRL that verifies under the
	// issuer is read for its listings, a delta that has no base included: a listing can only take trust
	// away. Holds are the exception to "from its time on", and are off by default (AllowHolds).
	var holdStarts, releases []time.Time
	var holdBy [32]byte
	for _, v := range all {
		for _, e := range v.entries[key] {
			if e.code == 6 || e.code == 8 {
				if !p.AllowHolds {
					st.Revoked, st.E, st.ReasonCode, st.RevokedBy = true, cert.NotBefore, e.code, v.hash
					st.Note = "the certificate has been on hold (certificateHold or removeFromCRL), which this policy does not accept (StatusParams.AllowHolds)"
					st.PolicyRefusal = true
					return st
				}
				if e.code == 6 {
					holdStarts = append(holdStarts, effective(cert, e))
				} else {
					releases = append(releases, e.revoked)
				}
				holdBy = v.hash
				continue
			}
			ev := effective(cert, e)
			if ev.After(a) || (isRoutine(e.code) && p.RoutineGrace > 0 && a.Before(ev.Add(p.RoutineGrace))) {
				continue
			}
			if !st.Revoked || ev.Before(st.E) {
				st.Revoked, st.E, st.ReasonCode, st.Compromise, st.RevokedBy = true, ev, e.code, isCompromise(e.code), v.hash
			}
		}
	}
	// Opted in: a decision is refused inside a hold, [start, release]. A listing bounds the start; the
	// release is the earliest removeFromCRL date at or after it, else the hold is open. A release with
	// no listing before it is a hold of unknown start: refused from notBefore to its date.
	if p.AllowHolds && !st.Revoked {
		sort.Slice(holdStarts, func(i, j int) bool { return holdStarts[i].Before(holdStarts[j]) })
		sort.Slice(releases, func(i, j int) bool { return releases[i].Before(releases[j]) })
		// The same hold is listed by several CRLs (the base, then every delta since): one hold.
		sameTime := func(a, b time.Time) bool { return a.Equal(b) }
		holdStarts = slices.CompactFunc(holdStarts, sameTime)
		releases = slices.CompactFunc(releases, sameTime)
		used := make([]bool, len(releases))
		inHold := func(start time.Time) {
			if !st.Revoked || start.Before(st.E) {
				st.Revoked, st.E, st.ReasonCode, st.RevokedBy = true, start, 6, holdBy
				st.Note = "the decision falls inside a certificateHold"
			}
		}
		// A CA that issues full CRLs only shows a release as the hold's disappearance: the first full CRL
		// after the last one that lists the hold bounds the release, but only if the series is
		// contiguous (the declared CA period) from that listing to it; across a gap the hold stays open.
		lists := func(v *crlView, hs time.Time) bool {
			for _, e := range v.entries[key] {
				if e.code == 6 && effective(cert, e).Equal(hs) {
					return true
				}
			}
			return false
		}
		releasedByAbsence := func(hs time.Time) time.Time {
			last := -1
			for i, v := range ordered {
				if lists(v, hs) {
					last = i
				}
			}
			if last < 0 {
				return time.Time{}
			}
			nxt := -1
			for i := last + 1; i < len(ordered); i++ {
				if !ordered[i].delta {
					nxt = i
					break
				}
			}
			if nxt < 0 {
				return time.Time{}
			}
			for i := last + 1; i <= nxt; i++ {
				if !contig(ordered[i-1], ordered[i]) {
					return time.Time{}
				}
			}
			return ordered[nxt].this.Add(skew + p.CoverageTolerance)
		}
		for _, hs := range holdStarts {
			end := time.Time{}
			for i, r := range releases {
				if !used[i] && !r.Before(hs) {
					end, used[i] = r, true
					break
				}
			}
			if end.IsZero() {
				end = releasedByAbsence(hs)
			}
			if !a.Before(hs) && (end.IsZero() || !a.After(end)) {
				inHold(hs)
			}
		}
		for i, r := range releases {
			if !used[i] && !a.After(r) {
				inHold(cert.NotBefore)
			}
		}
	}
	if st.Revoked {
		return st
	}

	// The evidence of absence is the full CRLs and the merged views: each delta with its base, as of
	// the delta. A delta with no base to merge with gives none.
	paired := pairDeltas(ordered)
	var views []*crlView
	for _, v := range ordered {
		if !v.delta {
			views = append(views, v)
			continue
		}
		if b := paired[v]; b != nil {
			// Retention comes from the base alone: the delta lists what changed since it, and a
			// retaining delta says nothing about the entries a pruning base already dropped.
			m := &crlView{hash: v.hash, number: v.number, this: v.this, next: v.next, expiredFrom: b.expiredFrom, parts: [][32]byte{b.hash, v.hash}}
			if v.next.After(b.next) {
				m.next = b.next // the earlier nextUpdate governs: a stale base does not make a fresh delta fresh
			}
			views = append(views, m)
		}
	}

	// L(C): the first CRL beyond notAfter, established only if the series shows nothing was issued
	// between it and its predecessor. Otherwise it may be a later CRL from which the entry was already
	// pruned (RFC 5280 section 3.3 lets a CA drop it once it has appeared on one regularly scheduled
	// CRL beyond notAfter).
	lcIdx := -1
	if i := slices.IndexFunc(views, func(v *crlView) bool { return v.this.After(cert.NotAfter) }); i > 0 && contig(views[i-1], views[i]) {
		lcIdx = i
		st.LC, st.LCKnown = views[i].this, true
	}

	// Usable as evidence of absence: issued at or after A (live counting also takes a fresh CRL
	// issued before A; its verdict cannot be final, because a revocation effective before A may be
	// reported after it), and, beyond notAfter, only if that CRL itself keeps expired entries or is L(C).
	live := !p.Now.IsZero()
	if p.CountLive && !live {
		st.Note = "CountLive needs Now: a live count is judged against the clock"
		return st
	}
	var usable []*crlView
	for _, v := range views {
		if v.this.Before(a) && !p.CountLive {
			continue
		}
		if v.this.After(cert.NotAfter) && !retains(v) && !(lcIdx >= 0 && v == views[lcIdx]) {
			continue
		}
		usable = append(usable, v)
	}
	fresh := usable[:0:0]
	for _, v := range usable {
		if live && (p.MaxStatusAge <= 0 || !p.Now.Before(v.next) || p.Now.Sub(v.this) > p.MaxStatusAge+skew || v.this.After(p.Now.Add(p.CoverageTolerance))) {
			continue
		}
		fresh = append(fresh, v)
	}
	if len(fresh) == 0 {
		st.Stale = len(usable) > 0
		switch {
		case len(usable) > 0:
			st.Note = "the CRLs after the decision are past nextUpdate, older than MaxStatusAge or dated in the future"
		default:
			st.Note = "no CRL issued after the decision verifies under the certificate's issuer"
		}
		return st
	}
	usable = fresh
	// S is the latest usable CRL that time coverage reaches from A. A later CRL beyond a gap is no
	// evidence of continuity, but it does not undo the status as of S: its listings already count.
	// When no usable CRL is reached, S is the latest one and the verdict says why it is not final.
	reached := -1
	for i := len(usable) - 1; i >= 0; i-- {
		if coveredFrom(views, a, usable[i].this, contig) {
			reached = i
			break
		}
	}
	covered := reached >= 0
	if !covered {
		reached = len(usable) - 1
	}
	usable = usable[:reached+1]
	for _, v := range usable {
		st.Used = append(st.Used, v.hashes()...)
	}
	sView := usable[len(usable)-1]
	st.S = sView.this
	st.Unbounded = retains(sView)

	w := p.W
	if st.LCKnown && !st.Unbounded {
		w = min(w, max(st.LC.Sub(a), 0))
	}
	st.Window = w

	st.Tier = TierProvisional
	st.Caveat = statusCaveat
	st.Basis = "status issued at or after the decision"
	if st.S.Before(a) {
		st.Basis = "live count: status issued before the decision (provisional only)"
	}
	if w := observedGapWarning(views, a, st.S, interval, p.CoverageTolerance); w != "" {
		st.Warning = w
	}
	// Did the snapshot drop CRLs from the range the verdict needs, from the CRL in force at A on?
	start := -1
	for i, v := range views {
		if !v.this.After(a) {
			start = i
		}
	}
	if !evictedNew.IsZero() && (start < 0 || !evictedNew.Before(views[start].this)) {
		st.EvictionGap = true
		st.Warning = joinWarning(st.Warning, "the snapshot evicted CRLs from the range this verdict needs")
	}
	switch {
	case st.EvictionGap:
		st.Note = "the snapshot evicted CRLs from the range this verdict needs"
	case st.S.Before(a.Add(w)):
		st.Note = "status ends before A + W'; a later CRL could still change the answer"
	case w < p.MinW:
		st.Note = "short window: the certificate's life leaves less than MinW after the decision"
	case !covered && interval <= 0 && !p.ConsecutiveNumbers:
		st.Note = "no CA issuance interval is declared, so time coverage cannot be shown"
	case !covered:
		st.Note = "the CRLs from the one in force at A to S leave a gap in time coverage"
	case !st.Unbounded && lcIdx < 0 && finalityNeedsLC:
		st.Note = "no CRL beyond notAfter is established yet, so L(C) is unknown"
	default:
		st.Tier = TierFinal
	}
	return st
}

// coveredFrom reports whether the CRLs form a chain of coverage from the one in force at a to the
// one issued at to: the latest CRL issued at or before a must reach a (nextUpdate), and each later
// CRL up to the one at to must follow its predecessor by contig (nextUpdate and the declared
// issuance interval). A gap means a revocation, or a certificateHold listed and released, could have
// gone unseen.
func coveredFrom(views []*crlView, a, to time.Time, contig func(prev, v *crlView) bool) bool {
	start := -1
	for i, v := range views {
		if !v.this.After(a) {
			start = i
		}
	}
	if start < 0 || views[start].next.Before(a) {
		return false
	}
	prev := views[start]
	for _, v := range views[start+1:] {
		if prev.this.Equal(to) || prev.this.After(to) {
			break
		}
		if !contig(prev, v) {
			return false
		}
		prev = v
	}
	return prev.this.Equal(to)
}

// observedGapWarning reports the longest gap between consecutive CRLs of the series from the one in
// force at a to the one at to, when it exceeds the declared period (plus tolerance).
func observedGapWarning(views []*crlView, a, to time.Time, period, tol time.Duration) string {
	if period <= 0 {
		return ""
	}
	start := -1
	for i, v := range views {
		if !v.this.After(a) {
			start = i
		}
	}
	if start < 0 {
		start = 0
	}
	var worst time.Duration
	for i := start + 1; i < len(views) && !views[i-1].this.After(to); i++ {
		if g := views[i].this.Sub(views[i-1].this); g > worst {
			worst = g
		}
	}
	if worst > period+tol {
		return fmt.Sprintf("the series has a gap of %s between CRLs, longer than the declared CA period of %s", worst, period)
	}
	return ""
}

func joinWarning(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// pairDeltas matches each delta CRL in vs with the base it is merged with (RFC 5280 sections 5.2.4
// and 6.3.3). A base B qualifies for a delta D when D.baseNumber <= B.number < D.number and D was
// issued no earlier than B. Of the qualifying bases the highest numbered is used, the freshest one
// that is still a valid partner (the delta is cumulative since the base it names, so a newer base
// carries everything). A delta with no qualifying base is absent from the result: it gives no
// evidence of absence, never a partial answer (its listings are read all the same). All the CRLs in
// vs are already known to verify under one issuer, and the snapshot bounds how many there are.
func pairDeltas(vs []*crlView) map[*crlView]*crlView {
	var bases []*crlView
	for _, v := range vs {
		if !v.delta {
			bases = append(bases, v)
		}
	}
	// vs is the series after the number-order rule, so the bases are already in increasing number.
	out := map[*crlView]*crlView{}
	for _, d := range vs {
		if !d.delta {
			continue
		}
		// The first base numbered at or above the delta is too high; walk down from the one before it.
		i := sort.Search(len(bases), func(i int) bool { return bases[i].number.Cmp(d.number) >= 0 })
		for i--; i >= 0 && bases[i].number.Cmp(d.baseNumber) >= 0; i-- {
			if !d.this.Before(bases[i].this) { // implied by the number order; kept as the RFC states it
				out[d] = bases[i]
				break
			}
		}
	}
	return out
}
