package audit

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
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
	maxCRLBytes   = 64 << 20 // a larger CRL is refused before parsing
	maxCRLEntries = 4_000_000

	defaultW    = 24 * time.Hour   // compromise-reporting window
	defaultMinW = 30 * time.Minute // floor on the window for a final verdict
)

// finalityNeedsLC chooses how a certificate that is still unexpired at the latest usable status is
// judged. false (the recommended reading, pending the user's decision on the design note's literal
// text): no CA can have pruned an entry of an unexpired certificate, so L(C) is not needed and the
// status is final once it reaches A+W'. true: the note's literal rule, under which a chain through an
// unexpired CA certificate is provisional until that CA certificate has expired and a CRL beyond
// it exists.
const finalityNeedsLC = false

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
	mu    sync.Mutex
	views []*crlView
	by    map[[32]byte]*crlView
	sigs  map[sigKey]bool // memo: does this CRL verify under this issuer certificate
}

// NewStatusSnapshot returns an empty snapshot.
func NewStatusSnapshot() *StatusSnapshot {
	return &StatusSnapshot{by: map[[32]byte]*crlView{}, sigs: map[sigKey]bool{}}
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
// not process, removeFromCRL in a full CRL, a malformed invalidity date, and a delta CRL (merging
// them is a separate step). The signature is not checked here: that needs the issuing certificate,
// which belongs to a chain (see CertIdentity.EvaluateStatus).
func (s *StatusSnapshot) AddCRL(der []byte) (CRLInfo, error) {
	if len(der) == 0 || len(der) > maxCRLBytes {
		return CRLInfo{}, statusErr(agent.ReasonStatusUnknown, "the CRL is %d bytes, outside 1 to %d", len(der), maxCRLBytes)
	}
	hash := sha256.Sum256(der)
	s.mu.Lock()
	if v, ok := s.by[hash]; ok {
		s.mu.Unlock()
		return v.info(), nil
	}
	s.mu.Unlock()

	v, err := parseCRL(der, hash)
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
	return v.info(), nil
}

func (v *crlView) info() CRLInfo {
	n := 0
	for _, es := range v.entries {
		n += len(es)
	}
	return CRLInfo{Hash: v.hash, Number: new(big.Int).Set(v.number), ThisUpdate: v.this, NextUpdate: v.next, Entries: n}
}

func parseCRL(der []byte, hash [32]byte) (*crlView, error) {
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
	for _, e := range rl.Extensions {
		switch {
		case e.Id.Equal(oidIssuingDistPoint):
			return unreadable("the CRL carries an issuing distribution point (a partitioned CRL cannot be matched correctly)")
		case e.Id.Equal(oidDeltaIndicator):
			return unreadable("the CRL is a delta CRL, which is merged with its base and never read alone")
		case e.Id.Equal(oidExpiredCertsOnCRL):
			var t time.Time
			if rest, err := asn1.Unmarshal(e.Value, &t); err != nil || len(rest) != 0 {
				return unreadable("the expiredCertsOnCRL extension is malformed")
			}
			v.expiredFrom = t
		case e.Id.Equal(oidCRLNumber), e.Id.Equal(oidAuthorityKeyID), e.Id.Equal(oidIssuerAltName), e.Id.Equal(oidFreshestCRL):
		case e.Critical:
			return unreadable("the CRL has a critical extension %s that bide does not process", e.Id)
		}
	}
	if len(rl.RevokedCertificateEntries) > maxCRLEntries {
		return unreadable("the CRL has %d entries, over the limit of %d", len(rl.RevokedCertificateEntries), maxCRLEntries)
	}
	for i, re := range rl.RevokedCertificateEntries {
		if re.SerialNumber == nil || re.RevocationTime.IsZero() {
			return unreadable("entry %d has no serial number or revocation time", i)
		}
		ce := crlEntry{revoked: re.RevocationTime, code: re.ReasonCode}
		switch {
		case re.ReasonCode == 7 || re.ReasonCode < 0 || re.ReasonCode > 10:
			return unreadable("entry %d has the undefined reason code %d", i, re.ReasonCode)
		case re.ReasonCode == 8:
			return unreadable("entry %d is removeFromCRL in a full CRL", i)
		}
		for _, x := range re.Extensions {
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
	// Now, when non-zero, makes the evaluation live: a CRL is evidence of absence only while
	// Now < nextUpdate and Now - thisUpdate <= MaxStatusAge. A live evaluation without a positive
	// MaxStatusAge accepts no evidence (a configuration mistake fails closed). Leave Now zero for an
	// offline audit, which judges the series against the decision, not the clock.
	Now          time.Time
	MaxStatusAge time.Duration
	// Distrust adds distrust dates to the policy's own, by issuer key: status signed by that key with
	// thisUpdate at or after the date is no information. It is the auditor's overlay.
	Distrust map[SPKIHash]time.Time
	// Retained says the bank's CA keeps the entries of expired certificates for as long as audits
	// need them (recorded retention in the policy), as expiredCertsOnCRL does per CRL.
	Retained bool
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
}

// ChainStatus is the finding for every non-anchor certificate of an accepted chain, leaf first.
type ChainStatus struct {
	Certs   []CertStatus
	Revoked bool       // any certificate revoked with E <= A(d)
	Tier    StatusTier // the weakest certificate's tier (meaningful when not Revoked)
}

// Err is nil when the chain has usable status and no revocation. Otherwise it is a *StatusError:
// ReasonRevoked, ReasonStatusStale (usable-in-time status exists but is too old) or
// ReasonStatusUnknown.
func (cs ChainStatus) Err() error {
	for i, c := range cs.Certs {
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
	out := ChainStatus{Tier: TierFinal}
	for i := 0; i+1 < len(ac.Chain); i++ {
		cert, issuer := ac.Chain[i], ac.Chain[i+1]
		views := s.viewsFor(issuer, c.distrustFor(issuer, p.Distrust))
		st := evalCert(cert, issuer, a, views, p)
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
func (s *StatusSnapshot) viewsFor(issuer *x509.Certificate, distrust time.Time) []*crlView {
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
	return out
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

func evalCert(cert, issuer *x509.Certificate, a time.Time, all []*crlView, p StatusParams) CertStatus {
	st := CertStatus{Serial: cert.SerialNumber, Tier: TierNoInformation}
	key := cert.SerialNumber.String()

	// A listing in any CRL of the series is a revocation from its time on, whatever the CRL's date;
	// a later CRL that no longer lists the serial does not undo it.
	for _, v := range all {
		for _, e := range v.entries[key] {
			ev := effective(cert, e)
			if ev.After(a) || (isRoutine(e.code) && p.RoutineGrace > 0 && a.Before(ev.Add(p.RoutineGrace))) {
				continue
			}
			if !st.Revoked || ev.Before(st.E) {
				st.Revoked, st.E, st.ReasonCode, st.Compromise, st.RevokedBy = true, ev, e.code, isCompromise(e.code), v.hash
			}
		}
	}
	if st.Revoked {
		return st
	}

	// Number order: a CRL dated later than another with a higher number is not a legitimate successor.
	var views []*crlView
	maxNum := big.NewInt(-1)
	for _, v := range all {
		if v.number.Cmp(maxNum) < 0 {
			continue // a rollback: kept for its listings only
		}
		if v.number.Cmp(maxNum) == 0 && len(views) > 0 && views[len(views)-1].hash != v.hash {
			continue // two different CRLs, one number
		}
		maxNum = v.number
		views = append(views, v)
	}

	// Retention and L(C).
	for _, v := range views {
		if !v.expiredFrom.IsZero() && !cert.NotAfter.Before(v.expiredFrom) {
			st.Unbounded = true
		}
	}
	st.Unbounded = st.Unbounded || p.Retained
	lcIdx := -1
	if !st.Unbounded {
		i := slices.IndexFunc(views, func(v *crlView) bool { return v.this.After(cert.NotAfter) })
		// The first CRL beyond notAfter is L(C) only if the CRL before it promised it in time: the
		// predecessor's nextUpdate reaches it. Otherwise a CRL may be missing between them, and this
		// one may be a later CRL from which the entry was already pruned.
		if i > 0 && !views[i-1].next.Before(views[i].this) {
			lcIdx = i
			st.LC, st.LCKnown = views[i].this, true
		}
	}

	// Usable as evidence of absence: issued at or after A, and no later than L(C).
	var usable []*crlView
	for _, v := range views {
		if v.this.Before(a) {
			continue
		}
		if !st.Unbounded && v.this.After(cert.NotAfter) && !(lcIdx >= 0 && v.this.Equal(st.LC)) {
			continue
		}
		usable = append(usable, v)
	}
	fresh := usable[:0:0]
	for _, v := range usable {
		if !p.Now.IsZero() && (p.MaxStatusAge <= 0 || !p.Now.Before(v.next) || p.Now.Sub(v.this) > p.MaxStatusAge) {
			continue
		}
		fresh = append(fresh, v)
	}
	if len(fresh) == 0 {
		st.Stale = len(usable) > 0
		switch {
		case len(usable) > 0:
			st.Note = "the CRLs after the decision are past nextUpdate or older than MaxStatusAge"
		default:
			st.Note = "no CRL issued after the decision verifies under the certificate's issuer"
		}
		return st
	}
	usable = fresh
	for _, v := range usable {
		st.Used = append(st.Used, v.hash)
	}
	st.S = usable[len(usable)-1].this

	w := p.W
	if st.LCKnown && !st.Unbounded {
		w = min(w, max(st.LC.Sub(a), 0))
	}
	st.Window = w

	st.Tier = TierProvisional
	switch {
	case st.S.Before(a.Add(w)):
		st.Note = "status ends before A + W'; a later CRL could still change the answer"
	case w < p.MinW:
		st.Note = "short window: the certificate's life leaves less than MinW after the decision"
	case !st.Unbounded && lcIdx >= 0 && !coveredTo(views, st.S, st.LC):
		st.Note = "the CRLs from S to L(C) leave a gap in time coverage"
	case !st.Unbounded && lcIdx < 0 && finalityNeedsLC:
		st.Note = "no CRL beyond notAfter is established yet, so L(C) is unknown"
	default:
		st.Tier = TierFinal
	}
	return st
}

// coveredTo reports whether the CRLs with thisUpdate in [from, to] form a chain in which each one
// was issued no later than its predecessor's nextUpdate, starting at from and ending at to.
func coveredTo(views []*crlView, from, to time.Time) bool {
	var prev *crlView
	for _, v := range views {
		if v.this.Before(from) || v.this.After(to) {
			continue
		}
		if prev == nil {
			if !v.this.Equal(from) {
				return false
			}
		} else if v.this.After(prev.next) {
			return false
		}
		prev = v
	}
	return prev != nil && prev.this.Equal(to)
}
