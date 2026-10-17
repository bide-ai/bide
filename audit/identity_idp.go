package audit

import (
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"slices"
)

// The issuing distribution point (IDP) rule. An IDP narrows what a CRL covers (RFC 5280 section 5.2.5 and
// 6.3.3): a CRL whose IDP names a distribution point covers only the certificates whose own CRL
// distribution points extension names that point. bide reads one shape of IDP, the one step-ca stamps on
// every CRL: critical, holding only distributionPoint.fullName made of URIs. Every other IDP (a scope
// flag, a reasons field, a relative name, a name that is not a URI, an empty one) is refused, because
// reading it would need the rest of the RFC 5280 partitioning rules. A CRL with such an IDP gives
// evidence of absence only for a certificate that names one of its URIs, byte for byte, in a
// distribution point that carries no reasons and no cRLIssuer; a listing in it revokes whatever the
// certificate names, because a listing can only take trust away.

var oidCRLDistributionPoints = asn1.ObjectIdentifier{2, 5, 29, 31}

var errIDPShape = errors.New("the CRL carries an issuing distribution point that is not exactly a full name made of URIs (a partitioned CRL cannot be matched correctly)")

// derElem reads one DER element of the given class and tag from b and returns its content and the rest.
func derElem(b []byte, class, tag int, compound bool) (content, rest []byte, ok bool) {
	var rv asn1.RawValue
	rest, err := asn1.Unmarshal(b, &rv)
	if err != nil || rv.Class != class || rv.Tag != tag || rv.IsCompound != compound {
		return nil, nil, false
	}
	return rv.Bytes, rest, true
}

// fullNameURIs parses the content of a [0] fullName GeneralNames into its URIs; ok is false if it is empty
// or holds any name that is not a uniformResourceIdentifier of printable ASCII.
func fullNameURIs(b []byte) ([]string, bool) {
	var out []string
	for len(b) > 0 {
		var rv asn1.RawValue
		rest, err := asn1.Unmarshal(b, &rv)
		if err != nil || rv.Class != asn1.ClassContextSpecific || rv.Tag != 6 || rv.IsCompound || len(rv.Bytes) == 0 {
			return nil, false
		}
		for _, c := range rv.Bytes {
			if c < 0x21 || c > 0x7e {
				return nil, false
			}
		}
		out = append(out, string(rv.Bytes))
		b = rest
	}
	return out, len(out) > 0
}

// parseIDP reads the value of an IDP extension: SEQUENCE { [0] { [0] fullName } } and nothing else.
func parseIDP(value []byte, critical bool) ([]string, error) {
	if !critical {
		return nil, errIDPShape
	}
	seq, rest, ok := derElem(value, asn1.ClassUniversal, asn1.TagSequence, true)
	if !ok || len(rest) != 0 {
		return nil, errIDPShape
	}
	dp, rest, ok := derElem(seq, asn1.ClassContextSpecific, 0, true) // distributionPoint
	if !ok || len(rest) != 0 {
		return nil, errIDPShape
	}
	full, rest, ok := derElem(dp, asn1.ClassContextSpecific, 0, true) // fullName
	if !ok || len(rest) != 0 {
		return nil, errIDPShape
	}
	uris, ok := fullNameURIs(full)
	if !ok {
		return nil, errIDPShape
	}
	return uris, nil
}

// certNamesPoint reports whether one of cert's CRL distribution points names one of uris: a point that
// holds only a fullName (no reasons, no cRLIssuer) with that URI, byte for byte.
func certNamesPoint(cert *x509.Certificate, uris []string) bool {
	for _, e := range cert.Extensions {
		if !e.Id.Equal(oidCRLDistributionPoints) {
			continue
		}
		list, rest, ok := derElem(e.Value, asn1.ClassUniversal, asn1.TagSequence, true)
		if !ok || len(rest) != 0 {
			return false
		}
		for len(list) > 0 {
			var dp asn1.RawValue
			var err error
			if list, err = asn1.Unmarshal(list, &dp); err != nil || dp.Class != asn1.ClassUniversal || dp.Tag != asn1.TagSequence {
				return false
			}
			// A point is read only if it is exactly [0] { [0] fullName }.
			name, r, ok := derElem(dp.Bytes, asn1.ClassContextSpecific, 0, true)
			if !ok || len(r) != 0 {
				continue
			}
			full, r, ok := derElem(name, asn1.ClassContextSpecific, 0, true)
			if !ok || len(r) != 0 {
				continue
			}
			var rv asn1.RawValue
			for len(full) > 0 {
				if full, err = asn1.Unmarshal(full, &rv); err != nil {
					break
				}
				if rv.Class == asn1.ClassContextSpecific && rv.Tag == 6 && !rv.IsCompound && slices.Contains(uris, string(rv.Bytes)) {
					return true
				}
			}
		}
	}
	return false
}

// protocol:identity begin CrlCovers

// covers reports whether v can give evidence of absence for cert: a CRL without an IDP covers every
// certificate of its issuer, one with an IDP only a certificate that names it.
func (v *crlView) covers(cert *x509.Certificate) bool {
	if len(v.idp) == 0 {
		return true
	}
	return certNamesPoint(cert, v.idp)
}

// protocol:identity end
