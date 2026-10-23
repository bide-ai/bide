package rfc3161

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/fips140"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"testing"
	"time"
)

// This file is a small RFC 3161 token writer for tests. It builds tokens piece by piece so a test
// can break exactly one rule; the verifier under test shares no code with it.

func tlv(tag byte, parts ...[]byte) []byte {
	var body []byte
	for _, p := range parts {
		body = append(body, p...)
	}
	out := []byte{tag}
	switch n := len(body); {
	case n < 0x80:
		out = append(out, byte(n))
	case n < 0x100:
		out = append(out, 0x81, byte(n))
	case n < 0x10000:
		out = append(out, 0x82, byte(n>>8), byte(n))
	default:
		out = append(out, 0x83, byte(n>>16), byte(n>>8), byte(n))
	}
	return append(out, body...)
}

func seq(p ...[]byte) []byte         { return tlv(0x30, p...) }
func setOf(p ...[]byte) []byte       { return tlv(0x31, p...) }
func octets(b []byte) []byte         { return tlv(0x04, b) }
func ctx(n int, p ...[]byte) []byte  { return tlv(0xA0|byte(n), p...) }
func ctxP(n int, p ...[]byte) []byte { return tlv(0x80|byte(n), p...) }
func nul() []byte                    { return []byte{5, 0} }

func oidDER(o asn1.ObjectIdentifier) []byte {
	b, err := asn1.Marshal(o)
	if err != nil {
		panic(err)
	}
	return b
}

func intDER(n int64) []byte {
	b, _ := asn1.Marshal(n)
	return b
}

func bigDER(n *big.Int) []byte {
	b, _ := asn1.Marshal(n)
	return b
}

func algID(o asn1.ObjectIdentifier, params ...[]byte) []byte {
	return seq(append([][]byte{oidDER(o)}, params...)...)
}

func algNull(o asn1.ObjectIdentifier) []byte { return algID(o, nul()) }

func genTimeDER(s string) []byte { return tlv(0x18, []byte(s)) }

var keyCache sync.Map

type keyKind string

const (
	kEC256   keyKind = "ec256"
	kEC384   keyKind = "ec384"
	kRSA2048 keyKind = "rsa2048"
	kRSA3072 keyKind = "rsa3072"
	kEd25519 keyKind = "ed25519"
	kEC521   keyKind = "ec521"
	kRSA1024 keyKind = "rsa1024"
)

func genKey(t testing.TB, k keyKind) crypto.Signer {
	t.Helper()
	if v, ok := keyCache.Load(k); ok && (k == kRSA2048 || k == kRSA3072 || k == kRSA1024) {
		return v.(crypto.Signer)
	}
	var s crypto.Signer
	var err error
	switch k {
	case kEC256:
		s, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case kEC384:
		s, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case kEC521:
		s, err = ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	case kRSA1024:
		fips140.WithoutEnforcement(func() { s, err = rsa.GenerateKey(rand.Reader, 1024) })
	case kRSA2048:
		s, err = rsa.GenerateKey(rand.Reader, 2048)
	case kRSA3072:
		s, err = rsa.GenerateKey(rand.Reader, 3072)
	case kEd25519:
		_, s, err = ed25519.GenerateKey(rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	keyCache.Store(k, s)
	return s
}

type kitOpts struct {
	rootKey, tsaKey keyKind
	// EKU: nil means the correct one. noEKU omits the extension. Not critical when ekuNotCritical.
	eku            []asn1.ObjectIdentifier
	noEKU          bool
	ekuNotCritical bool
	notBefore      time.Time
	notAfter       time.Time
	rootSigAlg     x509.SignatureAlgorithm
	keyUsage       x509.KeyUsage // zero means digitalSignature
	tsaIsCA        bool
	dnsNames       []string
	sanDirName     []byte // a directoryName subjectAltName (Name DER)
	ski            bool   // give the TSA certificate a subject key id (needed for SKI signer ids)
}

type kit struct {
	rootKey, tsaKey crypto.Signer
	root, tsa       *x509.Certificate
	pool            *x509.CertPool
}

// newKit builds a TSA kit. Some negative tests need material that GODEBUG=fips140=only refuses to
// make (a 1024-bit RSA key, a SHA-1 signed certificate), so the kit is built with enforcement off;
// the code under test still runs with it on.
func newKit(t testing.TB, o kitOpts) (k *kit) {
	t.Helper()
	fips140.WithoutEnforcement(func() { k = newKitUnenforced(t, o) })
	return k
}

func newKitUnenforced(t testing.TB, o kitOpts) *kit {
	t.Helper()
	if o.rootKey == "" {
		o.rootKey = kEC256
	}
	if o.tsaKey == "" {
		o.tsaKey = kEC256
	}
	if o.notBefore.IsZero() {
		o.notBefore = time.Now().Add(-24 * time.Hour)
	}
	if o.notAfter.IsZero() {
		o.notAfter = time.Now().Add(24 * time.Hour)
	}
	k := &kit{rootKey: genKey(t, o.rootKey), tsaKey: genKey(t, o.tsaKey)}
	rootT := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test tsa root"},
		NotBefore: o.notBefore.Add(-time.Hour), NotAfter: o.notAfter.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SignatureAlgorithm: o.rootSigAlg,
	}
	der, err := x509.CreateCertificate(rand.Reader, rootT, rootT, k.rootKey.Public(), k.rootKey)
	if err != nil {
		t.Fatal(err)
	}
	k.root, _ = x509.ParseCertificate(der)
	tsaT := &x509.Certificate{
		SerialNumber: big.NewInt(0x1234567), Subject: pkix.Name{CommonName: "test tsa", Organization: []string{"bide"}},
		NotBefore: o.notBefore, NotAfter: o.notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		SignatureAlgorithm: o.rootSigAlg, DNSNames: o.dnsNames,
	}
	if o.keyUsage != 0 {
		tsaT.KeyUsage = o.keyUsage
	}
	if o.sanDirName != nil {
		tsaT.ExtraExtensions = append(tsaT.ExtraExtensions, pkix.Extension{Id: oidSAN, Value: seq(ctx(4, o.sanDirName))})
	}
	if o.tsaIsCA {
		tsaT.IsCA, tsaT.BasicConstraintsValid = true, true
	}
	if o.ski {
		tsaT.SubjectKeyId = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	}
	if !o.noEKU {
		ekus := o.eku
		if ekus == nil {
			ekus = []asn1.ObjectIdentifier{oidKPTimeStamping}
		}
		val, _ := asn1.Marshal(ekus)
		tsaT.ExtraExtensions = append(tsaT.ExtraExtensions, pkix.Extension{Id: oidExtKeyUsage, Critical: !o.ekuNotCritical, Value: val})
	}
	der, err = x509.CreateCertificate(rand.Reader, tsaT, k.root, k.tsaKey.Public(), k.rootKey)
	if err != nil {
		t.Fatal(err)
	}
	k.tsa, _ = x509.ParseCertificate(der)
	k.pool = x509.NewCertPool()
	k.pool.AddCert(k.root)
	return k
}

// tokSpec describes a token; the zero value plus defaults(kit) is valid. Each field breaks one rule.
type tokSpec struct {
	kit *kit

	// TSTInfo
	tstVersion int64
	policy     asn1.ObjectIdentifier
	imprintAlg asn1.ObjectIdentifier
	imprint    []byte
	serial     int64
	genTime    string // GeneralizedTime text; default from genAt
	genTag     byte
	accuracy   []byte // full DER, or nil
	ordering   bool
	nonce      *big.Int
	tstExt     []byte // full DER of [1] extensions, or nil
	tsaRaw     []byte // full DER of the [0] tsa GeneralName, or nil
	tstRaw     []byte // replaces the whole TSTInfo when non-nil

	// CMS
	eContentType asn1.ObjectIdentifier
	detached     bool
	sdVersion    int64
	digestOID    asn1.ObjectIdentifier // signer digestAlgorithm
	digestSet    []asn1.ObjectIdentifier
	sigOID       asn1.ObjectIdentifier
	sigParams    []byte // full DER of parameters, or nil (then NULL for RSA, none for EC)
	pss          bool
	signerCount  int
	siVersion    int64
	sidSKI       bool
	sidRaw       []byte // replaces the sid
	attrHook     func(attrs [][]byte) [][]byte
	keepOrder    bool
	noSignedAttr bool
	unsigned     []byte // full DER of the [1] unsignedAttrs
	certs        []*x509.Certificate
	extraCerts   [][]byte // raw elements appended to certificates
	crls         bool
	noCerts      bool
	sdExtra      []byte // appended inside SignedData after signerInfos
	essCertHash  []byte
	essHashAlg   asn1.ObjectIdentifier
	essIssuer    []byte // replaces issuer name in issuerSerial
	essSerial    *big.Int
	orderingRaw  []byte // replaces the ordering element
	essNoIssuer  bool
	essV1Only    bool
	sigFlip      bool
}

func (s *tokSpec) defaults() {
	if s.tstVersion == 0 {
		s.tstVersion = 1
	}
	if s.policy == nil {
		s.policy = asn1.ObjectIdentifier{1, 2, 3, 4}
	}
	if s.imprintAlg == nil {
		s.imprintAlg = oidSHA256
	}
	if s.serial == 0 {
		s.serial = 42
	}
	if s.genTag == 0 {
		s.genTag = 0x18
	}
	if s.eContentType == nil {
		s.eContentType = oidTSTInfo
	}
	if s.sdVersion == 0 {
		s.sdVersion = 3
	}
	if s.signerCount == 0 {
		s.signerCount = 1
	}
	kt := s.kit.tsaKey
	if s.digestOID == nil {
		switch kt.(type) {
		case ed25519.PrivateKey:
			s.digestOID = oidSHA512
		default:
			s.digestOID = oidSHA256
		}
	}
	if s.sigOID == nil {
		switch k := kt.(type) {
		case ed25519.PrivateKey:
			s.sigOID = oidEd25519
		case *rsa.PrivateKey:
			if s.pss {
				s.sigOID = oidRSAPSS
			} else {
				s.sigOID = oidSHA256WithRSA
			}
		case *ecdsa.PrivateKey:
			s.sigOID = oidECDSASHA256
			if k.Curve == elliptic.P384() {
				s.sigOID = oidECDSASHA384
				s.digestOID = oidSHA384
			}
		}
	}
	if s.certs == nil && !s.noCerts {
		s.certs = []*x509.Certificate{s.kit.tsa, s.kit.root}
	}
	if s.siVersion == 0 {
		if s.sidSKI {
			s.siVersion = 3
		} else {
			s.siVersion = 1
		}
	}
}

func hashOf(o asn1.ObjectIdentifier, data []byte) []byte {
	switch {
	case o.Equal(oidSHA384):
		h := sha512.Sum384(data)
		return h[:]
	case o.Equal(oidSHA512):
		h := sha512.Sum512(data)
		return h[:]
	}
	h := sha256.Sum256(data)
	return h[:]
}

func (s *tokSpec) tstInfo() []byte {
	if s.tstRaw != nil {
		return s.tstRaw
	}
	parts := [][]byte{intDER(s.tstVersion), oidDER(s.policy),
		seq(algNull(s.imprintAlg), octets(s.imprint)), intDER(s.serial),
		tlv(s.genTag, []byte(s.genTime))}
	if s.accuracy != nil {
		parts = append(parts, s.accuracy)
	}
	if s.ordering {
		parts = append(parts, []byte{1, 1, 0xff})
	}
	if s.orderingRaw != nil {
		parts = append(parts, s.orderingRaw)
	}
	if s.nonce != nil {
		parts = append(parts, bigDER(s.nonce))
	}
	if s.tsaRaw != nil {
		parts = append(parts, s.tsaRaw)
	}
	if s.tstExt != nil {
		parts = append(parts, s.tstExt)
	}
	return seq(parts...)
}

func (s *tokSpec) cert() *x509.Certificate { return s.kit.tsa }

func (s *tokSpec) essV2() []byte {
	c := s.cert()
	h := s.essCertHash
	if h == nil {
		sum := sha256.Sum256(c.Raw)
		h = sum[:]
	}
	parts := [][]byte{}
	if s.essHashAlg != nil {
		parts = append(parts, algNull(s.essHashAlg))
	}
	parts = append(parts, octets(h))
	if !s.essNoIssuer {
		iss := c.RawIssuer
		if s.essIssuer != nil {
			iss = s.essIssuer
		}
		ser := c.SerialNumber
		if s.essSerial != nil {
			ser = s.essSerial
		}
		parts = append(parts, seq(seq(ctx(4, iss)), bigDER(ser)))
	}
	return seq(oidDER(oidAttrSigCertV2), setOf(seq(seq(seq(parts...)))))
}

func (s *tokSpec) essV1() []byte {
	sum := sha1sum(s.cert().Raw)
	return seq(oidDER(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 12}),
		setOf(seq(seq(seq(octets(sum))))))
}

func (s *tokSpec) sid() []byte {
	if s.sidRaw != nil {
		return s.sidRaw
	}
	c := s.cert()
	if s.sidSKI {
		return ctxP(0, c.SubjectKeyId)
	}
	return seq(c.RawIssuer, bigDER(c.SerialNumber))
}

// build returns the token DER.
// build signs the token. It runs with enforcement off for the same reason as newKit: a negative
// test may sign with a 1024-bit RSA key. Verification is not wrapped.
func (s *tokSpec) build(t testing.TB) (tok []byte) {
	t.Helper()
	fips140.WithoutEnforcement(func() { tok = s.buildUnenforced(t) })
	return tok
}

func (s *tokSpec) buildUnenforced(t testing.TB) []byte {
	t.Helper()
	s.defaults()
	tst := s.tstInfo()
	attrs := [][]byte{
		seq(oidDER(oidAttrContent), setOf(oidDER(oidTSTInfo))),
		seq(oidDER(oidAttrDigest), setOf(octets(hashOf(s.digestOID, tst)))),
	}
	if s.essV1Only {
		attrs = append(attrs, s.essV1())
	} else {
		attrs = append(attrs, s.essV2())
	}
	if s.attrHook != nil {
		attrs = s.attrHook(attrs)
	}
	ordered := append([][]byte(nil), attrs...)
	if !s.keepOrder {
		sort.Slice(ordered, func(i, j int) bool { return string(ordered[i]) < string(ordered[j]) })
	}
	attrBody := []byte{}
	for _, a := range ordered {
		attrBody = append(attrBody, a...)
	}
	toSign := tlv(0x31, attrBody)
	sig := s.sign(t, toSign)
	if s.sigFlip {
		sig[len(sig)-1] ^= 1
	}
	sigAlg := s.sigAlg()
	siParts := [][]byte{intDER(s.siVersion), s.sid(), algNull(s.digestOID)}
	if s.digestOID.Equal(oidSHA256) || true {
		siParts[2] = algNull(s.digestOID)
	}
	if !s.noSignedAttr {
		siParts = append(siParts, tlv(0xA0, attrBody))
	}
	siParts = append(siParts, sigAlg, octets(sig))
	if s.unsigned != nil {
		siParts = append(siParts, s.unsigned)
	}
	si := seq(siParts...)
	sis := [][]byte{si}
	for i := 1; i < s.signerCount; i++ {
		sis = append(sis, si)
	}
	dset := s.digestSet
	if dset == nil {
		dset = []asn1.ObjectIdentifier{s.digestOID}
	}
	var dalgs [][]byte
	for _, d := range dset {
		dalgs = append(dalgs, algNull(d))
	}
	encap := seq(oidDER(s.eContentType), ctx(0, octets(tst)))
	if s.detached {
		encap = seq(oidDER(s.eContentType))
	}
	sd := [][]byte{intDER(s.sdVersion), setOf(dalgs...), encap}
	if !s.noCerts || len(s.extraCerts) > 0 {
		var cb []byte
		for _, c := range s.certs {
			cb = append(cb, c.Raw...)
		}
		for _, c := range s.extraCerts {
			cb = append(cb, c...)
		}
		sd = append(sd, tlv(0xA0, cb))
	}
	if s.crls {
		sd = append(sd, tlv(0xA1, seq()))
	}
	sd = append(sd, setOf(sis...))
	if s.sdExtra != nil {
		sd = append(sd, s.sdExtra)
	}
	return seq(oidDER(oidSignedData), ctx(0, seq(sd...)))
}

func (s *tokSpec) sigAlg() []byte {
	if s.sigParams != nil {
		return algID(s.sigOID, s.sigParams)
	}
	switch s.kit.tsaKey.(type) {
	case *rsa.PrivateKey:
		if s.sigOID.Equal(oidRSAPSS) {
			return algID(oidRSAPSS, pssParams(oidSHA256, 32))
		}
		return algNull(s.sigOID)
	}
	return algID(s.sigOID)
}

func pssParams(h asn1.ObjectIdentifier, salt int64) []byte {
	return seq(ctx(0, algNull(h)), ctx(1, algID(oidMGF1, algNull(h))), ctx(2, intDER(salt)))
}

func (s *tokSpec) sign(t testing.TB, data []byte) []byte {
	t.Helper()
	var h crypto.Hash
	switch {
	case s.sigOID.Equal(oidECDSASHA384), s.sigOID.Equal(oidSHA384WithRSA):
		h = crypto.SHA384
	case s.sigOID.Equal(oidECDSASHA512), s.sigOID.Equal(oidSHA512WithRSA):
		h = crypto.SHA512
	default:
		h = crypto.SHA256
	}
	switch k := s.kit.tsaKey.(type) {
	case ed25519.PrivateKey:
		return ed25519.Sign(k, data)
	case *ecdsa.PrivateKey:
		d := hashBytes(h, data)
		sig, err := ecdsa.SignASN1(rand.Reader, k, d)
		if err != nil {
			t.Fatal(err)
		}
		return sig
	case *rsa.PrivateKey:
		d := hashBytes(h, data)
		if s.sigOID.Equal(oidRSAPSS) {
			sig, err := rsa.SignPSS(rand.Reader, k, h, d, &rsa.PSSOptions{SaltLength: 32})
			if err != nil {
				t.Fatal(err)
			}
			return sig
		}
		sig, err := rsa.SignPKCS1v15(rand.Reader, k, h, d)
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	t.Fatal("unsupported key")
	return nil
}

// hashBytes hashes data. The negative tests build tokens that name SHA-1 on purpose, so the digest
// is computed with enforcement off: under GODEBUG=fips140=only crypto/sha1 panics otherwise.
func hashBytes(h crypto.Hash, data []byte) (sum []byte) {
	fips140.WithoutEnforcement(func() {
		w := h.New()
		w.Write(data)
		sum = w.Sum(nil)
	})
	return sum
}

func sha1sum(b []byte) []byte { return hashBytes(crypto.SHA1, b) }

// fixture: a default valid token for a head hashed to digest.
type fixture struct {
	kit    *kit
	digest []byte
	head   time.Time
	gen    time.Time
	spec   tokSpec
}

func genTimeText(t time.Time) string { return t.UTC().Format("20060102150405") + "Z" }

func newFixture(t testing.TB) *fixture {
	t.Helper()
	k := newKit(t, kitOpts{})
	d := sha256.Sum256([]byte("covering head"))
	now := time.Now().UTC().Truncate(time.Second)
	f := &fixture{kit: k, digest: d[:], head: now.Add(-time.Minute), gen: now}
	f.spec = tokSpec{kit: k, imprint: d[:], genTime: genTimeText(now)}
	return f
}

func (f *fixture) policy() Policy {
	return Policy{
		Roots:      f.kit.pool,
		CertStatus: func(*x509.Certificate, []*x509.Certificate, time.Time) error { return nil },
	}
}

func (f *fixture) expected() Expected {
	return Expected{Hash: crypto.SHA256, Digest: f.digest, HeadTime: f.head}
}

func mustRefuse(t *testing.T, tok []byte, exp Expected, pol Policy, want Reason) {
	t.Helper()
	res, err := Verify(tok, exp, pol)
	if err == nil {
		t.Fatalf("token accepted (%+v), want refusal %q", res, want)
	}
	var r *Refusal
	if !asRefusal(err, &r) {
		t.Fatalf("error %v is not a *Refusal", err)
	}
	if r.Reason != want {
		t.Fatalf("refused for %q (%s), want %q", r.Reason, r.Detail, want)
	}
	if res != nil {
		t.Fatalf("a refusal returned a result")
	}
}

func asRefusal(err error, r **Refusal) bool {
	for err != nil {
		if x, ok := err.(*Refusal); ok {
			*r = x
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func mustAccept(t *testing.T, tok []byte, exp Expected, pol Policy) *Result {
	t.Helper()
	res, err := Verify(tok, exp, pol)
	if err != nil {
		t.Fatalf("token refused: %v", err)
	}
	return res
}

var _ = fmt.Sprintf
