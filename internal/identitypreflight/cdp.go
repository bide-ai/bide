package identitypreflight

import (
	"crypto/x509"
	"slices"
	"strings"

	"github.com/bide-ai/bide/audit"
)

// cdpChecks is the check cert.cdp: how the leaf's CRL distribution points relate to the CRLs the gate holds.
// A CRL with an issuing distribution point covers a certificate only if one of the certificate's own plain
// distribution points names one of its URIs byte for byte (RFC 5280 section 5.2.5; the rule is
// audit.CertIdentity's evalCert), so a leaf that names none of them gets no status from such a CRL and the gate
// waits. And a leaf with several plain points that share no name is read from a CRL covering any one of them,
// which is right for mirrors of one CRL and not for a CA that partitions.
func (s *run) cdpChecks(der [][]byte) {
	r := s.r
	leaf, err := x509.ParseCertificate(der[0])
	if err != nil {
		return
	}
	points, ok := audit.PlainDistributionPoints(leaf)
	if !ok {
		r.add("cert.cdp", Warn, "have the CA issue the certificate with a well-formed CRL distribution points extension", "the certificate's CRL distribution points extension does not parse, so no CRL with an issuing distribution point can cover it")
		return
	}
	disjoint := false
	for i := range points {
		for j := i + 1; j < len(points); j++ {
			if !slices.ContainsFunc(points[i], func(u string) bool { return slices.Contains(points[j], u) }) {
				disjoint = true
			}
		}
	}
	if len(points) > 1 && disjoint {
		r.add("cert.cdp", Warn, "put in each certificate only the distribution point of the CRL that lists it; several points are for mirrors of one CRL",
			"the certificate has %d plain CRL distribution points with different names (%s): its status is read from a CRL covering any one of them, so a CA that partitions its CRLs and names every partition would let one partition's clean CRL stand for another's", len(points), joinPoints(points))
		return
	}
	if s.snap == nil || s.o.NoStatus || !s.haveCRL || len(der) < 2 {
		return
	}
	issuer, err := x509.ParseCertificate(der[1])
	if err != nil {
		return
	}
	var scoped, covering int
	var names []string
	for _, c := range s.snap.HeldCRLs(issuer) {
		if len(c.DistributionPoints) == 0 {
			covering++
			continue
		}
		scoped++
		names = append(names, c.DistributionPoints...)
		if slices.ContainsFunc(points, func(p []string) bool {
			return slices.ContainsFunc(p, func(u string) bool { return slices.Contains(c.DistributionPoints, u) })
		}) {
			covering++
		}
	}
	if scoped > 0 && covering == 0 {
		r.add("cert.cdp", Fail, "have the CA stamp, in each certificate, a CRL distribution point equal to the URI in its CRL's issuing distribution point (step-ca: the template's crlDistributionPoints equal to crl.idpURL)",
			"the CRLs of this certificate's issuer carry an issuing distribution point (%s) that none of the certificate's plain CRL distribution points names (%s), so no CRL covers it and the gate waits for status that cannot arrive", join(dedup(names)), joinPoints(points))
		return
	}
	r.add("cert.cdp", OK, "", "the certificate's CRL distribution points and the CRLs held are consistent (%d point(s); %d CRL(s) scoped by an issuing distribution point)", len(points), scoped)
}

func joinPoints(points [][]string) string {
	var all []string
	for _, p := range points {
		all = append(all, strings.Join(p, " | "))
	}
	if len(all) == 0 {
		return "none"
	}
	return strings.Join(all, "; ")
}

func dedup(s []string) []string {
	slices.Sort(s)
	return slices.Compact(s)
}
