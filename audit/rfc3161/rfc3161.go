// Package rfc3161 verifies RFC 3161 time-stamp tokens with the standard library alone.
//
// The standard library has no CMS (SignedData) parser, so this package reads the token itself with
// encoding/asn1 under a narrow, strict profile: anything outside the profile is refused with a
// named Reason, never interpreted. It is the independent time source behind a gate that requires
// one (docs/design/approver-identity.md, "RFC 3161 token verifier").
//
// The profile, each point a refusal when violated:
//
//   - DER only: BER, indefinite lengths, non-minimal encodings and trailing bytes are refused, and
//     the signed attributes must already be in DER SET OF order.
//   - A ContentInfo of type id-signedData, SignedData version 3, exactly one SignerInfo, exactly one
//     digestAlgorithms entry equal to the signer's, content type id-ct-TSTInfo, content present.
//     crls and attrCertificates are refused; unsignedAttrs must be well-formed DER and are never
//     read.
//   - TSTInfo version 1, a message imprint hash of SHA-256, SHA-384 or SHA-512 equal to the caller's
//     digest, genTime a DER GeneralizedTime, accuracy added to the tolerance, nonce equal to the
//     caller's when one is expected, ordering and tsa ignored, any critical extension refused.
//   - The signed attributes contentType, messageDigest and signingCertificateV2 each exactly once
//     and matching. A token with the SHA-1 signingCertificate attribute only is refused (FreeTSA
//     tokens are of this kind); when signingCertificateV2 is present, other attributes, including
//     signingCertificate, are ignored but must be well-formed DER.
//   - Signature algorithms: ECDSA P-256 or P-384, RSA PKCS#1 v1.5 or PSS, Ed25519, with SHA-256 or
//     better; RSA moduli of at least 3072 bits (2048 only through Policy.RSA2048Until). SHA-1, MD5
//     and SHA-224 are never accepted, in the CMS digests, the ESS hash, the imprint or the
//     certificate chain.
//   - The signer certificate chains to a trust anchor in Policy.Roots at genTime, carries the
//     critical id-kp-timeStamping extended key usage as its only one, is not a CA, has a key usage
//     (when it states one) of digitalSignature or nonRepudiation without other bits beyond
//     keyCertSign, is not distrusted by Policy.DistrustAfter, and passes Policy.CertStatus. A
//     TSTInfo tsa name must match the certificate, the TSA policy must be in Policy.AcceptedPolicies
//     when that is set, and the stated accuracy may not exceed Policy.MaxAccuracy (one second by
//     default).
//   - A signingCertificate (SHA-1) attribute beside signingCertificateV2 is read only to refuse a
//     token whose two attributes name different certificates.
//   - The token size, certificate count and DER nesting depth are bounded before parsing.
//
// Time bounds. genTime must not be earlier than Expected.HeadTime less Policy.ClockTolerance and the
// token's accuracy (itself capped by Policy.MaxAccuracy), and, when Expected.Now is set, not later
// than Now plus the tolerance. Both bounds depend on the caller's clocks, so a log clock running ahead
// refuses a genuine token: the check fails closed. The TSA certificate path is evaluated at genTime,
// which a holder of a retired or compromised TSA key chooses; Policy.DistrustAfter and the status
// function limit that, and the long-term answer is renewal by an archive timestamp.
//
// The verifier takes the token (the ContentInfo), not the TimeStampResp: the caller that extracts
// it must check that the PKIStatus is granted. The imprint is a bare hash; the caller should hash a
// domain-separated structure of the head so that a token obtained for another purpose cannot be
// presented. Policy.RSA2048Until is evaluated against genTime, so it is bounded by the same
// back-dating window.
//
// Certificates in the token that are not used are still read: one that Go's parser rejects refuses
// the token (a parseable but unused certificate is ignored; the path is built only from the
// auditor's anchors).
package rfc3161

import (
	"crypto"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Reason names why a token was refused. The set is closed: every refusal carries one.
type Reason string

// The refusal reasons. Those in the first group are outside the profile (the token may be a valid
// RFC 3161 token that this verifier does not interpret); those in the second are checks the token
// failed inside the profile.
const (
	ReasonTooLarge             Reason = "too-large"
	ReasonTooDeep              Reason = "too-deep"
	ReasonTooManyCerts         Reason = "too-many-certificates"
	ReasonNotDER               Reason = "not-der"
	ReasonTrailingBytes        Reason = "trailing-bytes"
	ReasonMalformed            Reason = "malformed"
	ReasonNotSignedData        Reason = "not-signed-data"
	ReasonSignedDataVersion    Reason = "signed-data-version"
	ReasonSignerCount          Reason = "signer-count"
	ReasonDigestAlgs           Reason = "digest-algorithms"
	ReasonContentType          Reason = "content-type"
	ReasonDetached             Reason = "detached-content"
	ReasonCRLs                 Reason = "crls-present"
	ReasonAttrCerts            Reason = "attribute-certificates"
	ReasonSignerVersion        Reason = "signer-info-version"
	ReasonSignerID             Reason = "signer-identifier"
	ReasonNoSignedAttrs        Reason = "no-signed-attributes"
	ReasonSignedAttrsOrder     Reason = "signed-attributes-not-der-order"
	ReasonMissingAttr          Reason = "missing-signed-attribute"
	ReasonDuplicateAttr        Reason = "duplicate-signed-attribute"
	ReasonESSV1Only            Reason = "signing-certificate-v1-only"
	ReasonWeakHash             Reason = "weak-hash"
	ReasonUnsupportedAlgorithm Reason = "unsupported-algorithm"
	ReasonWeakKey              Reason = "weak-key"
	ReasonTSTVersion           Reason = "tstinfo-version"
	ReasonCriticalExtension    Reason = "tstinfo-critical-extension"
	ReasonAccuracy             Reason = "accuracy-out-of-range"
	ReasonGenTimeFormat        Reason = "gentime-format"

	ReasonAttrContentType    Reason = "content-type-attribute-mismatch"
	ReasonMessageDigest      Reason = "message-digest-mismatch"
	ReasonESSMismatch        Reason = "signing-certificate-mismatch"
	ReasonSignerCertNotFound Reason = "signer-certificate-not-found"
	ReasonBadSignature       Reason = "bad-signature"
	ReasonImprintMismatch    Reason = "imprint-mismatch"
	ReasonGenTimeBeforeHead  Reason = "gentime-before-head"
	ReasonNonceMismatch      Reason = "nonce-mismatch"
	ReasonNoTrustAnchors     Reason = "no-trust-anchors"
	ReasonUntrustedTSA       Reason = "untrusted-tsa"
	ReasonTSACertExpired     Reason = "tsa-certificate-expired"
	ReasonTSAEKU             Reason = "tsa-extended-key-usage"
	ReasonTSAStatus          Reason = "tsa-certificate-status"
	ReasonTSAName            Reason = "tsa-name-mismatch"
	ReasonTSAKeyUsage        Reason = "tsa-key-usage"
	ReasonTSADistrusted      Reason = "tsa-distrusted"
	ReasonPolicy             Reason = "policy-not-accepted"
	ReasonGenTimeFuture      Reason = "gentime-in-future"
)

// OutsideProfile reports whether r names a construct the verifier does not interpret, as opposed
// to a check the token failed.
func (r Reason) OutsideProfile() bool {
	switch r {
	case ReasonAttrContentType, ReasonMessageDigest, ReasonESSMismatch, ReasonSignerCertNotFound,
		ReasonBadSignature, ReasonImprintMismatch, ReasonGenTimeBeforeHead, ReasonNonceMismatch,
		ReasonNoTrustAnchors, ReasonUntrustedTSA, ReasonTSACertExpired, ReasonTSAEKU, ReasonTSAStatus,
		ReasonTSAName, ReasonTSAKeyUsage, ReasonTSADistrusted, ReasonPolicy, ReasonGenTimeFuture, ReasonAccuracy:
		return false
	}
	return true
}

// ErrRefused is wrapped by every *Refusal, so errors.Is(err, ErrRefused) tells a refused token
// from a misconfigured call.
var ErrRefused = errors.New("rfc3161: token refused")

// ErrConfig is wrapped by the error for a Policy or Expected that cannot be used.
var ErrConfig = errors.New("rfc3161: invalid verifier configuration")

// Refusal is the error for a token the verifier refused. Detail carries no token bytes beyond
// short identifiers.
type Refusal struct {
	Reason Reason
	Detail string
	err    error
}

// Error implements error.
func (r *Refusal) Error() string {
	s := "rfc3161: token refused (" + string(r.Reason) + ")"
	if r.Detail != "" {
		s += ": " + r.Detail
	}
	return s
}

// Is reports a match with ErrRefused.
func (r *Refusal) Is(target error) bool { return target == ErrRefused }

// Unwrap returns the underlying error, when there is one (a status-hook error, for example).
func (r *Refusal) Unwrap() error { return r.err }

func refuse(r Reason, format string, a ...any) *Refusal {
	return &Refusal{Reason: r, Detail: fmt.Sprintf(format, a...)}
}

// Default limits, applied when the Policy field is zero.
const (
	DefaultMaxTokenBytes = 128 << 10
	DefaultMaxCerts      = 10
	maxDepth             = 16
	hardMaxTokenBytes    = 1 << 20
	maxAccuracy          = 24 * time.Hour // sanity bound while reading, before the policy's cap applies
	// DefaultMaxAccuracy is the largest TSA-stated accuracy accepted when Policy.MaxAccuracy is zero.
	DefaultMaxAccuracy = time.Second
)

// Policy is what the auditor decides: which TSAs are trusted and how strictly.
type Policy struct {
	// Roots holds the trust anchors for TSA certificates (the auditor's -trusted-tsa list).
	// Required: a nil pool is refused, so the system roots are never used.
	Roots *x509.CertPool
	// Intermediates are extra certificates for chain building, beside those in the token.
	Intermediates []*x509.Certificate
	// CertStatus applies the status rule to the TSA certificate at genTime, given the chain from
	// the signer to the anchor. Required: the verifier never skips the status check silently. A
	// non-nil error refuses the token (reason tsa-certificate-status).
	CertStatus func(signer *x509.Certificate, chain []*x509.Certificate, at time.Time) error
	// RSA2048Until permits RSA moduli of 2048 bits (never smaller) in the signer and chain
	// certificates and the token signature when genTime is before it. The zero value refuses 2048.
	RSA2048Until time.Time
	// ClockTolerance is added to the accuracy when comparing genTime with Expected.HeadTime.
	ClockTolerance time.Duration
	// MaxAccuracy is the largest accuracy the token may state; zero means DefaultMaxAccuracy and it
	// may not exceed 24 hours. The accuracy widens the back-dating window against the head (genTime
	// may be that much earlier than the head less ClockTolerance), so a bank sets it to what its
	// TSA actually states.
	MaxAccuracy time.Duration
	// AcceptedPolicies lists the TSA policy OIDs the caller accepts; empty accepts any, and the
	// caller then must check Result.PolicyOID itself.
	AcceptedPolicies []asn1.ObjectIdentifier
	// DistrustAfter retires TSA keys or anchors at a time: a path containing a certificate with the
	// same public key as an entry is refused when genTime is at or after the entry's time. It is how
	// a TSA key that was retired or compromised stops being believed for dates after that time,
	// since a holder of such a key can write any genTime.
	DistrustAfter []Distrust
	// MaxTokenBytes and MaxCerts cap the input; zero means DefaultMaxTokenBytes and DefaultMaxCerts.
	// MaxTokenBytes may not exceed 1 MiB.
	MaxTokenBytes int
	MaxCerts      int
}

// Distrust ends belief in a TSA key (or anchor) from a time on.
type Distrust struct {
	Cert  *x509.Certificate // matched by public key
	After time.Time
}

// StatusUnknown is a Policy.CertStatus that refuses every certificate: the fail-closed choice for a
// caller that has no status data yet. A real hook must check the status of every non-anchor
// certificate of the chain as of genTime, using the status rule's usable-for-absence window (a CRL
// pruned after the certificate expired proves nothing), and return an error for revoked, unknown
// or unprovable status.
func StatusUnknown(*x509.Certificate, []*x509.Certificate, time.Time) error {
	return errors.New("certificate status is not known")
}

// Expected is what the caller knows the token must say.
type Expected struct {
	// Hash and Digest are the algorithm (SHA-256, SHA-384 or SHA-512) and the digest the caller
	// computed over the covering head or bundle. The token's imprint must equal them.
	Hash   crypto.Hash
	Digest []byte
	// HeadTime is when the stamped head was made. genTime must not be earlier than it, less the
	// accuracy and Policy.ClockTolerance. Required.
	HeadTime time.Time
	// Now, when non-zero, bounds genTime from above: a genTime later than Now plus the tolerance
	// (less the accuracy) is refused. A live gate sets it; an offline audit leaves it zero.
	Now time.Time
	// Nonce, when non-nil, must equal the token's nonce (the verifier made the request).
	Nonce *big.Int
}

// Result is what a verified token attests. The TSA states that the true time lies within Accuracy
// of GenTime. To show that something existed no later than a time, use Latest, not GenTime.
type Result struct {
	// GenTime is the TSA's time, Accuracy its stated accuracy (zero when absent).
	GenTime  time.Time
	Accuracy time.Duration
	// Hash and Imprint are the stamped digest. PolicyOID is the TSA policy, Serial the token serial.
	Hash      crypto.Hash
	Imprint   []byte
	PolicyOID string
	Serial    *big.Int
	// Signer is the TSA certificate and Chain the verified path from it to the anchor.
	Signer *x509.Certificate
	Chain  []*x509.Certificate
}

// Latest is the latest the token allows for the moment it was issued: GenTime plus Accuracy. It is
// the time a verifier may use for "the stamped data existed by".
func (r *Result) Latest() time.Time { return r.GenTime.Add(r.Accuracy) }
