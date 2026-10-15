package rfc3161

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"time"
)

var oidSigningCertificateV1 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 12}

// Verify checks token against exp and pol and returns what it attests. A token outside the profile
// or failing a check yields a *Refusal (errors.Is(err, ErrRefused)); an unusable exp or pol yields
// an error wrapping ErrConfig. Verify never panics on input and allocates in proportion to the
// bounded input size.
func Verify(token []byte, exp Expected, pol Policy) (*Result, error) {
	if err := checkConfig(exp, pol); err != nil {
		return nil, err
	}
	maxBytes, maxCerts := pol.MaxTokenBytes, pol.MaxCerts
	if maxBytes == 0 {
		maxBytes = DefaultMaxTokenBytes
	}
	if maxCerts == 0 {
		maxCerts = DefaultMaxCerts
	}
	if len(token) > maxBytes {
		return nil, refuse(ReasonTooLarge, "%d bytes, the limit is %d", len(token), maxBytes)
	}
	if pol.Roots == nil {
		return nil, refuse(ReasonNoTrustAnchors, "no trust anchors in the policy")
	}
	res, r := verify(token, exp, pol, maxCerts)
	if r != nil {
		return nil, r
	}
	return res, nil
}

func checkConfig(exp Expected, pol Policy) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrConfig, fmt.Sprintf(format, a...))
	}
	switch exp.Hash {
	case crypto.SHA256, crypto.SHA384, crypto.SHA512:
	default:
		return bad("expected hash %v is not SHA-256, SHA-384 or SHA-512", exp.Hash)
	}
	if len(exp.Digest) != exp.Hash.Size() {
		return bad("expected digest has %d bytes, %v needs %d", len(exp.Digest), exp.Hash, exp.Hash.Size())
	}
	if exp.HeadTime.IsZero() {
		return bad("Expected.HeadTime is required")
	}
	if pol.CertStatus == nil {
		return bad("Policy.CertStatus is required: the TSA certificate status rule is never skipped")
	}
	if pol.ClockTolerance < 0 {
		return bad("negative clock tolerance")
	}
	if pol.MaxTokenBytes < 0 || pol.MaxTokenBytes > hardMaxTokenBytes || pol.MaxCerts < 0 {
		return bad("size limits out of range")
	}
	return nil
}

type signerInfo struct {
	digest    algIdent
	hash      crypto.Hash
	sidIssuer []byte // raw issuer Name (issuerAndSerialNumber form)
	sidSerial *big.Int
	sidSKI    []byte
	attrsNode node
	attrs     []node
	sig       sigAlg
	signature []byte
}

type essID struct {
	hash      crypto.Hash
	certHash  []byte
	issuerRaw []byte // nil when issuerSerial is absent
	serial    *big.Int
}

type tstInfo struct {
	imprintHash crypto.Hash
	imprint     []byte
	policy      string
	serial      *big.Int
	genTime     time.Time
	accuracy    time.Duration
	nonce       *big.Int
}

func verify(token []byte, exp Expected, pol Policy, maxCerts int) (*Result, *Refusal) {
	ci, rest, r := parseNode(token)
	if r != nil {
		return nil, r
	}
	if len(rest) != 0 {
		return nil, refuse(ReasonTrailingBytes, "%d bytes after the token", len(rest))
	}
	if !isSeq(ci) {
		return nil, refuse(ReasonMalformed, "ContentInfo is not a SEQUENCE")
	}
	cs, r := kids(ci)
	if r != nil {
		return nil, r
	}
	if len(cs) != 2 {
		return nil, refuse(ReasonMalformed, "ContentInfo has %d elements", len(cs))
	}
	ct, r := oidOf(cs[0], "ContentInfo type")
	if r != nil {
		return nil, r
	}
	if !ct.Equal(oidSignedData) {
		return nil, refuse(ReasonNotSignedData, "content type %s", ct)
	}
	if !isCtx(cs[1], 0, true) {
		return nil, refuse(ReasonMalformed, "ContentInfo content is not [0] EXPLICIT")
	}
	sdNode, r := only(cs[1], "ContentInfo content")
	if r != nil {
		return nil, r
	}
	if !isSeq(sdNode) {
		return nil, refuse(ReasonMalformed, "SignedData is not a SEQUENCE")
	}
	sd, r := kids(sdNode)
	if r != nil {
		return nil, r
	}
	i := 0
	next := func() (node, bool) {
		if i < len(sd) {
			i++
			return sd[i-1], true
		}
		return node{}, false
	}
	peek := func() (node, bool) {
		if i < len(sd) {
			return sd[i], true
		}
		return node{}, false
	}

	// version, digestAlgorithms, encapContentInfo
	vn, ok := next()
	if !ok {
		return nil, refuse(ReasonMalformed, "SignedData is empty")
	}
	ver, r := smallOf(vn, "SignedData version")
	if r != nil {
		return nil, r
	}
	if ver != 3 {
		return nil, refuse(ReasonSignedDataVersion, "version %d, want 3", ver)
	}
	dn, ok := next()
	if !ok || !isSet(dn) {
		return nil, refuse(ReasonMalformed, "SignedData digestAlgorithms is missing or not a SET")
	}
	dalgs, r := kids(dn)
	if r != nil {
		return nil, r
	}
	if len(dalgs) != 1 {
		return nil, refuse(ReasonDigestAlgs, "%d digest algorithms, want exactly one", len(dalgs))
	}
	setAlg, r := parseAlg(dalgs[0], "digestAlgorithms entry")
	if r != nil {
		return nil, r
	}
	en, ok := next()
	if !ok || !isSeq(en) {
		return nil, refuse(ReasonMalformed, "encapContentInfo is missing or not a SEQUENCE")
	}
	eContent, r := parseEncap(en)
	if r != nil {
		return nil, r
	}

	// optional certificates [0], crls [1], then signerInfos
	var certNodes []node
	if n, ok := peek(); ok && n.Class == asn1.ClassContextSpecific && n.Tag == 0 {
		if !n.IsCompound {
			return nil, refuse(ReasonMalformed, "certificates is not constructed")
		}
		i++
		certNodes, r = kids(n)
		if r != nil {
			return nil, r
		}
		if len(certNodes) > maxCerts {
			return nil, refuse(ReasonTooManyCerts, "%d certificates, the limit is %d", len(certNodes), maxCerts)
		}
		for _, c := range certNodes {
			switch {
			case isSeq(c):
			case c.Class == asn1.ClassContextSpecific && (c.Tag == 1 || c.Tag == 2):
				return nil, refuse(ReasonAttrCerts, "attribute certificate in the token")
			default:
				return nil, refuse(ReasonMalformed, "unsupported CertificateChoices form")
			}
		}
	}
	if n, ok := peek(); ok && n.Class == asn1.ClassContextSpecific && n.Tag == 1 {
		return nil, refuse(ReasonCRLs, "crls present")
	}
	sn, ok := next()
	if !ok || !isSet(sn) {
		return nil, refuse(ReasonMalformed, "signerInfos is missing or not a SET")
	}
	if _, more := peek(); more {
		return nil, refuse(ReasonMalformed, "unexpected element after signerInfos")
	}
	sis, r := kids(sn)
	if r != nil {
		return nil, r
	}
	if len(sis) != 1 {
		return nil, refuse(ReasonSignerCount, "%d SignerInfos, want exactly one", len(sis))
	}
	si, r := parseSignerInfo(sis[0])
	if r != nil {
		return nil, r
	}
	if !setAlg.oid.Equal(si.digest.oid) {
		return nil, refuse(ReasonDigestAlgs, "digestAlgorithms names %s, the signer %s", setAlg.oid, si.digest.oid)
	}
	if !setAlg.noParams() {
		return nil, refuse(ReasonMalformed, "digestAlgorithms entry has parameters")
	}

	// signed attributes
	attrs, r := checkSignedAttrs(si, eContent)
	if r != nil {
		return nil, r
	}

	// TSTInfo
	tst, r := parseTSTInfo(eContent)
	if r != nil {
		return nil, r
	}

	// signer certificate
	var parsed []*x509.Certificate
	seen := map[string]bool{}
	for k, c := range certNodes {
		cert, err := x509.ParseCertificate(c.FullBytes)
		if err != nil {
			return nil, refuse(ReasonMalformed, "certificate %d in the token does not parse: %v", k, err)
		}
		if !seen[string(cert.Raw)] {
			seen[string(cert.Raw)] = true
			parsed = append(parsed, cert)
		}
	}
	for _, c := range pol.Intermediates {
		if c != nil && !seen[string(c.Raw)] {
			seen[string(c.Raw)] = true
			parsed = append(parsed, c)
		}
	}
	signer, r := selectSigner(parsed, si, attrs)
	if r != nil {
		return nil, r
	}

	// the signer certificate: key, extended key usage
	if r := checkKey(signer.PublicKey, tst.genTime, pol.RSA2048Until, "the TSA certificate"); r != nil {
		return nil, r
	}
	if r := checkEKU(signer); r != nil {
		return nil, r
	}

	// the token signature, over the DER SET OF signed attributes (RFC 5652 section 5.4)
	if r := checkSignature(signer, si); r != nil {
		return nil, r
	}

	// the TSTInfo against what the caller expects
	if tst.imprintHash != exp.Hash || !bytes.Equal(tst.imprint, exp.Digest) {
		return nil, refuse(ReasonImprintMismatch, "the token stamps a different digest")
	}
	if exp.Nonce != nil && (tst.nonce == nil || tst.nonce.Cmp(exp.Nonce) != 0) {
		return nil, refuse(ReasonNonceMismatch, "the token's nonce is absent or differs from the request's")
	}
	earliest := exp.HeadTime.Add(-pol.ClockTolerance - tst.accuracy)
	if tst.genTime.Before(earliest) {
		return nil, refuse(ReasonGenTimeBeforeHead, "genTime %s is earlier than the head time %s less tolerance and accuracy",
			tst.genTime.Format(time.RFC3339Nano), exp.HeadTime.UTC().Format(time.RFC3339Nano))
	}

	// the chain and the status rule, at genTime
	chain, r := verifyChain(signer, parsed, tst.genTime, pol)
	if r != nil {
		return nil, r
	}
	return &Result{
		GenTime: tst.genTime, Accuracy: tst.accuracy, Hash: tst.imprintHash, Imprint: append([]byte(nil), tst.imprint...),
		PolicyOID: tst.policy, Serial: tst.serial, Signer: signer, Chain: chain,
	}, nil
}

// parseEncap reads encapContentInfo and returns the eContent octets.
func parseEncap(en node) ([]byte, *Refusal) {
	cs, r := kids(en)
	if r != nil {
		return nil, r
	}
	if len(cs) == 0 || len(cs) > 2 {
		return nil, refuse(ReasonMalformed, "encapContentInfo has %d elements", len(cs))
	}
	o, r := oidOf(cs[0], "eContentType")
	if r != nil {
		return nil, r
	}
	if !o.Equal(oidTSTInfo) {
		return nil, refuse(ReasonContentType, "eContentType %s, want id-ct-TSTInfo", o)
	}
	if len(cs) == 1 {
		return nil, refuse(ReasonDetached, "the TSTInfo is not in the token")
	}
	if !isCtx(cs[1], 0, true) {
		return nil, refuse(ReasonMalformed, "eContent is not [0] EXPLICIT")
	}
	oc, r := only(cs[1], "eContent")
	if r != nil {
		return nil, r
	}
	if !isOctets(oc) {
		return nil, refuse(ReasonNotDER, "eContent is not a primitive OCTET STRING")
	}
	return oc.Bytes, nil
}

func parseSignerInfo(n node) (*signerInfo, *Refusal) {
	if !isSeq(n) {
		return nil, refuse(ReasonMalformed, "SignerInfo is not a SEQUENCE")
	}
	cs, r := kids(n)
	if r != nil {
		return nil, r
	}
	i := 0
	take := func(what string) (node, *Refusal) {
		if i >= len(cs) {
			return node{}, refuse(ReasonMalformed, "SignerInfo lacks %s", what)
		}
		i++
		return cs[i-1], nil
	}
	si := &signerInfo{}
	vn, r := take("version")
	if r != nil {
		return nil, r
	}
	ver, r := smallOf(vn, "SignerInfo version")
	if r != nil {
		return nil, r
	}
	sid, r := take("sid")
	if r != nil {
		return nil, r
	}
	switch {
	case isSeq(sid):
		if ver != 1 {
			return nil, refuse(ReasonSignerVersion, "issuerAndSerialNumber needs SignerInfo version 1, got %d", ver)
		}
		ids, r := kids(sid)
		if r != nil {
			return nil, r
		}
		if len(ids) != 2 || !isSeq(ids[0]) {
			return nil, refuse(ReasonMalformed, "issuerAndSerialNumber is malformed")
		}
		si.sidIssuer = ids[0].FullBytes
		if si.sidSerial, r = bigOf(ids[1], "signer serial number"); r != nil {
			return nil, r
		}
	case sid.Class == asn1.ClassContextSpecific && sid.Tag == 0 && !sid.IsCompound:
		if ver != 3 {
			return nil, refuse(ReasonSignerVersion, "subjectKeyIdentifier needs SignerInfo version 3, got %d", ver)
		}
		if len(sid.Bytes) == 0 {
			return nil, refuse(ReasonMalformed, "empty subjectKeyIdentifier")
		}
		si.sidSKI = sid.Bytes
	default:
		if ver != 1 && ver != 3 {
			return nil, refuse(ReasonSignerVersion, "SignerInfo version %d", ver)
		}
		return nil, refuse(ReasonSignerID, "unknown SignerIdentifier form")
	}
	dn, r := take("digestAlgorithm")
	if r != nil {
		return nil, r
	}
	if si.digest, r = parseAlg(dn, "signer digestAlgorithm"); r != nil {
		return nil, r
	}
	if si.hash, r = hashFromOID(si.digest, "signer digestAlgorithm"); r != nil {
		return nil, r
	}
	if i < len(cs) && isCtx(cs[i], 0, true) {
		si.attrsNode = cs[i]
		i++
	} else {
		return nil, refuse(ReasonNoSignedAttrs, "the SignerInfo has no signed attributes")
	}
	an, r := take("signatureAlgorithm")
	if r != nil {
		return nil, r
	}
	if si.sig, r = parseSigAlg(an, si.hash); r != nil {
		return nil, r
	}
	sg, r := take("signature")
	if r != nil {
		return nil, r
	}
	if !isOctets(sg) {
		return nil, refuse(ReasonMalformed, "signature is not an OCTET STRING")
	}
	si.signature = sg.Bytes
	if i < len(cs) && isCtx(cs[i], 1, true) {
		// unsignedAttrs: never read, but held to DER
		ua, r := kids(cs[i])
		if r != nil {
			return nil, r
		}
		for _, a := range ua {
			if !isSeq(a) {
				return nil, refuse(ReasonMalformed, "an unsigned attribute is not a SEQUENCE")
			}
			if r := wellFormed(a, 1); r != nil {
				return nil, r
			}
		}
		i++
	}
	if i != len(cs) {
		return nil, refuse(ReasonMalformed, "unexpected element in SignerInfo")
	}
	if si.attrs, r = kids(si.attrsNode); r != nil {
		return nil, r
	}
	return si, nil
}

// checkSignedAttrs verifies the DER order of the signed attributes and the three required ones,
// and returns the signing-certificate identifier.
func checkSignedAttrs(si *signerInfo, eContent []byte) (*essID, *Refusal) {
	for k := 1; k < len(si.attrs); k++ {
		if bytes.Compare(si.attrs[k-1].FullBytes, si.attrs[k].FullBytes) > 0 {
			return nil, refuse(ReasonSignedAttrsOrder, "the signed attributes are not in DER SET OF order")
		}
	}
	var contentType, msgDigest, sigCertV2 []node
	v1 := false
	for _, a := range si.attrs {
		if !isSeq(a) {
			return nil, refuse(ReasonMalformed, "a signed attribute is not a SEQUENCE")
		}
		ac, r := kids(a)
		if r != nil {
			return nil, r
		}
		if len(ac) != 2 || !isSet(ac[1]) {
			return nil, refuse(ReasonMalformed, "a signed attribute is not {type, SET}")
		}
		o, r := oidOf(ac[0], "signed attribute type")
		if r != nil {
			return nil, r
		}
		vals, r := kids(ac[1])
		if r != nil {
			return nil, r
		}
		switch {
		case o.Equal(oidAttrContent):
			contentType = append(contentType, vals...)
			if len(vals) != 1 {
				return nil, refuse(ReasonMalformed, "contentType attribute has %d values", len(vals))
			}
		case o.Equal(oidAttrDigest):
			msgDigest = append(msgDigest, vals...)
			if len(vals) != 1 {
				return nil, refuse(ReasonMalformed, "messageDigest attribute has %d values", len(vals))
			}
		case o.Equal(oidAttrSigCertV2):
			sigCertV2 = append(sigCertV2, vals...)
			if len(vals) != 1 {
				return nil, refuse(ReasonMalformed, "signingCertificateV2 attribute has %d values", len(vals))
			}
		default:
			if o.Equal(oidSigningCertificateV1) {
				v1 = true
			}
			for _, v := range vals {
				if r := wellFormed(v, 1); r != nil {
					return nil, r
				}
			}
		}
	}
	if len(contentType) > 1 || len(msgDigest) > 1 || len(sigCertV2) > 1 {
		return nil, refuse(ReasonDuplicateAttr, "a required signed attribute appears more than once")
	}
	if len(contentType) == 0 {
		return nil, refuse(ReasonMissingAttr, "no contentType signed attribute")
	}
	if len(msgDigest) == 0 {
		return nil, refuse(ReasonMissingAttr, "no messageDigest signed attribute")
	}
	if len(sigCertV2) == 0 {
		if v1 {
			return nil, refuse(ReasonESSV1Only, "only the SHA-1 signingCertificate attribute is present")
		}
		return nil, refuse(ReasonMissingAttr, "no signingCertificateV2 signed attribute")
	}
	ctOID, r := oidOf(contentType[0], "contentType attribute value")
	if r != nil {
		return nil, r
	}
	if !ctOID.Equal(oidTSTInfo) {
		return nil, refuse(ReasonAttrContentType, "contentType attribute is %s", ctOID)
	}
	if !isOctets(msgDigest[0]) {
		return nil, refuse(ReasonMalformed, "messageDigest value is not an OCTET STRING")
	}
	h := si.hash.New()
	h.Write(eContent)
	if !bytes.Equal(h.Sum(nil), msgDigest[0].Bytes) {
		return nil, refuse(ReasonMessageDigest, "messageDigest differs from the hash of the TSTInfo")
	}
	return parseESSCertIDv2(sigCertV2[0])
}

func parseESSCertIDv2(n node) (*essID, *Refusal) {
	if !isSeq(n) {
		return nil, refuse(ReasonMalformed, "SigningCertificateV2 is not a SEQUENCE")
	}
	cs, r := kids(n)
	if r != nil {
		return nil, r
	}
	if len(cs) == 0 || len(cs) > 2 || !isSeq(cs[0]) {
		return nil, refuse(ReasonMalformed, "SigningCertificateV2 is malformed")
	}
	if len(cs) == 2 {
		if !isSeq(cs[1]) {
			return nil, refuse(ReasonMalformed, "SigningCertificateV2 policies is not a SEQUENCE")
		}
		if r := wellFormed(cs[1], 1); r != nil {
			return nil, r
		}
	}
	ids, r := kids(cs[0])
	if r != nil {
		return nil, r
	}
	if len(ids) == 0 {
		return nil, refuse(ReasonMalformed, "SigningCertificateV2 names no certificate")
	}
	for _, other := range ids[1:] {
		if !isSeq(other) {
			return nil, refuse(ReasonMalformed, "an ESSCertIDv2 is not a SEQUENCE")
		}
		if r := wellFormed(other, 1); r != nil {
			return nil, r
		}
	}
	if !isSeq(ids[0]) {
		return nil, refuse(ReasonMalformed, "an ESSCertIDv2 is not a SEQUENCE")
	}
	fs, r := kids(ids[0])
	if r != nil {
		return nil, r
	}
	e := &essID{hash: crypto.SHA256}
	j := 0
	if j < len(fs) && isSeq(fs[j]) {
		a, r := parseAlg(fs[j], "ESSCertIDv2 hashAlgorithm")
		if r != nil {
			return nil, r
		}
		h, r := hashFromOID(a, "ESSCertIDv2 hashAlgorithm")
		if r != nil {
			return nil, r
		}
		if h == crypto.SHA256 {
			return nil, refuse(ReasonNotDER, "ESSCertIDv2 spells out its default hash algorithm")
		}
		e.hash = h
		j++
	}
	if j >= len(fs) || !isOctets(fs[j]) {
		return nil, refuse(ReasonMalformed, "ESSCertIDv2 lacks certHash")
	}
	e.certHash = fs[j].Bytes
	j++
	if len(e.certHash) != e.hash.Size() {
		return nil, refuse(ReasonMalformed, "ESSCertIDv2 certHash has %d bytes", len(e.certHash))
	}
	if j < len(fs) {
		is := fs[j]
		j++
		if !isSeq(is) {
			return nil, refuse(ReasonMalformed, "issuerSerial is not a SEQUENCE")
		}
		iss, r := kids(is)
		if r != nil {
			return nil, r
		}
		if len(iss) != 2 || !isSeq(iss[0]) {
			return nil, refuse(ReasonMalformed, "issuerSerial is malformed")
		}
		gns, r := kids(iss[0])
		if r != nil {
			return nil, r
		}
		if len(gns) != 1 || !isCtx(gns[0], 4, true) {
			return nil, refuse(ReasonMalformed, "issuerSerial issuer is not a single directoryName")
		}
		name, r := only(gns[0], "issuerSerial directoryName")
		if r != nil {
			return nil, r
		}
		if !isSeq(name) {
			return nil, refuse(ReasonMalformed, "issuerSerial directoryName is not a Name")
		}
		e.issuerRaw = name.FullBytes
		if e.serial, r = bigOf(iss[1], "issuerSerial serial number"); r != nil {
			return nil, r
		}
	}
	if j != len(fs) {
		return nil, refuse(ReasonMalformed, "unexpected element in ESSCertIDv2")
	}
	return e, nil
}

// selectSigner finds the certificate the SignerIdentifier names whose hash is the ESS one.
func selectSigner(certs []*x509.Certificate, si *signerInfo, e *essID) (*x509.Certificate, *Refusal) {
	var matched []*x509.Certificate
	for _, c := range certs {
		switch {
		case si.sidSKI != nil:
			if bytes.Equal(c.SubjectKeyId, si.sidSKI) {
				matched = append(matched, c)
			}
		default:
			if bytes.Equal(c.RawIssuer, si.sidIssuer) && c.SerialNumber.Cmp(si.sidSerial) == 0 {
				matched = append(matched, c)
			}
		}
	}
	if len(matched) == 0 {
		return nil, refuse(ReasonSignerCertNotFound, "no certificate matches the SignerIdentifier")
	}
	var signer *x509.Certificate
	for _, c := range matched {
		h := e.hash.New()
		h.Write(c.Raw)
		if bytes.Equal(h.Sum(nil), e.certHash) {
			signer = c
			break
		}
	}
	if signer == nil {
		return nil, refuse(ReasonESSMismatch, "no matching certificate has the hash in signingCertificateV2")
	}
	if e.issuerRaw != nil && (!bytes.Equal(e.issuerRaw, signer.RawIssuer) || e.serial.Cmp(signer.SerialNumber) != 0) {
		return nil, refuse(ReasonESSMismatch, "signingCertificateV2 issuerSerial differs from the certificate")
	}
	return signer, nil
}

// checkEKU requires the extended key usage extension to be critical and to hold id-kp-timeStamping
// and nothing else (RFC 3161 section 2.3).
func checkEKU(c *x509.Certificate) *Refusal {
	var ext *int
	for k := range c.Extensions {
		if c.Extensions[k].Id.Equal(oidExtKeyUsage) {
			if ext != nil {
				return refuse(ReasonTSAEKU, "the TSA certificate has two extended key usage extensions")
			}
			kk := k
			ext = &kk
		}
	}
	if ext == nil {
		return refuse(ReasonTSAEKU, "the TSA certificate has no extended key usage extension")
	}
	e := c.Extensions[*ext]
	if !e.Critical {
		return refuse(ReasonTSAEKU, "the TSA certificate's extended key usage is not critical")
	}
	var oids []asn1.ObjectIdentifier
	rest, err := asn1.Unmarshal(e.Value, &oids)
	if err != nil || len(rest) != 0 {
		return refuse(ReasonTSAEKU, "the TSA certificate's extended key usage is malformed")
	}
	if len(oids) != 1 || !oids[0].Equal(oidKPTimeStamping) {
		return refuse(ReasonTSAEKU, "the TSA certificate's extended key usage is not exactly id-kp-timeStamping")
	}
	return nil
}

func checkSignature(c *x509.Certificate, si *signerInfo) *Refusal {
	// RFC 5652 section 5.4: the signature covers the DER encoding of the attributes as a SET OF.
	// The order was checked, so the received bytes with the [0] tag replaced are that encoding.
	signed := append([]byte(nil), si.attrsNode.FullBytes...)
	signed[0] = 0x31
	bad := func() *Refusal { return refuse(ReasonBadSignature, "the signature does not verify") }
	mism := func() *Refusal {
		return refuse(ReasonUnsupportedAlgorithm, "the signature algorithm does not match the TSA certificate's key")
	}
	switch si.sig.kind {
	case sigEd25519:
		k, ok := c.PublicKey.(ed25519.PublicKey)
		if !ok {
			return mism()
		}
		if !ed25519.Verify(k, signed, si.signature) {
			return bad()
		}
		return nil
	}
	h := si.sig.hash.New()
	h.Write(signed)
	sum := h.Sum(nil)
	switch si.sig.kind {
	case sigECDSA:
		k, ok := c.PublicKey.(*ecdsa.PublicKey)
		if !ok {
			return mism()
		}
		if !ecdsa.VerifyASN1(k, sum, si.signature) {
			return bad()
		}
	case sigRSAPKCS1:
		k, ok := c.PublicKey.(*rsa.PublicKey)
		if !ok {
			return mism()
		}
		if rsa.VerifyPKCS1v15(k, si.sig.hash, sum, si.signature) != nil {
			return bad()
		}
	case sigRSAPSS:
		k, ok := c.PublicKey.(*rsa.PublicKey)
		if !ok {
			return mism()
		}
		if rsa.VerifyPSS(k, si.sig.hash, sum, si.signature, &rsa.PSSOptions{SaltLength: si.sig.salt}) != nil {
			return bad()
		}
	default:
		return mism()
	}
	return nil
}

// verifyChain builds a path from the signer to a trust anchor valid at genTime, applies the key
// and signature algorithm allowlist to every certificate on it, and then the caller's status rule.
// The first path that passes both is returned; otherwise the first path's refusal is.
func verifyChain(signer *x509.Certificate, pool []*x509.Certificate, genTime time.Time, pol Policy) ([]*x509.Certificate, *Refusal) {
	inter := x509.NewCertPool()
	for _, c := range pool {
		inter.AddCert(c)
	}
	chains, err := signer.Verify(x509.VerifyOptions{
		Roots: pol.Roots, Intermediates: inter, CurrentTime: genTime,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
	})
	if err != nil {
		var inv x509.CertificateInvalidError
		var insecure x509.InsecureAlgorithmError
		// Go reports a SHA-1 or MD5 signature as an unknown authority with a hint; name it.
		for _, c := range append([]*x509.Certificate{signer}, pool...) {
			if r := checkCertSignature(c); r != nil && r.Reason == ReasonWeakHash {
				return nil, r
			}
		}
		switch {
		case errors.As(err, &insecure):
			return nil, refuse(ReasonWeakHash, "a certificate on the path uses an insecure algorithm")
		case errors.As(err, &inv) && inv.Reason == x509.Expired:
			return nil, refuse(ReasonTSACertExpired, "a certificate on the path is not valid at genTime %s", genTime.Format(time.RFC3339))
		}
		return nil, refuse(ReasonUntrustedTSA, "no path to a trusted TSA anchor at genTime: %v", err)
	}
	var first *Refusal
	for _, chain := range chains {
		r := checkPath(chain, genTime, pol)
		if r == nil {
			if err := pol.CertStatus(signer, chain, genTime); err != nil {
				r = &Refusal{Reason: ReasonTSAStatus, Detail: "the status rule refused the TSA certificate: " + err.Error(), err: err}
			}
		}
		if r == nil {
			return chain, nil
		}
		if first == nil {
			first = r
		}
	}
	return nil, first
}

func checkPath(chain []*x509.Certificate, genTime time.Time, pol Policy) *Refusal {
	for k, c := range chain {
		if r := checkKey(c.PublicKey, genTime, pol.RSA2048Until, "a certificate on the path"); r != nil {
			return r
		}
		if k < len(chain)-1 { // a trust anchor's own signature is not verified
			if r := checkCertSignature(c); r != nil {
				return r
			}
		}
	}
	return nil
}

// parseTSTInfo reads TSTInfo (RFC 3161 section 2.4.2) under the profile.
func parseTSTInfo(b []byte) (*tstInfo, *Refusal) {
	n, rest, r := parseNode(b)
	if r != nil {
		return nil, r
	}
	if len(rest) != 0 {
		return nil, refuse(ReasonTrailingBytes, "%d bytes after the TSTInfo", len(rest))
	}
	if !isSeq(n) {
		return nil, refuse(ReasonMalformed, "TSTInfo is not a SEQUENCE")
	}
	cs, r := kids(n)
	if r != nil {
		return nil, r
	}
	if len(cs) < 5 {
		return nil, refuse(ReasonMalformed, "TSTInfo has %d elements", len(cs))
	}
	ver, r := smallOf(cs[0], "TSTInfo version")
	if r != nil {
		return nil, r
	}
	if ver != 1 {
		return nil, refuse(ReasonTSTVersion, "TSTInfo version %d, want 1", ver)
	}
	t := &tstInfo{}
	pol, r := oidOf(cs[1], "TSTInfo policy")
	if r != nil {
		return nil, r
	}
	t.policy = pol.String()
	if !isSeq(cs[2]) {
		return nil, refuse(ReasonMalformed, "messageImprint is not a SEQUENCE")
	}
	mi, r := kids(cs[2])
	if r != nil {
		return nil, r
	}
	if len(mi) != 2 || !isOctets(mi[1]) {
		return nil, refuse(ReasonMalformed, "messageImprint is malformed")
	}
	ha, r := parseAlg(mi[0], "imprint hashAlgorithm")
	if r != nil {
		return nil, r
	}
	if t.imprintHash, r = hashFromOID(ha, "imprint hashAlgorithm"); r != nil {
		return nil, r
	}
	t.imprint = mi[1].Bytes
	if t.serial, r = bigOf(cs[3], "TSTInfo serialNumber"); r != nil {
		return nil, r
	}
	if !isUniversal(cs[4], asn1.TagGeneralizedTime) || cs[4].IsCompound {
		return nil, refuse(ReasonGenTimeFormat, "genTime is not a GeneralizedTime")
	}
	if t.genTime, r = parseGenTime(cs[4].Bytes); r != nil {
		return nil, r
	}
	rest2 := cs[5:]
	if len(rest2) > 0 && isSeq(rest2[0]) {
		if t.accuracy, r = parseAccuracy(rest2[0]); r != nil {
			return nil, r
		}
		rest2 = rest2[1:]
	}
	if len(rest2) > 0 && isUniversal(rest2[0], asn1.TagBoolean) {
		if len(rest2[0].Bytes) != 1 || rest2[0].Bytes[0] != 0xff {
			return nil, refuse(ReasonNotDER, "ordering is not DER TRUE (FALSE is the omitted default)")
		}
		rest2 = rest2[1:]
	}
	if len(rest2) > 0 && isInt(rest2[0]) {
		if t.nonce, r = bigOf(rest2[0], "TSTInfo nonce"); r != nil {
			return nil, r
		}
		rest2 = rest2[1:]
	}
	if len(rest2) > 0 && isCtx(rest2[0], 0, true) { // tsa: not compared with the certificate
		if r := wellFormed(rest2[0], 1); r != nil {
			return nil, r
		}
		rest2 = rest2[1:]
	}
	if len(rest2) > 0 && isCtx(rest2[0], 1, true) {
		if r := checkExtensions(rest2[0]); r != nil {
			return nil, r
		}
		rest2 = rest2[1:]
	}
	if len(rest2) != 0 {
		return nil, refuse(ReasonMalformed, "unexpected element in TSTInfo")
	}
	return t, nil
}

func checkExtensions(n node) *Refusal {
	exts, r := kids(n)
	if r != nil {
		return r
	}
	if len(exts) == 0 {
		return refuse(ReasonMalformed, "TSTInfo extensions is empty")
	}
	for _, e := range exts {
		if !isSeq(e) {
			return refuse(ReasonMalformed, "a TSTInfo extension is not a SEQUENCE")
		}
		fs, r := kids(e)
		if r != nil {
			return r
		}
		if len(fs) < 2 || len(fs) > 3 {
			return refuse(ReasonMalformed, "a TSTInfo extension has %d elements", len(fs))
		}
		if _, r := oidOf(fs[0], "extension id"); r != nil {
			return r
		}
		if len(fs) == 3 {
			if !isUniversal(fs[1], asn1.TagBoolean) || len(fs[1].Bytes) != 1 {
				return refuse(ReasonMalformed, "extension critical flag is malformed")
			}
			if fs[1].Bytes[0] != 0xff { // DER omits the default FALSE and writes TRUE as 0xff
				return refuse(ReasonNotDER, "extension critical flag is not DER TRUE")
			}
			return refuse(ReasonCriticalExtension, "TSTInfo carries a critical extension the verifier does not know")
		}
		if !isOctets(fs[len(fs)-1]) {
			return refuse(ReasonMalformed, "extension value is not an OCTET STRING")
		}
	}
	return nil
}

func parseAccuracy(n node) (time.Duration, *Refusal) {
	cs, r := kids(n)
	if r != nil {
		return 0, r
	}
	var d time.Duration
	bad := func(what string) (time.Duration, *Refusal) {
		return 0, refuse(ReasonAccuracy, "accuracy %s out of range", what)
	}
	j := 0
	if j < len(cs) && isInt(cs[j]) {
		v, r := bigOf(cs[j], "accuracy seconds")
		if r != nil {
			return 0, r
		}
		if v.Sign() < 0 || !v.IsInt64() || time.Duration(v.Int64()) > maxAccuracy/time.Second {
			return bad("seconds")
		}
		d += time.Duration(v.Int64()) * time.Second
		j++
	}
	if j < len(cs) && isCtx(cs[j], 0, false) {
		v, r := implicitInt(cs[j], "accuracy millis")
		if r != nil {
			return 0, r
		}
		if v.Int64() < 1 || v.Int64() > 999 {
			return bad("millis")
		}
		d += time.Duration(v.Int64()) * time.Millisecond
		j++
	}
	if j < len(cs) && isCtx(cs[j], 1, false) {
		v, r := implicitInt(cs[j], "accuracy micros")
		if r != nil {
			return 0, r
		}
		if v.Int64() < 1 || v.Int64() > 999 {
			return bad("micros")
		}
		d += time.Duration(v.Int64()) * time.Microsecond
		j++
	}
	if j != len(cs) {
		return 0, refuse(ReasonMalformed, "unexpected element in Accuracy")
	}
	if d > maxAccuracy {
		return bad("total")
	}
	return d, nil
}

// parseGenTime reads a DER GeneralizedTime: YYYYMMDDHHMMSS, an optional fraction of one to nine
// digits with no trailing zero, and Z.
func parseGenTime(b []byte) (time.Time, *Refusal) {
	bad := func(why string) (time.Time, *Refusal) {
		return time.Time{}, refuse(ReasonGenTimeFormat, "genTime %s", why)
	}
	if len(b) < 15 || b[len(b)-1] != 'Z' {
		return bad("is not YYYYMMDDHHMMSS[.f]Z")
	}
	digits := func(s []byte) (int, bool) {
		v := 0
		for _, c := range s {
			if c < '0' || c > '9' {
				return 0, false
			}
			v = v*10 + int(c-'0')
		}
		return v, true
	}
	var f [7]int
	for k, w := range [][2]int{{0, 4}, {4, 6}, {6, 8}, {8, 10}, {10, 12}, {12, 14}} {
		v, ok := digits(b[w[0]:w[1]])
		if !ok {
			return bad("has a non-digit")
		}
		f[k] = v
	}
	nanos := 0
	frac := b[14 : len(b)-1]
	if len(frac) > 0 {
		if frac[0] != '.' || len(frac) < 2 || len(frac) > 10 {
			return bad("has a malformed or over-long fraction")
		}
		if frac[len(frac)-1] == '0' {
			return bad("has a fraction with a trailing zero (not DER)")
		}
		v, ok := digits(frac[1:])
		if !ok {
			return bad("has a non-digit in the fraction")
		}
		nanos = v
		for k := len(frac) - 1; k < 9; k++ {
			nanos *= 10
		}
	}
	t := time.Date(f[0], time.Month(f[1]), f[2], f[3], f[4], f[5], nanos, time.UTC)
	if t.Format("20060102150405") != string(b[:14]) { // a normalized date (February 30, hour 24) differs
		return bad("is not a valid date and time")
	}
	return t, nil
}
