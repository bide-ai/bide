package rfc3161

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	_ "crypto/sha256" // register the hashes used below
	_ "crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"time"
)

// algIdent is a parsed AlgorithmIdentifier: the OID and the parameters element, when present.
type algIdent struct {
	oid    asn1.ObjectIdentifier
	params *node
}

func parseAlg(n node, what string) (algIdent, *Refusal) {
	if !isSeq(n) {
		return algIdent{}, refuse(ReasonMalformed, "%s is not a SEQUENCE", what)
	}
	cs, r := kids(n)
	if r != nil {
		return algIdent{}, r
	}
	if len(cs) == 0 || len(cs) > 2 {
		return algIdent{}, refuse(ReasonMalformed, "%s has %d elements", what, len(cs))
	}
	o, r := oidOf(cs[0], what)
	if r != nil {
		return algIdent{}, r
	}
	a := algIdent{oid: o}
	if len(cs) == 2 {
		a.params = &cs[1]
	}
	return a, nil
}

// noParams reports whether the parameters are absent or NULL, the two forms of a hash algorithm.
func (a algIdent) noParams() bool {
	return a.params == nil || (isUniversal(*a.params, asn1.TagNull) && len(a.params.Bytes) == 0)
}

// hashOf classifies a hash OID: the SHA-2 hashes of 256 bits and more are allowed; SHA-1, MD5 and
// SHA-224 are named as weak; anything else is unsupported.
func hashFromOID(a algIdent, what string) (crypto.Hash, *Refusal) {
	switch {
	case a.oid.Equal(oidSHA256), a.oid.Equal(oidSHA384), a.oid.Equal(oidSHA512):
		if !a.noParams() {
			return 0, refuse(ReasonMalformed, "%s has parameters", what)
		}
		switch {
		case a.oid.Equal(oidSHA256):
			return crypto.SHA256, nil
		case a.oid.Equal(oidSHA384):
			return crypto.SHA384, nil
		}
		return crypto.SHA512, nil
	case a.oid.Equal(oidSHA1), a.oid.Equal(oidMD5), a.oid.Equal(oidSHA224):
		return 0, refuse(ReasonWeakHash, "%s uses %s", what, a.oid)
	}
	return 0, refuse(ReasonUnsupportedAlgorithm, "%s uses hash %s", what, a.oid)
}

type sigKind int

const (
	sigECDSA sigKind = iota + 1
	sigRSAPKCS1
	sigRSAPSS
	sigEd25519
)

type sigAlg struct {
	kind sigKind
	hash crypto.Hash // zero when the algorithm does not name one (plain rsaEncryption)
	salt int         // PSS salt length
}

// parseSigAlg reads a SignerInfo signatureAlgorithm. digest is the SignerInfo digestAlgorithm.
func parseSigAlg(n node, digest crypto.Hash) (sigAlg, *Refusal) {
	a, r := parseAlg(n, "signature algorithm")
	if r != nil {
		return sigAlg{}, r
	}
	named := func(kind sigKind, h crypto.Hash) (sigAlg, *Refusal) {
		if kind == sigRSAPKCS1 {
			if !a.noParams() {
				return sigAlg{}, refuse(ReasonMalformed, "signature algorithm %s has parameters", a.oid)
			}
		} else if a.params != nil {
			return sigAlg{}, refuse(ReasonMalformed, "signature algorithm %s has parameters", a.oid)
		}
		if h != digest {
			return sigAlg{}, refuse(ReasonUnsupportedAlgorithm, "signature algorithm %s does not match the signer digest algorithm", a.oid)
		}
		return sigAlg{kind: kind, hash: h}, nil
	}
	switch {
	case a.oid.Equal(oidECDSASHA256):
		return named(sigECDSA, crypto.SHA256)
	case a.oid.Equal(oidECDSASHA384):
		return named(sigECDSA, crypto.SHA384)
	case a.oid.Equal(oidECDSASHA512):
		return named(sigECDSA, crypto.SHA512)
	case a.oid.Equal(oidSHA256WithRSA):
		return named(sigRSAPKCS1, crypto.SHA256)
	case a.oid.Equal(oidSHA384WithRSA):
		return named(sigRSAPKCS1, crypto.SHA384)
	case a.oid.Equal(oidSHA512WithRSA):
		return named(sigRSAPKCS1, crypto.SHA512)
	case a.oid.Equal(oidRSAEncryption):
		return named(sigRSAPKCS1, digest) // RFC 5652: the hash is the digestAlgorithm
	case a.oid.Equal(oidECDSASHA1), a.oid.Equal(oidECDSASHA224), a.oid.Equal(oidMD5WithRSA),
		a.oid.Equal(oidSHA1WithRSA), a.oid.Equal(oidSHA224WithRSA):
		return sigAlg{}, refuse(ReasonWeakHash, "signature algorithm %s uses a weak hash", a.oid)
	case a.oid.Equal(oidEd25519):
		if a.params != nil {
			return sigAlg{}, refuse(ReasonMalformed, "Ed25519 has parameters")
		}
		if digest != crypto.SHA512 {
			return sigAlg{}, refuse(ReasonUnsupportedAlgorithm, "Ed25519 needs the SHA-512 digest algorithm (RFC 8419)")
		}
		return sigAlg{kind: sigEd25519}, nil
	case a.oid.Equal(oidRSAPSS):
		return parsePSS(a, digest)
	}
	return sigAlg{}, refuse(ReasonUnsupportedAlgorithm, "signature algorithm %s", a.oid)
}

// parsePSS reads RSASSA-PSS-params (RFC 4055): the hash and the MGF1 hash must be explicit, equal
// and allowed; the trailer field is left to its default.
func parsePSS(a algIdent, digest crypto.Hash) (sigAlg, *Refusal) {
	if a.params == nil || !isSeq(*a.params) {
		return sigAlg{}, refuse(ReasonUnsupportedAlgorithm, "RSASSA-PSS without parameters (defaults to SHA-1)")
	}
	cs, r := kids(*a.params)
	if r != nil {
		return sigAlg{}, r
	}
	var hashAlg, mgfAlg *node
	salt := 20
	last := -1
	for _, c := range cs {
		if c.Class != asn1.ClassContextSpecific || !c.IsCompound || c.Tag <= last || c.Tag > 2 {
			return sigAlg{}, refuse(ReasonMalformed, "unexpected, repeated or misordered element in RSASSA-PSS parameters")
		}
		last = c.Tag
		in, r := only(c, "PSS parameter")
		if r != nil {
			return sigAlg{}, r
		}
		switch c.Tag {
		case 0:
			hashAlg = &in
		case 1:
			mgfAlg = &in
		case 2:
			v, r := smallOf(in, "PSS salt length")
			if r != nil {
				return sigAlg{}, r
			}
			if v < 0 || v > 1024 {
				return sigAlg{}, refuse(ReasonUnsupportedAlgorithm, "PSS salt length %d", v)
			}
			salt = int(v)
		}
	}
	if hashAlg == nil || mgfAlg == nil {
		return sigAlg{}, refuse(ReasonUnsupportedAlgorithm, "RSASSA-PSS needs explicit hash and MGF1 parameters")
	}
	ha, r := parseAlg(*hashAlg, "PSS hash")
	if r != nil {
		return sigAlg{}, r
	}
	h, r := hashFromOID(ha, "PSS hash")
	if r != nil {
		return sigAlg{}, r
	}
	ma, r := parseAlg(*mgfAlg, "PSS mask generation")
	if r != nil {
		return sigAlg{}, r
	}
	if !ma.oid.Equal(oidMGF1) || ma.params == nil {
		return sigAlg{}, refuse(ReasonUnsupportedAlgorithm, "PSS mask generation is not MGF1")
	}
	mh, r := parseAlg(*ma.params, "MGF1 hash")
	if r != nil {
		return sigAlg{}, r
	}
	mhash, r := hashFromOID(mh, "MGF1 hash")
	if r != nil {
		return sigAlg{}, r
	}
	if mhash != h || h != digest {
		return sigAlg{}, refuse(ReasonUnsupportedAlgorithm, "PSS hash, MGF1 hash and signer digest algorithm differ")
	}
	return sigAlg{kind: sigRSAPSS, hash: h, salt: salt}, nil
}

// checkKey applies the algorithm allowlist to a public key: ECDSA P-256 or P-384, Ed25519, RSA of
// at least 3072 bits (2048 while genTime is before the policy's exception date).
func checkKey(pub any, genTime time.Time, until time.Time, what string) *Refusal {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return refuse(ReasonWeakKey, "%s uses curve %s", what, k.Curve.Params().Name)
		}
	case ed25519.PublicKey:
	case *rsa.PublicKey:
		bits := k.N.BitLen()
		switch {
		case bits >= 3072:
		case bits >= 2048 && !until.IsZero() && genTime.Before(until):
		default:
			return refuse(ReasonWeakKey, "%s is a %d-bit RSA key", what, bits)
		}
	default:
		return refuse(ReasonWeakKey, "%s has an unsupported key type", what)
	}
	return nil
}

// checkCertSignature applies the allowlist to the algorithm a CA used to sign a chain certificate.
func checkCertSignature(c *x509.Certificate) *Refusal {
	switch c.SignatureAlgorithm {
	case x509.ECDSAWithSHA256, x509.ECDSAWithSHA384, x509.ECDSAWithSHA512,
		x509.SHA256WithRSA, x509.SHA384WithRSA, x509.SHA512WithRSA,
		x509.SHA256WithRSAPSS, x509.SHA384WithRSAPSS, x509.SHA512WithRSAPSS, x509.PureEd25519:
		return nil
	case x509.MD2WithRSA, x509.MD5WithRSA, x509.SHA1WithRSA, x509.ECDSAWithSHA1, x509.DSAWithSHA1:
		return refuse(ReasonWeakHash, "a chain certificate is signed with %s", c.SignatureAlgorithm)
	}
	return refuse(ReasonUnsupportedAlgorithm, "a chain certificate is signed with %s", c.SignatureAlgorithm)
}
