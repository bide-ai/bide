package rfc3161

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"math/big"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAcceptsDefaultToken(t *testing.T) {
	f := newFixture(t)
	tok := f.spec.build(t)
	res := mustAccept(t, tok, f.expected(), f.policy())
	if !res.GenTime.Equal(f.gen) {
		t.Errorf("genTime %v, want %v", res.GenTime, f.gen)
	}
	if !bytes.Equal(res.Imprint, f.digest) || res.Hash != crypto.SHA256 {
		t.Errorf("imprint %x / %v", res.Imprint, res.Hash)
	}
	if res.Signer == nil || !res.Signer.Equal(f.kit.tsa) || len(res.Chain) != 2 {
		t.Errorf("signer/chain wrong: %v %d", res.Signer, len(res.Chain))
	}
	if res.PolicyOID != "1.2.3.4" || res.Serial.Int64() != 42 {
		t.Errorf("policy %q serial %v", res.PolicyOID, res.Serial)
	}
}

func TestAcceptedVariants(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cases := []struct {
		name string
		kit  kitOpts
		mut  func(*tokSpec)
		pol  func(*Policy)
	}{
		{"p384-sha384", kitOpts{tsaKey: kEC384}, nil, nil},
		{"ed25519", kitOpts{tsaKey: kEd25519}, nil, nil},
		{"rsa3072", kitOpts{tsaKey: kRSA3072}, nil, nil},
		{"rsa3072-pss", kitOpts{tsaKey: kRSA3072}, func(s *tokSpec) { s.pss = true }, nil},
		{"rsa2048-with-exception", kitOpts{tsaKey: kRSA2048}, nil, func(p *Policy) { p.RSA2048Until = now.Add(time.Hour) }},
		{"ski-signer-id", kitOpts{ski: true}, func(s *tokSpec) { s.sidSKI = true }, nil},
		{"no-issuer-serial-in-ess", kitOpts{}, func(s *tokSpec) { s.essNoIssuer = true }, nil},
		{"unsigned-attrs-ignored", kitOpts{}, func(s *tokSpec) { s.unsigned = ctx(1, seq(oidDER(asn1.ObjectIdentifier{1, 2, 3}), setOf(intDER(1)))) }, nil},
		{"v1-and-v2-signing-cert", kitOpts{}, func(s *tokSpec) {
			s.attrHook = func(a [][]byte) [][]byte { return append(a, s.essV1()) }
		}, nil},
		{"ignored-extra-signed-attr", kitOpts{}, func(s *tokSpec) {
			s.attrHook = func(a [][]byte) [][]byte {
				return append(a, seq(oidDER(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 5}), setOf(tlv(0x17, []byte("261015155957Z")))))
			}
		}, nil},
		{"fraction-genTime", kitOpts{}, func(s *tokSpec) { s.genTime = genTimeText(now)[:14] + ".25Z" }, nil},
		{"non-critical-tstinfo-extension", kitOpts{}, func(s *tokSpec) {
			s.tstExt = ctx(1, seq(oidDER(asn1.ObjectIdentifier{1, 2, 3, 9}), octets([]byte{5, 0})))
		}, nil},
		{"ordering-true-and-nonce", kitOpts{}, func(s *tokSpec) { s.ordering = true; s.nonce = big.NewInt(77) }, nil},
		{"token-without-certs-signer-from-policy", kitOpts{}, func(s *tokSpec) { s.noCerts = true }, func(p *Policy) {}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newKit(t, c.kit)
			f := newFixture(t)
			f.kit = k
			f.spec.kit = k
			if c.mut != nil {
				c.mut(&f.spec)
			}
			pol := f.policy()
			if c.name == "token-without-certs-signer-from-policy" {
				pol.Intermediates = []*x509.Certificate{k.tsa}
			}
			if c.pol != nil {
				c.pol(&pol)
			}
			tok := f.spec.build(t)
			res := mustAccept(t, tok, f.expected(), pol)
			if c.name == "fraction-genTime" && res.GenTime.Nanosecond() != 250_000_000 {
				t.Errorf("fraction lost: %v", res.GenTime)
			}
		})
	}
}

func TestNonceMatching(t *testing.T) {
	f := newFixture(t)
	f.spec.nonce = big.NewInt(1234567890123)
	tok := f.spec.build(t)
	exp := f.expected()
	exp.Nonce = big.NewInt(1234567890123)
	mustAccept(t, tok, exp, f.policy())
	exp.Nonce = big.NewInt(5)
	mustRefuse(t, tok, exp, f.policy(), ReasonNonceMismatch)

	f2 := newFixture(t) // token without a nonce when one is expected
	exp = f2.expected()
	exp.Nonce = big.NewInt(1)
	mustRefuse(t, f2.spec.build(t), exp, f2.policy(), ReasonNonceMismatch)
}

func TestGenTimeAgainstHead(t *testing.T) {
	f := newFixture(t)
	f.head = f.gen.Add(10 * time.Second) // head made 10s after genTime
	tok := f.spec.build(t)
	mustRefuse(t, tok, f.expected(), f.policy(), ReasonGenTimeBeforeHead)

	pol := f.policy()
	pol.ClockTolerance = 10 * time.Second
	mustAccept(t, tok, f.expected(), pol) // exactly at the edge
	pol.ClockTolerance = 9 * time.Second
	mustRefuse(t, tok, f.expected(), pol, ReasonGenTimeBeforeHead)

	// accuracy is added to the tolerance
	f.spec.accuracy = seq(intDER(1), ctxP(0, []byte{0x01, 0xf4})) // 1 s + 500 ms
	tok = f.spec.build(t)
	pol.MaxAccuracy = 2 * time.Second
	pol.ClockTolerance = 8500 * time.Millisecond
	mustAccept(t, tok, f.expected(), pol)
	pol.ClockTolerance = 8400 * time.Millisecond
	mustRefuse(t, tok, f.expected(), pol, ReasonGenTimeBeforeHead)
}

func TestConfigErrors(t *testing.T) {
	f := newFixture(t)
	tok := f.spec.build(t)
	for name, fn := range map[string]func(*Expected, *Policy){
		"no status hook":     func(e *Expected, p *Policy) { p.CertStatus = nil },
		"no head time":       func(e *Expected, p *Policy) { e.HeadTime = time.Time{} },
		"sha1 expected":      func(e *Expected, p *Policy) { e.Hash = crypto.SHA1 },
		"wrong digest len":   func(e *Expected, p *Policy) { e.Digest = e.Digest[:20] },
		"negative tolerance": func(e *Expected, p *Policy) { p.ClockTolerance = -1 },
		"huge size cap":      func(e *Expected, p *Policy) { p.MaxTokenBytes = 2 << 20 },
	} {
		t.Run(name, func(t *testing.T) {
			e, p := f.expected(), f.policy()
			fn(&e, &p)
			_, err := Verify(tok, e, p)
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("err = %v, want ErrConfig", err)
			}
			if errors.Is(err, ErrRefused) {
				t.Fatal("a config error is not a refusal")
			}
		})
	}
	// A nil root pool is a refusal, not a fallback to the system roots.
	p := f.policy()
	p.Roots = nil
	mustRefuse(t, tok, f.expected(), p, ReasonNoTrustAnchors)
}

// derLen rewrites the outer SEQUENCE header of tok with the given length bytes.
func withOuterLen(tok []byte, lenBytes []byte) []byte {
	// outer header: 0x30 then 0x82 hi lo (tokens here are 256..65535 bytes)
	body := tok[4:]
	return append(append([]byte{0x30}, lenBytes...), body...)
}

func TestEncodingRefusals(t *testing.T) {
	f := newFixture(t)
	tok := f.spec.build(t)
	if tok[1] != 0x82 {
		t.Fatalf("test assumes a 2-byte length, got %x", tok[:4])
	}
	cases := []struct {
		name string
		tok  []byte
		want Reason
	}{
		{"trailing byte", append(append([]byte(nil), tok...), 0), ReasonTrailingBytes},
		{"trailing token", append(append([]byte(nil), tok...), tok...), ReasonTrailingBytes},
		{"indefinite length (BER)", append(append([]byte{0x30, 0x80}, tok[4:]...), 0, 0), ReasonNotDER},
		{"non-minimal length", withOuterLen(tok, []byte{0x83, 0x00, tok[2], tok[3]}), ReasonNotDER},
		{"long form for short length", withOuterLen(tok, []byte{0x82, 0x00, 0x05}), ReasonNotDER},
		{"truncated", tok[:len(tok)-3], ReasonNotDER},
		{"empty", nil, ReasonNotDER},
		{"not a sequence", []byte{0x04, 0x01, 0x00}, ReasonMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustRefuse(t, c.tok, f.expected(), f.policy(), c.want) })
	}
}

func TestStructureRefusals(t *testing.T) { runNeg(t, structureCases) }

var structureCases = []negCase{
	{"two signer infos", kitOpts{}, func(s *tokSpec) { s.signerCount = 2 }, ReasonSignerCount},
	{"two digest algorithms", kitOpts{}, func(s *tokSpec) { s.digestSet = []asn1.ObjectIdentifier{oidSHA256, oidSHA384} }, ReasonDigestAlgs},
	{"digest set differs from signer", kitOpts{}, func(s *tokSpec) { s.digestSet = []asn1.ObjectIdentifier{oidSHA384} }, ReasonDigestAlgs},
	{"signed data version 1", kitOpts{}, func(s *tokSpec) { s.sdVersion = 1 }, ReasonSignedDataVersion},
	{"content type id-data", kitOpts{}, func(s *tokSpec) { s.eContentType = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1} }, ReasonContentType},
	{"detached content", kitOpts{}, func(s *tokSpec) { s.detached = true }, ReasonDetached},
	{"crls present", kitOpts{}, func(s *tokSpec) { s.crls = true }, ReasonCRLs},
	{"attribute certificate", kitOpts{}, func(s *tokSpec) { s.extraCerts = [][]byte{tlv(0xA1, seq())} }, ReasonAttrCerts},
	{"unknown signer identifier", kitOpts{}, func(s *tokSpec) { s.sidRaw = tlv(0x82, []byte{1}) }, ReasonSignerID},
	{"signer version 3 with issuer and serial", kitOpts{}, func(s *tokSpec) { s.siVersion = 3 }, ReasonSignerVersion},
	{"signer version 2", kitOpts{}, func(s *tokSpec) { s.siVersion = 2 }, ReasonSignerVersion},
	{"no signed attributes", kitOpts{}, func(s *tokSpec) { s.noSignedAttr = true }, ReasonNoSignedAttrs},
	{"trailing bytes inside signed data", kitOpts{}, func(s *tokSpec) { s.sdExtra = tlv(0x05, nil) }, ReasonMalformed},
	{"malformed unsigned attributes", kitOpts{}, func(s *tokSpec) { s.unsigned = ctx(1, []byte{0x30, 0x05, 0x01}) }, ReasonNotDER},
	{"unsigned attributes not a set of sequences", kitOpts{}, func(s *tokSpec) { s.unsigned = ctx(1, tlv(0x04, []byte{1})) }, ReasonMalformed},
	{"signer version 1 with a subject key id", kitOpts{}, func(s *tokSpec) { s.sidSKI = true; s.siVersion = 1 }, ReasonSignerVersion},
	{"unknown signer identifier with version 2", kitOpts{}, func(s *tokSpec) { s.sidRaw = tlv(0x82, []byte{1}); s.siVersion = 2 }, ReasonSignerVersion},
	{"issuerAndSerialNumber with three elements", kitOpts{}, func(s *tokSpec) {
		c := s.kit.tsa
		s.sidRaw = seq(c.RawIssuer, bigDER(c.SerialNumber), intDER(1))
	}, ReasonMalformed},
	{"tstinfo trailing bytes", kitOpts{}, func(s *tokSpec) {
		s.defaults()
		s.tstRaw = append(s.tstInfo(), 0)
	}, ReasonTrailingBytes},
	{"ordering FALSE spelled out", kitOpts{}, func(s *tokSpec) { s.orderingRaw = []byte{1, 1, 0} }, ReasonNotDER},
	{"ordering with a non-DER true", kitOpts{}, func(s *tokSpec) { s.orderingRaw = []byte{1, 1, 1} }, ReasonNotDER},
	{"accuracy seconds that overflow to zero", kitOpts{}, func(s *tokSpec) { s.accuracy = seq(bigDER(new(big.Int).Lsh(big.NewInt(1), 55))) }, ReasonAccuracy},
	{"accuracy above a day by its millis", kitOpts{}, func(s *tokSpec) { s.accuracy = seq(intDER(86400), ctxP(0, []byte{0x01})) }, ReasonAccuracy},
	{"accuracy millis not minimal", kitOpts{}, func(s *tokSpec) { s.accuracy = seq(ctxP(0, []byte{0x00, 0x05})) }, ReasonNotDER},
	{"accuracy millis zero", kitOpts{}, func(s *tokSpec) { s.accuracy = seq(ctxP(0, []byte{0x00})) }, ReasonAccuracy},
	{"genTime with a ten-digit fraction", kitOpts{}, func(s *tokSpec) { s.genTime = base14(s) + ".0000000001Z" }, ReasonGenTimeFormat},
	{"genTime without Z", kitOpts{}, func(s *tokSpec) { s.genTime = base14(s) + "X" }, ReasonGenTimeFormat},
	{"genTime on a day that does not exist", kitOpts{}, func(s *tokSpec) { s.genTime = "20260230120000Z" }, ReasonGenTimeFormat},
	{"genTime at hour 24", kitOpts{}, func(s *tokSpec) { s.genTime = "20261015240000Z" }, ReasonGenTimeFormat},
	{"genTime in month 13", kitOpts{}, func(s *tokSpec) { s.genTime = "20261301120000Z" }, ReasonGenTimeFormat},
	{"tstinfo version 2", kitOpts{}, func(s *tokSpec) { s.tstVersion = 2 }, ReasonTSTVersion},
	{"critical tstinfo extension", kitOpts{}, func(s *tokSpec) {
		s.tstExt = ctx(1, seq(oidDER(asn1.ObjectIdentifier{1, 2, 3, 9}), []byte{1, 1, 0xff}, octets([]byte{5, 0})))
	}, ReasonCriticalExtension},
	{"explicit false critical flag (not DER)", kitOpts{}, func(s *tokSpec) {
		s.tstExt = ctx(1, seq(oidDER(asn1.ObjectIdentifier{1, 2, 3, 9}), []byte{1, 1, 0}, octets([]byte{5, 0})))
	}, ReasonNotDER},
	{"accuracy beyond a day", kitOpts{}, func(s *tokSpec) { s.accuracy = seq(intDER(90000)) }, ReasonAccuracy},
	{"negative accuracy", kitOpts{}, func(s *tokSpec) { s.accuracy = seq(intDER(-1)) }, ReasonAccuracy},
	{"genTime as UTCTime", kitOpts{}, func(s *tokSpec) { s.genTag = 0x17 }, ReasonGenTimeFormat},
	{"genTime with trailing zero fraction", kitOpts{}, func(s *tokSpec) { s.genTime = base14(s) + ".50Z" }, ReasonGenTimeFormat},
	{"genTime with zone offset", kitOpts{}, func(s *tokSpec) { s.genTime = base14(s) + "+0100" }, ReasonGenTimeFormat},
	{"genTime without seconds", kitOpts{}, func(s *tokSpec) { s.genTime = base14(s)[:12] + "Z" }, ReasonGenTimeFormat},
	{"genTime with ten fraction digits", kitOpts{}, func(s *tokSpec) { s.genTime = base14(s) + ".1234567891Z" }, ReasonGenTimeFormat},
	{"genTime with impossible date", kitOpts{}, func(s *tokSpec) { s.genTime = "20261340000000Z" }, ReasonGenTimeFormat},
}

func TestSignedAttributeRefusals(t *testing.T) { runNeg(t, attrCases) }

func dropAttr(oid asn1.ObjectIdentifier) func(*tokSpec) {
	return func(s *tokSpec) {
		s.attrHook = func(a [][]byte) [][]byte {
			var out [][]byte
			for _, x := range a {
				if !bytes.Contains(x[:min(len(x), 16)], oidDER(oid)[2:]) {
					out = append(out, x)
				}
			}
			return out
		}
	}
}
func dupAttr(oid asn1.ObjectIdentifier) func(*tokSpec) {
	return func(s *tokSpec) {
		s.attrHook = func(a [][]byte) [][]byte {
			for _, x := range a {
				if bytes.Contains(x[:min(len(x), 16)], oidDER(oid)[2:]) {
					return append(a, x)
				}
			}
			return a
		}
	}
}

var attrCases = []negCase{
	{"missing contentType", kitOpts{}, dropAttr(oidAttrContent), ReasonMissingAttr},
	{"missing messageDigest", kitOpts{}, dropAttr(oidAttrDigest), ReasonMissingAttr},
	{"missing signingCertificateV2", kitOpts{}, dropAttr(oidAttrSigCertV2), ReasonMissingAttr},
	{"signingCertificate (SHA-1) only", kitOpts{}, func(s *tokSpec) { s.essV1Only = true }, ReasonESSV1Only},
	{"duplicate contentType", kitOpts{}, dupAttr(oidAttrContent), ReasonDuplicateAttr},
	{"duplicate messageDigest", kitOpts{}, dupAttr(oidAttrDigest), ReasonDuplicateAttr},
	{"duplicate signingCertificateV2", kitOpts{}, dupAttr(oidAttrSigCertV2), ReasonDuplicateAttr},
	{"signed attributes not in DER order", kitOpts{}, func(s *tokSpec) {
		s.keepOrder = true
		s.attrHook = func(a [][]byte) [][]byte {
			// content type attribute sorts first; put it last
			return [][]byte{a[1], a[2], a[0]}
		}
	}, ReasonSignedAttrsOrder},
	{"contentType attribute names another type", kitOpts{}, func(s *tokSpec) {
		s.attrHook = func(a [][]byte) [][]byte {
			a[0] = seq(oidDER(oidAttrContent), setOf(oidDER(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1})))
			return a
		}
	}, ReasonAttrContentType},
	{"wrong messageDigest", kitOpts{}, func(s *tokSpec) {
		s.attrHook = func(a [][]byte) [][]byte {
			a[1] = seq(oidDER(oidAttrDigest), setOf(octets(bytes.Repeat([]byte{7}, 32))))
			return a
		}
	}, ReasonMessageDigest},
	{"messageDigest with two values", kitOpts{}, func(s *tokSpec) {
		s.attrHook = func(a [][]byte) [][]byte {
			a[1] = seq(oidDER(oidAttrDigest), setOf(octets(hashOf(oidSHA256, s.tstInfo())), octets(hashOf(oidSHA256, s.tstInfo()))))
			return a
		}
	}, ReasonMalformed},
	{"ESS certificate hash differs", kitOpts{}, func(s *tokSpec) { s.essCertHash = bytes.Repeat([]byte{1}, 32) }, ReasonESSMismatch},
	{"ESS issuer differs", kitOpts{}, func(s *tokSpec) {
		s.essIssuer = seq(tlv(0x31, seq(oidDER(asn1.ObjectIdentifier{2, 5, 4, 3}), tlv(0x0c, []byte("other")))))
	}, ReasonESSMismatch},
	{"ESS certHash of the wrong length", kitOpts{}, func(s *tokSpec) { s.essCertHash = bytes.Repeat([]byte{1}, 31) }, ReasonMalformed},
	{"ESS issuerSerial serial differs", kitOpts{}, func(s *tokSpec) { s.essSerial = big.NewInt(99) }, ReasonESSMismatch},
	{"ignored attribute holds a constructed OCTET STRING (BER)", kitOpts{}, func(s *tokSpec) {
		s.attrHook = func(a [][]byte) [][]byte {
			return append(a, seq(oidDER(asn1.ObjectIdentifier{1, 2, 3}), setOf(tlv(0x24, octets([]byte{1})))))
		}
	}, ReasonNotDER},
	{"ESS hash is SHA-1", kitOpts{}, func(s *tokSpec) { s.essHashAlg = oidSHA1; s.essCertHash = sha1sum(s.kit.tsa.Raw) }, ReasonWeakHash},
	{"ESS hash is SHA-256 spelled out (DER default)", kitOpts{}, func(s *tokSpec) { s.essHashAlg = oidSHA256 }, ReasonNotDER},
	{"ESS hash is MD5", kitOpts{}, func(s *tokSpec) { s.essHashAlg = oidMD5; s.essCertHash = make([]byte, 16) }, ReasonWeakHash},
	{"ESS hash is SHA-224", kitOpts{}, func(s *tokSpec) { s.essHashAlg = oidSHA224; s.essCertHash = make([]byte, 28) }, ReasonWeakHash},
}

func TestAlgorithmRefusals(t *testing.T) { runNeg(t, algCases) }

var algCases = []negCase{
	{"signer digest SHA-1", kitOpts{}, func(s *tokSpec) { s.digestOID = oidSHA1; s.digestSet = nil }, ReasonWeakHash},
	{"signer digest MD5", kitOpts{}, func(s *tokSpec) { s.digestOID = oidMD5 }, ReasonWeakHash},
	{"signer digest SHA-224", kitOpts{}, func(s *tokSpec) { s.digestOID = oidSHA224 }, ReasonWeakHash},
	{"signer digest unknown", kitOpts{}, func(s *tokSpec) { s.digestOID = asn1.ObjectIdentifier{1, 2, 3} }, ReasonUnsupportedAlgorithm},
	{"ecdsa with SHA-1", kitOpts{}, func(s *tokSpec) { s.sigOID = oidECDSASHA1 }, ReasonWeakHash},
	{"ecdsa with SHA-224", kitOpts{}, func(s *tokSpec) { s.sigOID = oidECDSASHA224 }, ReasonWeakHash},
	{"ecdsa digest differs from signer digest", kitOpts{}, func(s *tokSpec) { s.sigOID = oidECDSASHA384 }, ReasonUnsupportedAlgorithm},
	{"rsa with SHA-1", kitOpts{tsaKey: kRSA3072}, func(s *tokSpec) { s.sigOID = oidSHA1WithRSA }, ReasonWeakHash},
	{"rsa with MD5", kitOpts{tsaKey: kRSA3072}, func(s *tokSpec) { s.sigOID = oidMD5WithRSA }, ReasonWeakHash},
	{"rsa with SHA-224", kitOpts{tsaKey: kRSA3072}, func(s *tokSpec) { s.sigOID = oidSHA224WithRSA }, ReasonWeakHash},
	{"rsa signature algorithm on an EC key", kitOpts{}, func(s *tokSpec) { s.sigOID = oidSHA256WithRSA }, ReasonUnsupportedAlgorithm},
	{"unknown signature algorithm", kitOpts{}, func(s *tokSpec) { s.sigOID = asn1.ObjectIdentifier{1, 2, 3, 4} }, ReasonUnsupportedAlgorithm},
	{"pss with SHA-1", kitOpts{tsaKey: kRSA3072}, func(s *tokSpec) {
		s.pss = true
		s.sigParams = pssParams(oidSHA1, 20)
	}, ReasonWeakHash},
	{"pss hashes both SHA-384 under a SHA-256 signer digest", kitOpts{tsaKey: kRSA3072}, func(s *tokSpec) {
		s.pss = true
		s.sigParams = pssParams(oidSHA384, 48)
	}, ReasonUnsupportedAlgorithm},
	{"rsa 1024 signer", kitOpts{tsaKey: kRSA1024}, nil, ReasonWeakKey},
	{"pss with mismatched mgf hash", kitOpts{tsaKey: kRSA3072}, func(s *tokSpec) {
		s.pss = true
		s.sigParams = seq(ctx(0, algNull(oidSHA256)), ctx(1, algID(oidMGF1, algNull(oidSHA384))), ctx(2, intDER(32)))
	}, ReasonUnsupportedAlgorithm},
	{"ed25519 with SHA-256 digest", kitOpts{tsaKey: kEd25519}, func(s *tokSpec) { s.digestOID = oidSHA256 }, ReasonUnsupportedAlgorithm},
	{"rsa 2048 signer", kitOpts{tsaKey: kRSA2048}, nil, ReasonWeakKey},
	{"rsa 2048 CA", kitOpts{rootKey: kRSA2048}, nil, ReasonWeakKey},
	{"p-521 signer", kitOpts{tsaKey: kEC521}, nil, ReasonWeakKey},
	{"signature bit flipped", kitOpts{}, func(s *tokSpec) { s.sigFlip = true }, ReasonBadSignature},
	{"rsa signature bit flipped", kitOpts{tsaKey: kRSA3072}, func(s *tokSpec) { s.sigFlip = true }, ReasonBadSignature},
	{"ed25519 signature bit flipped", kitOpts{tsaKey: kEd25519}, func(s *tokSpec) { s.sigFlip = true }, ReasonBadSignature},
}

func TestRSA2048ExceptionIsBoundedByGenTime(t *testing.T) {
	f := newFixture(t)
	f.kit = newKit(t, kitOpts{tsaKey: kRSA2048})
	f.spec.kit = f.kit
	tok := f.spec.build(t)
	pol := f.policy()
	pol.RSA2048Until = f.gen.Add(-time.Second)
	mustRefuse(t, tok, f.expected(), pol, ReasonWeakKey)
	pol.RSA2048Until = f.gen.Add(time.Second)
	mustAccept(t, tok, f.expected(), pol)
}

func TestImprintRefusals(t *testing.T) { runNeg(t, imprintCases) }

var imprintCases = []negCase{
	{"wrong imprint", kitOpts{}, func(s *tokSpec) { s.imprint = bytes.Repeat([]byte{9}, 32) }, ReasonImprintMismatch},
	{"imprint of another length", kitOpts{}, func(s *tokSpec) { s.imprint = append([]byte(nil), s.imprint...)[:min(31, len(s.imprint))] }, ReasonImprintMismatch},
	{"imprint algorithm differs from expected", kitOpts{}, func(s *tokSpec) {
		s.imprintAlg = oidSHA384
		s.imprint = make([]byte, 48)
	}, ReasonImprintMismatch},
	{"imprint labelled SHA-384 but holding the expected SHA-256 bytes", kitOpts{}, func(s *tokSpec) { s.imprintAlg = oidSHA384 }, ReasonImprintMismatch},
	{"imprint algorithm SHA-1", kitOpts{}, func(s *tokSpec) { s.imprintAlg = oidSHA1; s.imprint = make([]byte, 20) }, ReasonWeakHash},
	{"imprint algorithm MD5", kitOpts{}, func(s *tokSpec) { s.imprintAlg = oidMD5; s.imprint = make([]byte, 16) }, ReasonWeakHash},
	{"imprint algorithm unknown", kitOpts{}, func(s *tokSpec) { s.imprintAlg = asn1.ObjectIdentifier{1, 2, 3} }, ReasonUnsupportedAlgorithm},
}

func TestSignerCertificateRefusals(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		kit  kitOpts
		want Reason
	}{
		{"no extended key usage", kitOpts{noEKU: true}, ReasonTSAEKU},
		{"EKU not critical", kitOpts{ekuNotCritical: true}, ReasonTSAEKU},
		{"EKU is server auth only", kitOpts{eku: []asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 1}}}, ReasonTSAEKU},
		{"extra EKU beside time stamping", kitOpts{eku: []asn1.ObjectIdentifier{oidKPTimeStamping, {1, 3, 6, 1, 5, 5, 7, 3, 1}}}, ReasonTSAEKU},
		{"time stamping listed twice", kitOpts{eku: []asn1.ObjectIdentifier{oidKPTimeStamping, oidKPTimeStamping}}, ReasonTSAEKU},
		{"any extended key usage", kitOpts{eku: []asn1.ObjectIdentifier{{2, 5, 29, 37, 0}}}, ReasonTSAEKU},
		{"expired at genTime", kitOpts{notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(-24 * time.Hour)}, ReasonTSACertExpired},
		{"not yet valid at genTime", kitOpts{notBefore: now.Add(24 * time.Hour), notAfter: now.Add(48 * time.Hour)}, ReasonTSACertExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.kit = newKit(t, c.kit)
			f.spec.kit = f.kit
			mustRefuse(t, f.spec.build(t), f.expected(), f.policy(), c.want)
		})
	}

	t.Run("expired now but valid at genTime is accepted", func(t *testing.T) {
		f := newFixture(t)
		f.gen = now.Add(-36 * time.Hour).UTC().Truncate(time.Second)
		f.head = f.gen.Add(-time.Minute)
		f.kit = newKit(t, kitOpts{notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(-24 * time.Hour)})
		f.spec.kit = f.kit
		f.spec.genTime = genTimeText(f.gen)
		mustAccept(t, f.spec.build(t), f.expected(), f.policy())
	})
	t.Run("unlisted TSA", func(t *testing.T) {
		f := newFixture(t)
		other := newKit(t, kitOpts{})
		p := f.policy()
		p.Roots = other.pool
		mustRefuse(t, f.spec.build(t), f.expected(), p, ReasonUntrustedTSA)
	})
	t.Run("revoked TSA certificate", func(t *testing.T) {
		f := newFixture(t)
		sentinel := errors.New("revoked at genTime")
		p := f.policy()
		var gotAt time.Time
		p.CertStatus = func(c *x509.Certificate, chain []*x509.Certificate, at time.Time) error {
			gotAt = at
			if !c.Equal(f.kit.tsa) || len(chain) != 2 {
				t.Errorf("hook got wrong certificates")
			}
			return sentinel
		}
		_, err := Verify(f.spec.build(t), f.expected(), p)
		var r *Refusal
		if !asRefusal(err, &r) || r.Reason != ReasonTSAStatus || !errors.Is(err, sentinel) {
			t.Fatalf("err = %v", err)
		}
		if !gotAt.Equal(f.gen) {
			t.Errorf("status checked at %v, want genTime %v", gotAt, f.gen)
		}
	})
	t.Run("signer certificate absent", func(t *testing.T) {
		f := newFixture(t)
		f.spec.noCerts = true
		mustRefuse(t, f.spec.build(t), f.expected(), f.policy(), ReasonSignerCertNotFound)
	})
	t.Run("signer id names a different certificate", func(t *testing.T) {
		f := newFixture(t)
		f.spec.certs = []*x509.Certificate{f.kit.root}
		mustRefuse(t, f.spec.build(t), f.expected(), f.policy(), ReasonSignerCertNotFound)
	})
	t.Run("intermediate is a CA with another EKU", func(t *testing.T) {
		// chain EKU nesting is enforced through x509 Verify with the time-stamping usage
		f := newFixture(t)
		mustAccept(t, f.spec.build(t), f.expected(), f.policy())
	})
}

func TestLimits(t *testing.T) {
	f := newFixture(t)
	tok := f.spec.build(t)
	p := f.policy()
	p.MaxTokenBytes = len(tok) - 1
	mustRefuse(t, tok, f.expected(), p, ReasonTooLarge)
	p.MaxTokenBytes = len(tok)
	mustAccept(t, tok, f.expected(), p)

	big := make([]byte, DefaultMaxTokenBytes+1)
	mustRefuse(t, big, f.expected(), f.policy(), ReasonTooLarge)

	f.spec.extraCerts = nil
	f.spec.certs = nil
	f.spec.defaults()
	f.spec.certs = []*x509.Certificate{f.kit.tsa, f.kit.root}
	p = f.policy()
	p.MaxCerts = 1
	mustRefuse(t, f.spec.build(t), f.expected(), p, ReasonTooManyCerts)

	t.Run("deep nesting in an ignored attribute", func(t *testing.T) {
		f := newFixture(t)
		deep := intDER(1)
		for i := 0; i < 40; i++ {
			deep = seq(deep)
		}
		f.spec.attrHook = func(a [][]byte) [][]byte {
			return append(a, seq(oidDER(asn1.ObjectIdentifier{1, 2, 3}), setOf(deep)))
		}
		mustRefuse(t, f.spec.build(t), f.expected(), f.policy(), ReasonTooDeep)
	})
}

// A length field that claims far more than the input holds must not make the verifier allocate.
func TestLyingLengthsDoNotAllocate(t *testing.T) {
	f := newFixture(t)
	tok := f.spec.build(t)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < 200; i++ {
		m := append([]byte(nil), tok...)
		// claim a 2 GiB body at the start of every structure header found in the first 60 bytes
		for j := 0; j+5 < 60; j++ {
			if m[j] == 0x30 || m[j] == 0x31 || m[j] == 0xA0 || m[j] == 0x04 {
				c := append([]byte(nil), m...)
				c[j+1] = 0x84
				c = append(c[:j+2], append([]byte{0x7f, 0xff, 0xff, 0xff}, c[j+2:]...)...)
				_, _ = Verify(c, f.expected(), f.policy())
			}
		}
		_, _ = Verify(m, f.expected(), f.policy())
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 512<<20 {
		t.Fatalf("allocated %d MiB for lying lengths", grew>>20)
	}
	for _, huge := range [][]byte{
		{0x30, 0x84, 0xff, 0xff, 0xff, 0xff},
		{0x30, 0x88, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		{0x30, 0x84, 0x7f, 0xff, 0xff, 0xff, 0x01},
	} {
		if _, err := Verify(huge, f.expected(), f.policy()); err == nil {
			t.Fatal("accepted a lying length")
		}
	}
}

func TestErrorsAreRefusals(t *testing.T) {
	f := newFixture(t)
	f.spec.sigFlip = true
	_, err := Verify(f.spec.build(t), f.expected(), f.policy())
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err %v does not match ErrRefused", err)
	}
	if ReasonBadSignature.OutsideProfile() || !ReasonCRLs.OutsideProfile() {
		t.Error("OutsideProfile classification wrong")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Error("error text spans lines")
	}
}

// A certificate that does not parse is refused even when it is not the signer's: the token is held
// to DER throughout, and OpenSSL rejects such a token too (found by the differential test).
func TestUnparsableCertificateRefused(t *testing.T) {
	f := newFixture(t)
	bad := append([]byte(nil), f.kit.root.Raw...)
	bad[10] ^= 0xff
	f.spec.extraCerts = [][]byte{bad}
	mustRefuse(t, f.spec.build(t), f.expected(), f.policy(), ReasonMalformed)
}

// The 2048-bit exception never reaches below 2048.
func TestRSA2048ExceptionDoesNotCover1024(t *testing.T) {
	f := newFixture(t)
	f.kit = newKit(t, kitOpts{tsaKey: kRSA1024})
	f.spec.kit = f.kit
	pol := f.policy()
	pol.RSA2048Until = f.gen.Add(time.Hour)
	mustRefuse(t, f.spec.build(t), f.expected(), pol, ReasonWeakKey)
}

// A certificate on the path signed with SHA-1 is refused by name.
func TestSHA1SignedPathCertificateRefused(t *testing.T) {
	f := newFixture(t)
	f.kit = newKit(t, kitOpts{rootKey: kRSA3072, rootSigAlg: x509.SHA1WithRSA})
	f.spec.kit = f.kit
	if f.kit.tsa.SignatureAlgorithm != x509.SHA1WithRSA {
		t.Skip("this Go version does not sign certificates with SHA-1")
	}
	mustRefuse(t, f.spec.build(t), f.expected(), f.policy(), ReasonWeakHash)
}

// negCase is one token with exactly one fault, the Reason that must refuse it, and the kit it is
// built with. The same cases are replayed against OpenSSL.
type negCase struct {
	name string
	kit  kitOpts
	mut  func(*tokSpec)
	want Reason
}

// build returns the fixture and token for the case.
func (c negCase) build(t testing.TB) (*fixture, []byte) {
	f := newFixture(t)
	f.kit = newKit(t, c.kit)
	f.spec.kit = f.kit
	if c.mut != nil {
		c.mut(&f.spec)
	}
	return f, f.spec.build(t)
}

func runNeg(t *testing.T, cases []negCase) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, tok := c.build(t)
			mustRefuse(t, tok, f.expected(), f.policy(), c.want)
		})
	}
}

func dirName(cn string) []byte {
	return seq(setOf(seq(oidDER(asn1.ObjectIdentifier{2, 5, 4, 3}), tlv(0x0c, []byte(cn)))))
}

// pkiCases are the faults of the PKI assessment: the tsa name, the key usage and CA flag of the TSA
// certificate, a disagreeing SHA-1 signingCertificate, and repeated PSS parameters.
var pkiCases = []negCase{
	{"tsa directoryName differs from the certificate", kitOpts{}, func(s *tokSpec) { s.tsaRaw = ctx(0, ctx(4, dirName("someone else"))) }, ReasonTSAName},
	{"tsa dNSName differs from the SAN", kitOpts{dnsNames: []string{"tsa.example.test"}}, func(s *tokSpec) { s.tsaRaw = ctx(0, tlv(0x82, []byte("other.example.test"))) }, ReasonTSAName},
	{"tsa directoryName differs from the SAN directoryName", kitOpts{sanDirName: dirName("san name")}, func(s *tokSpec) { s.tsaRaw = ctx(0, ctx(4, dirName("not it"))) }, ReasonTSAName},
	{"tsa dNSName absent from the certificate", kitOpts{}, func(s *tokSpec) { s.tsaRaw = ctx(0, tlv(0x82, []byte("tsa.example.test"))) }, ReasonTSAName},
	{"tsa name of an unsupported form", kitOpts{}, func(s *tokSpec) {
		s.tsaRaw = ctx(0, ctx(0, oidDER(asn1.ObjectIdentifier{1, 2, 3}), ctx(0, octets([]byte{1}))))
	}, ReasonTSAName},
	{"key usage keyCertSign only", kitOpts{keyUsage: x509.KeyUsageCertSign}, nil, ReasonTSAKeyUsage},
	{"key usage keyEncipherment only", kitOpts{keyUsage: x509.KeyUsageKeyEncipherment}, nil, ReasonTSAKeyUsage},
	{"key usage signature plus keyEncipherment", kitOpts{keyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment}, nil, ReasonTSAKeyUsage},
	{"TSA certificate is a CA", kitOpts{tsaIsCA: true}, nil, ReasonTSAKeyUsage},
	{"signingCertificate names another certificate", kitOpts{}, func(s *tokSpec) {
		s.attrHook = func(a [][]byte) [][]byte {
			return append(a, seq(oidDER(oidSigningCertificateV1), setOf(seq(seq(seq(octets(bytes.Repeat([]byte{3}, 20))))))))
		}
	}, ReasonESSMismatch},
	{"signingCertificate twice", kitOpts{}, func(s *tokSpec) {
		s.attrHook = func(a [][]byte) [][]byte { return append(a, s.essV1(), s.essV1()) }
	}, ReasonDuplicateAttr},
	{"PSS parameters repeat the salt length", kitOpts{tsaKey: kRSA3072}, func(s *tokSpec) {
		s.pss = true
		s.sigParams = seq(ctx(0, algNull(oidSHA256)), ctx(1, algID(oidMGF1, algNull(oidSHA256))), ctx(2, intDER(32)), ctx(2, intDER(32)))
	}, ReasonMalformed},
	{"PSS parameters out of order", kitOpts{tsaKey: kRSA3072}, func(s *tokSpec) {
		s.pss = true
		s.sigParams = seq(ctx(1, algID(oidMGF1, algNull(oidSHA256))), ctx(0, algNull(oidSHA256)), ctx(2, intDER(32)))
	}, ReasonMalformed},
}

func TestPKIAssessmentRefusals(t *testing.T) { runNeg(t, pkiCases) }

func TestPKIAssessmentAccepted(t *testing.T) {
	cases := []struct {
		name string
		kit  kitOpts
		mut  func(*tokSpec)
	}{
		{"tsa directoryName equals the subject", kitOpts{}, func(s *tokSpec) { s.tsaRaw = ctx(0, ctx(4, s.kit.tsa.RawSubject)) }},
		{"tsa dNSName in the SAN", kitOpts{dnsNames: []string{"tsa.example.test"}}, func(s *tokSpec) { s.tsaRaw = ctx(0, tlv(0x82, []byte("TSA.example.test"))) }},
		{"tsa directoryName equals a SAN directoryName", kitOpts{sanDirName: dirName("san name")}, func(s *tokSpec) { s.tsaRaw = ctx(0, ctx(4, dirName("san name"))) }},
		{"key usage nonRepudiation only", kitOpts{keyUsage: x509.KeyUsageContentCommitment}, nil},
		{"key usage signature and certSign", kitOpts{keyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.kit = newKit(t, c.kit)
			f.spec.kit = f.kit
			if c.mut != nil {
				c.mut(&f.spec)
			}
			mustAccept(t, f.spec.build(t), f.expected(), f.policy())
		})
	}
}

func TestAccuracyCap(t *testing.T) {
	f := newFixture(t)
	f.spec.accuracy = seq(intDER(1))
	tok := f.spec.build(t)
	mustAccept(t, tok, f.expected(), f.policy()) // exactly the default cap
	f.spec.accuracy = seq(intDER(1), ctxP(0, []byte{0x01}))
	tok = f.spec.build(t)
	mustRefuse(t, tok, f.expected(), f.policy(), ReasonAccuracy)
	p := f.policy()
	p.MaxAccuracy = 2 * time.Second
	res := mustAccept(t, tok, f.expected(), p)
	if res.Accuracy != 1001*time.Millisecond || !res.Latest().Equal(res.GenTime.Add(1001*time.Millisecond)) {
		t.Errorf("accuracy %v latest %v", res.Accuracy, res.Latest())
	}
	p.MaxAccuracy = -1
	mustRefuse(t, tok, f.expected(), p, ReasonAccuracy)
	p.MaxAccuracy = 25 * time.Hour
	mustRefuse(t, tok, f.expected(), p, ReasonAccuracy)
	// the cap bounds the back-dating window: an accuracy cannot widen it past the cap
	f.head = f.gen.Add(time.Hour)
	f.spec.accuracy = seq(intDER(1))
	mustRefuse(t, f.spec.build(t), f.expected(), f.policy(), ReasonGenTimeBeforeHead)
}

func TestAcceptedPolicies(t *testing.T) {
	f := newFixture(t)
	tok := f.spec.build(t)
	p := f.policy()
	p.AcceptedPolicies = []asn1.ObjectIdentifier{{1, 2, 3, 4}}
	mustAccept(t, tok, f.expected(), p)
	p.AcceptedPolicies = []asn1.ObjectIdentifier{{1, 2, 3, 5}, {1, 2, 3}}
	mustRefuse(t, tok, f.expected(), p, ReasonPolicy)
}

func TestDistrustAfter(t *testing.T) {
	f := newFixture(t)
	tok := f.spec.build(t)
	p := f.policy()
	p.DistrustAfter = []Distrust{{Cert: f.kit.tsa, After: f.gen.Add(time.Second)}}
	mustAccept(t, tok, f.expected(), p) // distrusted only after genTime
	p.DistrustAfter = []Distrust{{Cert: f.kit.tsa, After: f.gen}}
	mustRefuse(t, tok, f.expected(), p, ReasonTSADistrusted)
	p.DistrustAfter = []Distrust{{Cert: f.kit.root, After: f.gen.Add(-time.Hour)}}
	mustRefuse(t, tok, f.expected(), p, ReasonTSADistrusted) // an anchor retired earlier covers its descendants
	p.DistrustAfter = []Distrust{{Cert: newKit(t, kitOpts{}).tsa, After: f.gen.Add(-time.Hour)}}
	mustAccept(t, tok, f.expected(), p) // another key
}

func TestGenTimeNotInTheFuture(t *testing.T) {
	f := newFixture(t)
	tok := f.spec.build(t)
	e := f.expected()
	e.Now = f.gen.Add(-time.Minute)
	mustRefuse(t, tok, e, f.policy(), ReasonGenTimeFuture)
	p := f.policy()
	p.ClockTolerance = time.Minute
	mustAccept(t, tok, e, p)
	e.Now = f.gen
	mustAccept(t, tok, e, f.policy())
	// the stated accuracy counts in the token's favour: genTime less accuracy is not after Now
	f.spec.accuracy = seq(intDER(1))
	tok = f.spec.build(t)
	e.Now = f.gen.Add(-time.Second)
	mustAccept(t, tok, e, f.policy())
	e.Now = f.gen.Add(-time.Second - time.Millisecond)
	mustRefuse(t, tok, e, f.policy(), ReasonGenTimeFuture)
}

func TestStatusUnknownRefuses(t *testing.T) {
	f := newFixture(t)
	p := f.policy()
	p.CertStatus = StatusUnknown
	mustRefuse(t, f.spec.build(t), f.expected(), p, ReasonTSAStatus)
}

// base14 is the YYYYMMDDHHMMSS part of the spec's genTime (a stand-in when another fault shortened it).
func base14(s *tokSpec) string {
	if len(s.genTime) >= 14 {
		return s.genTime[:14]
	}
	return "20261015120000"
}
