package rfc3161

import (
	"bytes"
	"crypto/fips140"
	"crypto/sha1" //nolint:gosec // only to detect a signingCertificate attribute that disagrees; SHA-1 is never trusted
	"crypto/x509"
	"encoding/asn1"
	"math/big"
	"net"
	"strings"
)

var (
	oidKeyUsage = asn1.ObjectIdentifier{2, 5, 29, 15}
	oidSAN      = asn1.ObjectIdentifier{2, 5, 29, 17}
)

// checkTSACert applies the signer certificate rules beyond the extended key usage: it is not a CA,
// and when it states a key usage that includes digitalSignature or nonRepudiation and nothing
// outside those and keyCertSign (the OpenSSL timestamp-signing purpose).
func checkTSACert(c *x509.Certificate) *Refusal {
	if c.IsCA {
		return refuse(ReasonTSAKeyUsage, "the TSA certificate is a CA")
	}
	for _, e := range c.Extensions {
		if !e.Id.Equal(oidKeyUsage) {
			continue
		}
		const sign = x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment
		if c.KeyUsage&sign == 0 {
			return refuse(ReasonTSAKeyUsage, "the TSA certificate's key usage lacks digitalSignature and nonRepudiation")
		}
		if c.KeyUsage&^(sign|x509.KeyUsageCertSign) != 0 {
			return refuse(ReasonTSAKeyUsage, "the TSA certificate's key usage has bits beyond signing")
		}
	}
	return nil
}

// checkTSAName compares the TSTInfo tsa GeneralName (RFC 3161 section 2.4.2) with the names of the
// signer certificate: a directoryName must equal the subject or a SAN directoryName byte for byte;
// a dNSName, rfc822Name, URI or iPAddress must equal one in the SAN. Other name forms are refused.
func checkTSAName(gn node, c *x509.Certificate) *Refusal {
	in, r := only(gn, "tsa")
	if r != nil {
		return r
	}
	no := func() *Refusal {
		return refuse(ReasonTSAName, "the TSTInfo tsa name does not match the TSA certificate")
	}
	if in.Class != asn1.ClassContextSpecific {
		return refuse(ReasonTSAName, "the TSTInfo tsa name is not a GeneralName")
	}
	switch in.Tag {
	case 4:
		name, r := only(in, "tsa directoryName")
		if r != nil {
			return r
		}
		if bytes.Equal(name.FullBytes, c.RawSubject) {
			return nil
		}
		for _, e := range c.Extensions {
			if !e.Id.Equal(oidSAN) {
				continue
			}
			san, _, r := parseNode(e.Value)
			if r != nil {
				return no()
			}
			names, r := kids(san)
			if r != nil {
				return no()
			}
			for _, n := range names {
				if isCtx(n, 4, true) {
					if inner, r := only(n, "SAN directoryName"); r == nil && bytes.Equal(inner.FullBytes, name.FullBytes) {
						return nil
					}
				}
			}
		}
		return no()
	case 2:
		for _, d := range c.DNSNames {
			if strings.EqualFold(d, string(in.Bytes)) {
				return nil
			}
		}
	case 1:
		for _, m := range c.EmailAddresses {
			if m == string(in.Bytes) {
				return nil
			}
		}
	case 6:
		for _, u := range c.URIs {
			if u.String() == string(in.Bytes) {
				return nil
			}
		}
	case 7:
		for _, ip := range c.IPAddresses {
			if (len(in.Bytes) == 4 || len(in.Bytes) == 16) && net.IP(in.Bytes).Equal(ip) {
				return nil
			}
		}
	default:
		return refuse(ReasonTSAName, "the TSTInfo tsa name has an unsupported form (tag %d)", in.Tag)
	}
	return no()
}

// parseESSV1 reads the SHA-1 signingCertificate attribute value and returns the hash, issuer and
// serial of its first certificate identifier (issuer and serial are nil when absent).
func parseESSV1(n node) ([]byte, []byte, *big.Int, *Refusal) {
	if !isSeq(n) {
		return nil, nil, nil, refuse(ReasonMalformed, "SigningCertificate is not a SEQUENCE")
	}
	cs, r := kids(n)
	if r != nil {
		return nil, nil, nil, r
	}
	if len(cs) == 0 || len(cs) > 2 || !isSeq(cs[0]) {
		return nil, nil, nil, refuse(ReasonMalformed, "SigningCertificate is malformed")
	}
	if len(cs) == 2 {
		if r := wellFormed(cs[1], 1); r != nil {
			return nil, nil, nil, r
		}
	}
	ids, r := kids(cs[0])
	if r != nil {
		return nil, nil, nil, r
	}
	if len(ids) == 0 || !isSeq(ids[0]) {
		return nil, nil, nil, refuse(ReasonMalformed, "SigningCertificate names no certificate")
	}
	for _, o := range ids[1:] {
		if r := wellFormed(o, 1); r != nil {
			return nil, nil, nil, r
		}
	}
	fs, r := kids(ids[0])
	if r != nil {
		return nil, nil, nil, r
	}
	if len(fs) == 0 || len(fs) > 2 || !isOctets(fs[0]) || len(fs[0].Bytes) != sha1.Size {
		return nil, nil, nil, refuse(ReasonMalformed, "ESSCertID is malformed")
	}
	if len(fs) == 2 {
		issuer, serial, r := parseIssuerSerial(fs[1])
		if r != nil {
			return nil, nil, nil, r
		}
		return fs[0].Bytes, issuer, serial, nil
	}
	return fs[0].Bytes, nil, nil, nil
}

// allowedESSV1 reports whether the policy lists a certificate with the signer's public key.
func allowedESSV1(signer *x509.Certificate, allow []*x509.Certificate) bool {
	for _, a := range allow {
		if a != nil && bytes.Equal(a.RawSubjectPublicKeyInfo, signer.RawSubjectPublicKeyInfo) {
			return true
		}
	}
	return false
}

// sha1Sum is the SHA-1 of b, for the consistency comparison against a signingCertificate (v1)
// attribute: the binding to the signer is the signingCertificateV2 SHA-256 hash, which is checked
// first, and SHA-1 is never trusted. Under GODEBUG=fips140=only crypto/sha1 panics, so a real token
// that carries both attributes (DigiCert's does) would crash verification; the comparison runs
// with enforcement off (fips140.WithoutEnforcement), which keeps the cross-check in place.
func sha1Sum(b []byte) []byte {
	var s [sha1.Size]byte
	fips140.WithoutEnforcement(func() { s = sha1.Sum(b) })
	return s[:]
}

// distrusted reports whether a certificate of the path shares a public key with a Distrust entry
// that has taken effect by genTime.
func distrusted(chain []*x509.Certificate, d []Distrust, genTimeAfter func(Distrust) bool) bool {
	for _, c := range chain {
		for _, e := range d {
			if e.Cert != nil && bytes.Equal(c.RawSubjectPublicKeyInfo, e.Cert.RawSubjectPublicKeyInfo) && genTimeAfter(e) {
				return true
			}
		}
	}
	return false
}
