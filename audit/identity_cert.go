package audit

import (
	"cmp"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Certificate-bound approvers (docs/design/approver-identity.md). This file is the certificate half
// of the counting rule: given a decision's leaf-first chain and the two times that matter, decide
// whether the chain is a chain this gate accepts for this approver. It does not look at revocation
// status, the signature or the journal; those are separate steps and are not part of this check.

// SPKIHash is the SHA-256 of a certificate's SubjectPublicKeyInfo. Intermediates are pinned and
// distrust is dated by it: a key, not a certificate, so a reissued CA certificate for the same key is
// the same CA.
type SPKIHash [32]byte

// SPKIHashOf returns the pin of c's public key.
func SPKIHashOf(c *x509.Certificate) SPKIHash { return sha256.Sum256(c.RawSubjectPublicKeyInfo) }

// IdentityPolicy is the certificate rules of one gate. It is configuration, change-controlled by the
// bank: whoever can edit it can enroll. Compile checks it and fixes it into a CertIdentity.
//
// The zero value of Skew, AnchorGrace and MaxChain selects the default.
type IdentityPolicy struct {
	// Anchors are the CA certificates this gate trusts: not the system roots. Each must be a CA with
	// an allowed key (see the algorithm profile in the design note).
	Anchors []*x509.Certificate
	// Inter pins the intermediates a chain may pass through, by key. A certificate carried in a
	// decision that is not pinned is ignored. A policy with no pins accepts leaves issued directly by
	// an anchor.
	Inter []SPKIHash
	// Distrust ends trust in an anchor or pinned intermediate (by key) for decisions provable at or
	// after the given time, without voiding earlier history. It is how a compromised CA is retired.
	Distrust map[SPKIHash]time.Time
	// Identity maps EVERY approver id of the gate, bare-key approvers included, to the identifier of
	// that approver's person. Two ids that canonicalize to one identifier are refused: one person is
	// one seat.
	Identity map[string]string
	// IDKind is the kind of the one subject alternative name a leaf must carry.
	IDKind IDKind
	// EKU is the extended key usage a leaf must carry. It is checked by hand and also along the
	// chain: every CA certificate that carries extended key usages must permit it.
	EKU asn1.ObjectIdentifier
	// AllowEmailProtectionEKU accepts id-kp-emailProtection in place of EKU. Set it only when the
	// anchors are a dedicated issuing CA for approvers: every S/MIME certificate carries that usage,
	// often with different key handling.
	AllowEmailProtectionEKU bool
	// Policy is the certificate policy OID the leaf must assert. The bank's CA asserts it only after
	// single sign-on with multi-factor step-up and a key generated or held in hardware; bide cannot
	// see either, it can require that the CA says so.
	Policy x509.OID
	// Canonical maps identifiers to their one canonical form. Nil selects NewCanonicalizer with no
	// configuration, which suits IDEmail only: an IDPersonURI policy must supply one (it needs the
	// host).
	Canonical Canonicalizer
	// MaxChain bounds the path from the leaf to the anchor, and the number of certificates a decision
	// may carry. Zero means 4; at most 8.
	MaxChain int
	// Skew is how far a decision's signed time may lie after the time it became provable
	// (decided_at <= A + Skew). Zero means 2 minutes.
	Skew time.Duration
	// AnchorGrace is how long after the leaf's notAfter a decision may first become provable
	// (A <= notAfter + AnchorGrace). Zero means 60 seconds.
	AnchorGrace time.Duration
	// RSA2048Until permits RSA-2048 CA keys while the attested time (here A(d), the time the decision
	// became provable) is strictly before it; many bank roots are still RSA-2048. Zero refuses them,
	// and anything under 2048 bits is always refused: the default is RSA-3072 or better. The field has
	// the same meaning and name in the RFC 3161 verifier's policy.
	RSA2048Until           time.Time
	MaxLeafLifetime        time.Duration
	RequireNameConstraints bool
	// RequirePolicyOnCAs refuses a path in which an issuing CA carries no certificatePolicies
	// extension (an anchor that does not issue the leaf directly is not required to). Without it a
	// CA with no extension is unconstrained, so the bank's policy OID is only the leaf's assertion.
	// Recommended: put certificatePolicies on the issuing CA, naming the approver policy, and set
	// this. It is off by default only so that a policy can be adopted before the CA is reissued.
	RequirePolicyOnCAs bool
}

const (
	defaultMaxChain    = 4
	maxMaxChain        = 8
	defaultSkew        = 2 * time.Minute
	defaultAnchorGrace = 60 * time.Second
	maxTolerance       = 10 * time.Minute // ceiling on Skew and AnchorGrace
	defaultLeafLife    = 24 * time.Hour
	maxLeafLife        = 31 * 24 * time.Hour
	maxCertDER         = 16 << 10 // a larger certificate is refused before parsing
	maxSerialBits      = 159      // RFC 5280: at most 20 octets, positive
	maxDetail          = 160
)

// CertIdentity is a compiled IdentityPolicy: immutable, safe for concurrent use.
type CertIdentity struct {
	anchors  *x509.CertPool
	anchorBy map[SPKIHash]*x509.Certificate
	inter    map[SPKIHash]bool
	distrust map[SPKIHash]time.Time
	identity map[string]string // approver id -> canonical identifier
	kind     IDKind
	eku      asn1.ObjectIdentifier
	emailEKU bool
	policy   x509.OID
	canon    Canonicalizer
	maxChain int
	skew     time.Duration
	grace    time.Duration
	rsa2048  time.Time
	maxLife  time.Duration
	needNC   bool
	needPol  bool
}

// Compile validates the policy and fixes a copy of it. Every failure is an error wrapping
// agent.ErrConfig: a gate with a bad certificate policy must not start, not fall back to bare keys.
func (p IdentityPolicy) Compile() (*CertIdentity, error) {
	cfgErr := func(format string, args ...any) (*CertIdentity, error) {
		return nil, fmt.Errorf("audit: identity policy: "+format+": %w", append(args, agent.ErrConfig)...)
	}
	c := &CertIdentity{
		anchors: x509.NewCertPool(), anchorBy: map[SPKIHash]*x509.Certificate{},
		inter: map[SPKIHash]bool{}, distrust: map[SPKIHash]time.Time{}, identity: map[string]string{},
		kind: p.IDKind, eku: slices.Clone(p.EKU), emailEKU: p.AllowEmailProtectionEKU, policy: p.Policy,
		canon: p.Canonical, maxChain: p.MaxChain, skew: p.Skew, grace: p.AnchorGrace, rsa2048: p.RSA2048Until,
		maxLife: p.MaxLeafLifetime, needNC: p.RequireNameConstraints, needPol: p.RequirePolicyOnCAs,
	}
	if !p.IDKind.valid() {
		return cfgErr("identifier kind %q is not %q or %q", p.IDKind, IDPersonURI, IDEmail)
	}
	if len(p.EKU) == 0 {
		return cfgErr("no required extended key usage")
	}
	if p.Policy.Equal(x509.OID{}) {
		return cfgErr("no required certificate policy OID")
	}
	switch {
	case c.maxChain == 0:
		c.maxChain = defaultMaxChain
	case c.maxChain < 0 || c.maxChain > maxMaxChain:
		return cfgErr("MaxChain %d is outside 1 to %d", c.maxChain, maxMaxChain)
	}
	if c.skew < 0 || c.grace < 0 {
		return cfgErr("Skew and AnchorGrace must not be negative")
	}
	if c.skew == 0 {
		c.skew = defaultSkew
	}
	if c.grace == 0 {
		c.grace = defaultAnchorGrace
	}
	if c.skew > maxTolerance || c.grace > maxTolerance {
		return cfgErr("Skew and AnchorGrace are at most %s: a larger tolerance widens the proof window beyond what the design bounds", maxTolerance)
	}
	switch {
	case c.maxLife < 0 || c.maxLife > maxLeafLife:
		return cfgErr("MaxLeafLifetime %s is outside 0 to %s", c.maxLife, maxLeafLife)
	case c.maxLife == 0:
		c.maxLife = defaultLeafLife
	}
	if c.canon == nil {
		if p.IDKind != IDEmail {
			return cfgErr("an %q policy needs a Canonicalizer (it holds the URI host)", p.IDKind)
		}
		c.canon = NewCanonicalizer(CanonicalConfig{})
	}
	if len(p.Anchors) == 0 {
		return cfgErr("no trust anchors")
	}
	for i, a := range p.Anchors {
		if a == nil || !a.IsCA || !a.BasicConstraintsValid {
			return cfgErr("trust anchor %d is not a CA certificate", i)
		}
		if err := caKeyAllowed(a.PublicKey, time.Time{}, c.rsa2048); err != nil {
			return cfgErr("trust anchor %d: %v", i, err)
		}
		c.anchors.AddCert(a)
		c.anchorBy[SPKIHashOf(a)] = a
	}
	for _, h := range p.Inter {
		c.inter[h] = true
	}
	for h, t := range p.Distrust {
		// A mistyped hash would leave a compromised CA trusted while the bank believes it distrusted.
		if _, anchor := c.anchorBy[h]; !anchor && !c.inter[h] {
			return cfgErr("Distrust names the key %x, which is neither a trust anchor nor a pinned intermediate", h[:8])
		}
		c.distrust[h] = t
	}
	if len(p.Identity) == 0 {
		return cfgErr("the identity map is empty: it must name every approver of the gate")
	}
	owner := map[string]string{}
	for id, ident := range p.Identity {
		if id == "" {
			return cfgErr("the identity map has an empty approver id")
		}
		canon, err := c.canon.Canonical(p.IDKind, ident)
		if err != nil {
			return cfgErr("identity of approver %q: %v", id, err)
		}
		if other, dup := owner[canon]; dup {
			a, b := id, other
			if a > b {
				a, b = b, a
			}
			return cfgErr("approvers %q and %q have one identity, so one person would fill two seats", a, b)
		}
		owner[canon] = id
		c.identity[id] = canon
	}
	return c, nil
}

// Canonicalizer returns the function this policy compares identifiers with, for the identity record
// and for the other places an identity is compared (the requester principal).
func (c *CertIdentity) Canonicalizer() Canonicalizer { return c.canon }

// Identity returns the canonical identifier the policy expects for an approver id.
func (c *CertIdentity) Identity(approverID string) (string, bool) {
	v, ok := c.identity[approverID]
	return v, ok
}

// ApproverCert is the result of a successful VerifyChain: what the later steps (the signature, the
// status, the seat) work from.
type ApproverCert struct {
	Leaf  *x509.Certificate
	Chain []*x509.Certificate // the accepted path, leaf first, ending at the anchor
	// Anchor is the key of the trust anchor the chain ended at.
	Anchor SPKIHash
	// Scheme is the decision scheme derived from the leaf's key. A record whose approver_alg differs
	// from it does not count.
	Scheme Alg
	// PublicKey is the leaf key in the encoding NewVerifier reads for Scheme.
	PublicKey []byte
	// Identity is the verified canonical identifier of the leaf.
	Identity string
	// LeafHash is the SHA-256 of the leaf's DER, which the v4 decision bytes commit to.
	LeafHash [32]byte
	// KeyHash is the SHA-256 of the leaf's SubjectPublicKeyInfo: two counted decisions by different
	// approver ids with equal KeyHash are one key and do not both count.
	KeyHash SPKIHash
}

// SeatKeyID is the agent.ApproverVerifier.KeyIDs entry of a certificate-bound approver: the seat is
// the canonical identity, so a person with two approver ids, or two certificates, fills one seat.
func (a *ApproverCert) SeatKeyID() string { return "ident:" + a.Identity }

// CertError is the failure of VerifyChain. Reason is one of the agent.ReasonNoCert,
// agent.ReasonCertChain, agent.ReasonCertIdentity, agent.ReasonCertUsage or agent.ReasonCertWindow
// categories; Detail says precisely what failed. Neither holds key material, and any text taken from
// the certificate is quoted and truncated.
type CertError struct {
	Reason string
	Detail string
}

// Error reports the category and the precise cause.
func (e *CertError) Error() string { return "approver certificate: " + e.Reason + ": " + e.Detail }

func certErr(reason, format string, args ...any) error {
	return &CertError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// quote returns s quoted and cut to a length that is safe to log.
func quote(s string) string {
	if len(s) > maxDetail {
		return strconv.Quote(s[:maxDetail]) + "..."
	}
	return strconv.Quote(s)
}

// VerifyChain checks a decision's certificate chain for approver approverID.
//
// decidedAt is the time the approver signed (their claim); anchoredAt is A(d), the time of the
// smallest signed head that covers the decision record. chain is the leaf-first DER chain the
// decision carries. The check is rules 1 to 3 of the counting rule in the design note:
//
//   - exactly one chain builds from the pinned intermediates to a trust anchor of this gate, at
//     min(A, leaf notAfter) for the leaf and A for every CA certificate;
//   - the extended key usage nests along the chain, the algorithms are the profile's, no anchor or
//     intermediate is past its DistrustAfter;
//   - the leaf is not a CA, may sign, carries the required extended key usage and policy, has a key
//     a decision scheme can verify, and has exactly one subject alternative name of the configured
//     kind whose canonical value is the approver's identity;
//   - notBefore <= decidedAt <= notAfter, decidedAt <= A + skew, and A <= notAfter + anchorGrace.
//
// It does not check revocation status or the signature (the decision verifier built from
// ApproverCert.PublicKey refuses weak Ed25519 keys when the signature is checked, so KeyHash is over
// bytes that step must still vet). Every carried certificate must parse, pinned or not: one
// unparseable certificate refuses the chain. The signature algorithm profile is SHA-256 or better,
// with ECDSA limited to SHA-256 and SHA-384 as the design note says (the RFC 3161 verifier also
// accepts SHA-512 on ECDSA). An error is always a *CertError.
func (c *CertIdentity) VerifyChain(approverID string, chain [][]byte, decidedAt, anchoredAt time.Time) (*ApproverCert, error) {
	want, ok := c.identity[approverID]
	if !ok {
		return nil, certErr(agent.ReasonCertIdentity, "the policy has no identity for approver %s", quote(approverID))
	}
	if len(chain) == 0 {
		return nil, certErr(agent.ReasonNoCert, "the decision carries no certificate")
	}
	if len(chain) > c.maxChain {
		return nil, certErr(agent.ReasonCertChain, "the decision carries %d certificates, over the limit of %d", len(chain), c.maxChain)
	}
	certs := make([]*x509.Certificate, len(chain))
	for i, der := range chain {
		if len(der) == 0 || len(der) > maxCertDER {
			return nil, certErr(agent.ReasonCertChain, "certificate %d is %d bytes, outside 1 to %d", i, len(der), maxCertDER)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, certErr(agent.ReasonCertChain, "certificate %d does not parse: %v", i, err)
		}
		certs[i] = cert
	}
	leaf := certs[0]

	// The chain: only pinned intermediates are used, and exactly one path must result.
	pool := x509.NewCertPool()
	for _, ic := range certs[1:] {
		if c.inter[SPKIHashOf(ic)] {
			pool.AddCert(ic)
		}
	}
	at := anchoredAt
	if leaf.NotAfter.Before(at) {
		at = leaf.NotAfter
	}
	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots: c.anchors, Intermediates: pool, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	if err != nil {
		var inv x509.CertificateInvalidError
		if errors.As(err, &inv) && inv.Reason == x509.Expired {
			return nil, certErr(agent.ReasonCertWindow, "a certificate of the chain is outside its validity at %s: %v", at.UTC().Format(time.RFC3339), err)
		}
		return nil, certErr(agent.ReasonCertChain, "the chain does not verify: %v", err)
	}
	// Chains that differ only in a certificate for the same key (a renewed root, a reissued
	// intermediate) are one chain; chains through different keys are ambiguous and refused.
	groups := map[string][][]*x509.Certificate{}
	for _, ch := range chains {
		var k []byte
		for _, cert := range ch {
			h := SPKIHashOf(cert)
			k = append(k, h[:]...)
		}
		groups[string(k)] = append(groups[string(k)], ch)
	}
	if len(groups) != 1 {
		return nil, certErr(agent.ReasonCertChain, "%d valid chains through different keys build from the carried certificates; exactly one is required", len(groups))
	}
	var cands [][]*x509.Certificate
	for _, g := range groups {
		cands = g
	}
	sort.Slice(cands, func(i, j int) bool { return chainDigest(cands[i]) < chainDigest(cands[j]) })

	// The leaf's role comes before the CAs' profile so a wrong-purpose certificate is named for that.
	scheme, pub, err := c.checkLeaf(leaf)
	if err != nil {
		return nil, err
	}
	var path []*x509.Certificate
	var pathErr error
	for _, cand := range cands {
		if len(cand) > c.maxChain {
			pathErr = cmp.Or(pathErr, certErr(agent.ReasonCertChain, "the path has %d certificates, over the limit of %d", len(cand), c.maxChain))
			continue
		}
		if err := c.checkPath(cand, anchoredAt); err != nil {
			pathErr = cmp.Or(pathErr, err)
			continue
		}
		path = cand
		break
	}
	if path == nil {
		return nil, pathErr
	}

	ident, err := c.leafIdentifier(leaf)
	if err != nil {
		return nil, err
	}
	if ident != want {
		return nil, certErr(agent.ReasonCertIdentity, "the certificate's identifier %s is not the identity of approver %s", quote(ident), quote(approverID))
	}

	// Rules 2 and 3: the signed time is plausible and the proof is not too late.
	switch {
	case decidedAt.Before(leaf.NotBefore) || decidedAt.After(leaf.NotAfter):
		return nil, certErr(agent.ReasonCertWindow, "decided_at %s is outside the certificate's validity %s to %s",
			decidedAt.UTC().Format(time.RFC3339Nano), leaf.NotBefore.UTC().Format(time.RFC3339), leaf.NotAfter.UTC().Format(time.RFC3339))
	case decidedAt.After(anchoredAt.Add(c.skew)):
		return nil, certErr(agent.ReasonCertWindow, "decided_at %s is more than %s after the time it became provable, %s",
			decidedAt.UTC().Format(time.RFC3339Nano), c.skew, anchoredAt.UTC().Format(time.RFC3339Nano))
	case anchoredAt.After(leaf.NotAfter.Add(c.grace)):
		return nil, certErr(agent.ReasonCertWindow, "the decision became provable at %s, more than %s after the certificate expired at %s",
			anchoredAt.UTC().Format(time.RFC3339Nano), c.grace, leaf.NotAfter.UTC().Format(time.RFC3339))
	}

	return &ApproverCert{
		Leaf: leaf, Chain: path, Anchor: SPKIHashOf(path[len(path)-1]), Scheme: scheme, PublicKey: pub,
		Identity: ident, LeafHash: sha256.Sum256(leaf.Raw), KeyHash: SPKIHashOf(leaf),
	}, nil
}

// checkLeaf is the leaf's role: not a CA, may sign, the extended key usage and policy, and a key one
// of the decision schemes can verify. It returns that scheme and the key in NewVerifier's encoding.
func (c *CertIdentity) checkLeaf(leaf *x509.Certificate) (Alg, []byte, error) {
	if leaf.IsCA {
		return "", nil, certErr(agent.ReasonCertUsage, "the leaf is marked as a CA")
	}
	// Key usage is exclusive to signing: an approver key is not also a certificate-signing,
	// encipherment or agreement key.
	const allowedKU = x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return "", nil, certErr(agent.ReasonCertUsage, "the leaf's key usage does not include digitalSignature")
	}
	if extra := leaf.KeyUsage &^ allowedKU; extra != 0 {
		return "", nil, certErr(agent.ReasonCertUsage, "the leaf's key usage has bits beyond digitalSignature and contentCommitment (0x%x)", int(extra))
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) > c.maxLife {
		return "", nil, certErr(agent.ReasonCertWindow, "the leaf is valid for %s, over the limit of %s", leaf.NotAfter.Sub(leaf.NotBefore), c.maxLife)
	}
	if _, err := c.leafEKUMode(leaf); err != nil {
		return "", nil, err
	}
	if !slices.ContainsFunc(leaf.Policies, c.policy.Equal) {
		return "", nil, certErr(agent.ReasonCertUsage, "the leaf does not assert the certificate policy %s", c.policy)
	}
	switch k := leaf.PublicKey.(type) {
	case ed25519.PublicKey:
		return AlgEd25519, slices.Clone(k), nil
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() {
			return "", nil, certErr(agent.ReasonCertUsage, "the leaf's ECDSA key is on %s; only P-256 has a decision scheme", k.Curve.Params().Name)
		}
		b, err := k.Bytes()
		if err != nil {
			return "", nil, certErr(agent.ReasonCertUsage, "the leaf's ECDSA key does not encode: %v", err)
		}
		return AlgECDSAP256, b, nil
	case *mldsa.PublicKey:
		if k.Parameters() != mldsa.MLDSA65() {
			return "", nil, certErr(agent.ReasonCertUsage, "the leaf's ML-DSA key is not ML-DSA-65")
		}
		return AlgMLDSA65, k.Bytes(), nil
	}
	return "", nil, certErr(agent.ReasonCertUsage, "the leaf's key is %T, which no decision scheme can verify (ed25519, ecdsa-p256 and ml-dsa-65 only)", leaf.PublicKey)
}

// leafEKU is how the leaf qualifies on extended key usage.
type leafEKU int

const (
	ekuApprover leafEKU = iota // exactly the required OID
	ekuEmail                   // exactly id-kp-emailProtection (dedicated approver CA mode)
)

// leafEKUMode requires the leaf's extended key usage to be exactly the required OID, or, when the
// policy allows it, exactly emailProtection: an approver key is not shared across purposes, and a
// leaf that mixes the two modes is refused.
func (c *CertIdentity) leafEKUMode(cert *x509.Certificate) (leafEKU, error) {
	switch {
	case len(cert.ExtKeyUsage) == 0 && len(cert.UnknownExtKeyUsage) == 1 && c.eku.Equal(cert.UnknownExtKeyUsage[0]):
		return ekuApprover, nil
	case c.emailEKU && len(cert.UnknownExtKeyUsage) == 0 && len(cert.ExtKeyUsage) == 1 && cert.ExtKeyUsage[0] == x509.ExtKeyUsageEmailProtection:
		return ekuEmail, nil
	case slices.ContainsFunc(cert.UnknownExtKeyUsage, c.eku.Equal) || (c.emailEKU && slices.Contains(cert.ExtKeyUsage, x509.ExtKeyUsageEmailProtection)):
		return 0, certErr(agent.ReasonCertUsage, "the leaf carries the extended key usage %s together with others; an approver certificate carries exactly one", c.eku)
	}
	return 0, certErr(agent.ReasonCertUsage, "the leaf does not carry the extended key usage %s", c.eku)
}

// ekuMatch is the usage the leaf is judged on, for nesting along the chain.
func (c *CertIdentity) caPermits(ca, leaf *x509.Certificate) bool {
	if len(ca.ExtKeyUsage) == 0 && len(ca.UnknownExtKeyUsage) == 0 {
		return true // no EKU on the CA: unconstrained
	}
	if slices.Contains(ca.ExtKeyUsage, x509.ExtKeyUsageAny) {
		return true
	}
	if mode, err := c.leafEKUMode(leaf); err == nil && mode == ekuEmail {
		return slices.Contains(ca.ExtKeyUsage, x509.ExtKeyUsageEmailProtection)
	}
	return slices.ContainsFunc(ca.UnknownExtKeyUsage, c.eku.Equal)
}

// checkPath applies what the standard library does not: EKU nesting, the algorithm profile, the
// serial number limit, each CA's validity at A (the library checked it at min(A, leaf notAfter)) and
// the distrust dates.
func (c *CertIdentity) checkPath(path []*x509.Certificate, a time.Time) error {
	leaf := path[0]
	inhibitAny := slices.ContainsFunc(path, func(x *x509.Certificate) bool { return x.InhibitAnyPolicy > 0 || x.InhibitAnyPolicyZero })
	for i, cert := range path {
		if cert.SerialNumber.Sign() <= 0 || cert.SerialNumber.BitLen() > maxSerialBits {
			return certErr(agent.ReasonCertChain, "certificate %d has a serial number outside 1 to 20 octets", i)
		}
		if i == len(path)-1 {
			// The anchor's own signature is not checked (it is trusted by pinning); its key is.
		} else if err := sigAlgAllowed(cert.SignatureAlgorithm); err != nil {
			return certErr(agent.ReasonCertChain, "certificate %d: %v", i, err)
		}
		if i == 0 {
			continue
		}
		if a.Before(cert.NotBefore) || a.After(cert.NotAfter) {
			return certErr(agent.ReasonCertWindow, "CA certificate %d is outside its validity %s to %s at %s", i,
				cert.NotBefore.UTC().Format(time.RFC3339), cert.NotAfter.UTC().Format(time.RFC3339), a.UTC().Format(time.RFC3339))
		}
		if err := caKeyAllowed(cert.PublicKey, a, c.rsa2048); err != nil {
			return certErr(agent.ReasonCertChain, "CA certificate %d: %v", i, err)
		}
		if t, ok := c.distrust[SPKIHashOf(cert)]; ok && !a.Before(t) {
			return certErr(agent.ReasonCertChain, "CA certificate %d is distrusted from %s, which is not after the time the decision became provable", i, t.UTC().Format(time.RFC3339))
		}
		if !c.caPermits(cert, leaf) {
			return certErr(agent.ReasonCertUsage, "CA certificate %d has extended key usages that do not permit %s", i, c.eku)
		}
		if err := c.caPermitsPolicy(cert, i, inhibitAny); err != nil {
			return err
		}
		if c.needPol && len(cert.Policies) == 0 && (i < len(path)-1 || len(path) == 2) {
			return certErr(agent.ReasonCertUsage, "CA certificate %d carries no certificatePolicies, which the policy requires of an issuing CA", i)
		}
	}
	if c.needNC && !slices.ContainsFunc(path[1:], c.constrainsKind) {
		return certErr(agent.ReasonCertChain, "no CA of the path carries a permitted-subtree name constraint for the %q identifier kind", c.kind)
	}
	return nil
}

// caPermitsPolicy nests certificate policies along the chain, as extended key usages are: a CA that
// states the policies it issues under and does not include the required one (or anyPolicy) cannot
// issue a leaf that passes for it. A CA with no certificatePolicies extension is unconstrained.
// Policy mappings are not processed (a mapped policy cannot be judged without the RFC 5280 policy
// tree), so a CA that maps policies is refused rather than guessed at (this rules out cross-certified
// hierarchies that map policies between their domains); inhibitAnyPolicy anywhere in
// the path withdraws the anyPolicy allowance.
func (c *CertIdentity) caPermitsPolicy(ca *x509.Certificate, i int, inhibitAny bool) error {
	if len(ca.PolicyMappings) > 0 {
		return certErr(agent.ReasonCertUsage, "CA certificate %d maps certificate policies, which is not supported", i)
	}
	if len(ca.Policies) == 0 {
		return nil
	}
	if slices.ContainsFunc(ca.Policies, c.policy.Equal) {
		return nil
	}
	if !inhibitAny && slices.ContainsFunc(ca.Policies, anyPolicy.Equal) {
		return nil
	}
	return certErr(agent.ReasonCertUsage, "CA certificate %d is restricted to other certificate policies than %s", i, c.policy)
}

var anyPolicy = mustOID("2.5.29.32.0")

func mustOID(s string) x509.OID {
	o, err := x509.ParseOID(s)
	if err != nil {
		panic(err)
	}
	return o
}

// constrainsKind reports whether the CA carries a permitted name constraint for the identifier kind.
func (c *CertIdentity) constrainsKind(ca *x509.Certificate) bool {
	if c.kind == IDEmail {
		return len(ca.PermittedEmailAddresses) > 0
	}
	return len(ca.PermittedURIDomains) > 0
}

// sigAlgAllowed is the signature algorithm profile for the signatures on the certificates of a
// chain: SHA-256 or better, never SHA-1 or MD5. The same list applies to every signed object the
// verifier accepts.
func sigAlgAllowed(a x509.SignatureAlgorithm) error {
	switch a {
	case x509.ECDSAWithSHA256, x509.ECDSAWithSHA384,
		x509.SHA256WithRSA, x509.SHA384WithRSA, x509.SHA512WithRSA,
		x509.SHA256WithRSAPSS, x509.SHA384WithRSAPSS, x509.SHA512WithRSAPSS,
		x509.PureEd25519, x509.MLDSA65:
		return nil
	}
	return fmt.Errorf("signature algorithm %s is not in the accepted profile", a)
}

// caKeyAllowed is the key profile for a CA: RSA of 3072 bits or more (2048 only through the dated
// exception, applied against at; a zero at means "the exception is configured"), ECDSA on P-256 or
// P-384, Ed25519, ML-DSA-65.
func caKeyAllowed(pub any, at, rsa2048Until time.Time) error {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		bits := k.N.BitLen()
		switch {
		case bits >= 3072:
			return nil
		case bits >= 2048 && !rsa2048Until.IsZero() && (at.IsZero() || at.Before(rsa2048Until)):
			return nil
		}
		return fmt.Errorf("RSA key of %d bits is below the profile (3072; 2048 only under an unexpired RSA2048Until)", bits)
	case *ecdsa.PublicKey:
		if k.Curve == elliptic.P256() || k.Curve == elliptic.P384() {
			return nil
		}
		return fmt.Errorf("ECDSA curve %s is not P-256 or P-384", k.Curve.Params().Name)
	case ed25519.PublicKey:
		return nil
	case *mldsa.PublicKey:
		if k.Parameters() == mldsa.MLDSA65() {
			return nil
		}
		return errors.New("ML-DSA key is not ML-DSA-65")
	}
	return fmt.Errorf("key type %T is not in the accepted profile", pub)
}

var oidSubjectAltName = asn1.ObjectIdentifier{2, 5, 29, 17}

// leafIdentifier returns the canonical identifier of the leaf's one subject alternative name.
//
// The SAN extension is read from its bytes, not through Certificate.DNSNames and the like, because
// the library does not expose every GeneralName form (otherName, directoryName, registeredID): a
// second name of a kind it hides would still be a second name. Exactly one name must be present, of
// the configured kind, with no other.
func (c *CertIdentity) leafIdentifier(leaf *x509.Certificate) (string, error) {
	var ext []byte
	for _, e := range leaf.Extensions {
		if e.Id.Equal(oidSubjectAltName) {
			ext = e.Value
			break
		}
	}
	if ext == nil {
		return "", certErr(agent.ReasonCertIdentity, "the leaf has no subject alternative name")
	}
	var names asn1.RawValue
	rest, err := asn1.Unmarshal(ext, &names)
	if err != nil || len(rest) != 0 || names.Class != asn1.ClassUniversal || names.Tag != asn1.TagSequence || !names.IsCompound {
		return "", certErr(agent.ReasonCertIdentity, "the subject alternative name extension is not a GeneralNames sequence")
	}
	var entries []asn1.RawValue
	for b := names.Bytes; len(b) > 0; {
		var rv asn1.RawValue
		b, err = asn1.Unmarshal(b, &rv)
		if err != nil {
			return "", certErr(agent.ReasonCertIdentity, "the subject alternative name extension does not parse: %v", err)
		}
		entries = append(entries, rv)
	}
	if len(entries) != 1 {
		return "", certErr(agent.ReasonCertIdentity, "the leaf has %d subject alternative names; exactly one is required", len(entries))
	}
	tag := 6 // uniformResourceIdentifier
	if c.kind == IDEmail {
		tag = 1 // rfc822Name
	}
	e := entries[0]
	if e.Class != asn1.ClassContextSpecific || e.Tag != tag || e.IsCompound {
		return "", certErr(agent.ReasonCertIdentity, "the leaf's only subject alternative name is not of the configured kind %q", c.kind)
	}
	ident, err := c.canon.Canonical(c.kind, string(e.Bytes))
	if err != nil {
		return "", certErr(agent.ReasonCertIdentity, "the leaf's identifier %s is outside the accepted profile: %v", quote(string(e.Bytes)), err)
	}
	return ident, nil
}

// chainDigest orders equivalent chains deterministically.
func chainDigest(ch []*x509.Certificate) string {
	h := sha256.New()
	for _, c := range ch {
		h.Write(c.Raw)
	}
	return string(h.Sum(nil))
}
