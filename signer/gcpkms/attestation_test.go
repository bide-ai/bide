package gcpkms

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// ---- the real fixture: a version 1 attestation and its chain from a real Cloud HSM-style chain ----

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/attestation/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureAttestation(t *testing.T) *kmspb.KeyOperationAttestation {
	t.Helper()
	gz, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(readTestdata(t, "peculiar-v1.att.gz.b64"))))
	if err != nil {
		t.Fatal(err)
	}
	return &kmspb.KeyOperationAttestation{
		Format:     kmspb.KeyOperationAttestation_CAVIUM_V1_COMPRESSED,
		Content:    gz,
		CertChains: &kmspb.KeyOperationAttestation_CertificateChains{CaviumCerts: []string{string(readTestdata(t, "peculiar-v1.chain.pem"))}},
	}
}

func TestBuiltInRootsArePinned(t *testing.T) {
	r, err := defaultAttestationRoots()
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		c    *x509.Certificate
		file string
		fp   string
	}{
		"owner":        {r.Owner, "google-owner-root.pem", googleOwnerRootSHA256},
		"manufacturer": {r.Manufacturer, "marvell-root.pem", marvellRootSHA256},
	} {
		sum := sha256.Sum256(tc.c.Raw)
		if hex.EncodeToString(sum[:]) != tc.fp {
			t.Errorf("%s: fingerprint %x", name, sum)
		}
		blk, _ := pem.Decode(readTestdata(t, tc.file))
		if blk == nil || !bytes.Equal(blk.Bytes, tc.c.Raw) {
			t.Errorf("%s differs from testdata/%s", name, tc.file)
		}
	}
	if r.Owner.Subject.CommonName != "Hawksbill Root v1 prod" || r.Manufacturer.Subject.CommonName != "localca.liquidsecurity.cavium.com" {
		t.Error("unexpected root subjects")
	}
	// A root that is not the pinned one is refused.
	if _, err := pinnedRoot(marvellRootPEM, googleOwnerRootSHA256); err == nil {
		t.Error("a root with another fingerprint was accepted")
	}
}

// The real chain and signature verify under the built-in roots, although the Marvell root expired in 2025.
func TestRealAttestationChainAndSignature(t *testing.T) {
	att := fixtureAttestation(t)
	roots, _ := defaultAttestationRoots()
	if time.Now().Before(roots.Manufacturer.NotAfter) {
		t.Log("the Marvell root has not expired yet; the expiry part of this test is vacuous")
	}
	content, err := gunzipLimited(att.Content)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := parseV1(content)
	if err != nil || len(objs) != 2 {
		t.Fatalf("objects %d, err %v", len(objs), err)
	}
	point := objs[0][attrECPoint]
	if len(point) != 65 || point[0] != 4 {
		t.Fatalf("point %x", point)
	}
	// The chain and signature are accepted; the key policy then rejects this object, which is a
	// public key object (extractable) of a key named app_key.
	err = verifyAttestation(att, "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1", point, roots)
	if !errors.Is(err, ErrAttestation) || !strings.Contains(err.Error(), "extractable") {
		t.Fatalf("err = %v", err)
	}
	// With the wrong public key the policy fails on the key.
	other := bytes.Clone(point)
	other[10] ^= 1
	if err := verifyAttestation(att, "v", other, roots); err == nil || !strings.Contains(err.Error(), "public key") {
		t.Fatalf("other key: %v", err)
	}
	// Roots swapped, the chain does not build.
	swapped := &AttestationRoots{Manufacturer: roots.Owner, Owner: roots.Manufacturer}
	if err := verifyAttestation(att, "v", point, swapped); !errors.Is(err, ErrAttestation) {
		t.Fatalf("swapped roots: %v", err)
	}
	// A flipped bit in the signed data fails the signature.
	bad := bytes.Clone(content)
	bad[100] ^= 1
	att2 := &kmspb.KeyOperationAttestation{Format: att.Format, Content: gz(t, bad), CertChains: att.CertChains}
	if err := verifyAttestation(att2, "v", point, roots); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("tampered content: %v", err)
	}
	// The chain with a certificate removed does not build; with one repeated it is refused.
	pemChain := string(readTestdata(t, "peculiar-v1.chain.pem"))
	blocks := strings.SplitAfter(pemChain, "-----END CERTIFICATE-----\n")
	for i := range blocks {
		if strings.TrimSpace(blocks[i]) == "" {
			continue
		}
		var rest []string
		for j, b := range blocks {
			if j != i {
				rest = append(rest, b)
			}
		}
		a := &kmspb.KeyOperationAttestation{Format: att.Format, Content: att.Content,
			CertChains: &kmspb.KeyOperationAttestation_CertificateChains{CaviumCerts: []string{strings.Join(rest, "")}}}
		if err := verifyAttestation(a, "v", point, roots); !errors.Is(err, ErrAttestation) {
			t.Errorf("chain without certificate %d: %v", i, err)
		}
	}
	a := &kmspb.KeyOperationAttestation{Format: att.Format, Content: att.Content,
		CertChains: &kmspb.KeyOperationAttestation_CertificateChains{CaviumCerts: []string{pemChain + blocks[0]}}}
	if err := verifyAttestation(a, "v", point, roots); err == nil || !strings.Contains(err.Error(), "not part of the chains") {
		t.Errorf("a repeated certificate: %v", err)
	}
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

// ---- a synthetic Cloud HSM: our own roots, card, partition and attestation, to test the policy ----

type testHSM struct {
	mfrRoot, ownRoot *x509.Certificate
	mfrRootKey       *rsa.PrivateKey
	ownRootKey       *rsa.PrivateKey
	partKey          *rsa.PrivateKey
	chains           *kmspb.KeyOperationAttestation_CertificateChains
	roots            *AttestationRoots
	mfrPart, ownPart *x509.Certificate
}

var (
	hsmOnce sync.Once
	hsm     *testHSM
)

func newTestHSM(t *testing.T) *testHSM {
	t.Helper()
	hsmOnce.Do(func() {
		must := func(err error) {
			if err != nil {
				panic(err)
			}
		}
		key := func() *rsa.PrivateKey {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			must(err)
			return k
		}
		h := &testHSM{mfrRootKey: key(), ownRootKey: key(), partKey: key()}
		cardKey := key()
		serial := int64(1)
		issue := func(subject string, pub *rsa.PublicKey, parent *x509.Certificate, parentKey *rsa.PrivateKey) *x509.Certificate {
			serial++
			tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: subject},
				NotBefore: time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), // expired on purpose
				SignatureAlgorithm: x509.SHA256WithRSA}
			if parent == nil {
				parent, parentKey = tmpl, nil
			}
			signer := parentKey
			if signer == nil {
				signer = h.mfrRootKey
				if subject == "Test Owner Root" {
					signer = h.ownRootKey
				}
			}
			der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
			must(err)
			c, err := x509.ParseCertificate(der)
			must(err)
			return c
		}
		h.mfrRoot = issue("Test Manufacturer Root", &h.mfrRootKey.PublicKey, nil, nil)
		h.ownRoot = issue("Test Owner Root", &h.ownRootKey.PublicKey, nil, nil)
		mfrCard := issue("Test Card", &cardKey.PublicKey, h.mfrRoot, h.mfrRootKey)
		h.mfrPart = issue("Test Mfr Partition", &h.partKey.PublicKey, mfrCard, cardKey)
		ownCard := issue("Test Owner Card", &cardKey.PublicKey, h.ownRoot, h.ownRootKey)
		h.ownPart = issue("Test Owner Partition", &h.partKey.PublicKey, h.ownRoot, h.ownRootKey)
		p := func(cs ...*x509.Certificate) []string {
			var out []string
			for _, c := range cs {
				out = append(out, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})))
			}
			return out
		}
		h.chains = &kmspb.KeyOperationAttestation_CertificateChains{
			CaviumCerts: p(h.mfrPart, mfrCard), GoogleCardCerts: p(ownCard), GooglePartitionCerts: p(h.ownPart)}
		h.roots = &AttestationRoots{Manufacturer: h.mfrRoot, Owner: h.ownRoot}
		hsm = h
	})
	return hsm
}

// attrs is an attested object under construction.
type attrs struct {
	order []uint32
	v     map[uint32][]byte
}

func goodAttrs(version string, pub []byte) *attrs {
	a := &attrs{v: map[uint32][]byte{}}
	h1 := sha256.Sum256([]byte("parent"))
	h2 := sha256.Sum256([]byte(version))
	a.set(0x0000, []byte{3}) // CKO_PRIVATE_KEY
	a.set(attrKeyType, []byte{ckkEC})
	a.set(attrExtractable, []byte{0})
	a.set(attrLocal, []byte{1})
	a.set(attrID, []byte(hex.EncodeToString(h1[:])+hex.EncodeToString(h2[:])))
	a.set(attrECPoint, pub)
	return a
}

func (a *attrs) set(tag uint32, v []byte) *attrs {
	if _, ok := a.v[tag]; !ok {
		a.order = append(a.order, tag)
	}
	a.v[tag] = v
	return a
}

func (a *attrs) del(tag uint32) *attrs {
	delete(a.v, tag)
	for i, t := range a.order {
		if t == tag {
			a.order = append(a.order[:i:i], a.order[i+1:]...)
			break
		}
	}
	return a
}

func (a *attrs) object() []byte {
	var body []byte
	for _, tag := range a.order {
		body = binary.BigEndian.AppendUint32(body, tag)
		body = binary.BigEndian.AppendUint32(body, uint32(len(a.v[tag])))
		body = append(body, a.v[tag]...)
	}
	o := binary.BigEndian.AppendUint32(nil, 7) // handle
	o = binary.BigEndian.AppendUint32(o, uint32(len(a.order)))
	o = binary.BigEndian.AppendUint32(o, uint32(12+len(body)))
	return append(o, body...)
}

func (h *testHSM) sign(t *testing.T, data []byte) []byte {
	t.Helper()
	d := sha256.Sum256(data)
	sig, err := rsa.SignPKCS1v15(rand.Reader, h.partKey, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// v1 builds a version 1 attestation of one or two objects, signed by the partition key.
func (h *testHSM) v1(t *testing.T, objs ...*attrs) []byte {
	t.Helper()
	var buf []byte
	first := 8
	var second int
	obj0 := objs[0].object()
	if len(objs) > 1 {
		second = first + len(obj0)
	}
	info := binary.BigEndian.AppendUint16(nil, 1)
	info = binary.BigEndian.AppendUint16(info, 3)
	info = binary.BigEndian.AppendUint16(info, uint16(first))
	info = binary.BigEndian.AppendUint16(info, uint16(second))
	buf = append(info, obj0...)
	if len(objs) > 1 {
		buf = append(buf, objs[1].object()...)
	}
	hdr := binary.BigEndian.AppendUint32(nil, 0)
	hdr = binary.BigEndian.AppendUint32(hdr, 3)
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(16+len(buf)+attestSigSize))
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(len(buf)))
	data := append(hdr, buf...)
	return append(data, h.sign(t, data)...)
}

// v2 builds a version 2 attestation: a 20-byte header and one object.
func (h *testHSM) v2(t *testing.T, o *attrs) []byte {
	t.Helper()
	data := append(make([]byte, 20), o.object()...)
	return append(data, h.sign(t, data)...)
}

func (h *testHSM) att(content []byte, format kmspb.KeyOperationAttestation_AttestationFormat) *kmspb.KeyOperationAttestation {
	return &kmspb.KeyOperationAttestation{Format: format, Content: content, CertChains: h.chains}
}

const (
	v1fmt = kmspb.KeyOperationAttestation_CAVIUM_V1_COMPRESSED
	v2fmt = kmspb.KeyOperationAttestation_CAVIUM_V2_COMPRESSED
)

func TestAttestationAccepted(t *testing.T) {
	h := newTestHSM(t)
	pub := bytes.Repeat([]byte{4}, 65)
	other := goodAttrs("another", bytes.Repeat([]byte{5}, 65))
	pubObj := goodAttrs(testVersion, pub).set(0x0000, []byte{2}).set(attrExtractable, []byte{1}) // a public key object, extractable
	for name, att := range map[string]*kmspb.KeyOperationAttestation{
		"v1":                       h.att(gz(t, h.v1(t, goodAttrs(testVersion, pub))), v1fmt),
		"v1 matching second":       h.att(gz(t, h.v1(t, other, goodAttrs(testVersion, pub))), v1fmt),
		"v1 public object first":   h.att(gz(t, h.v1(t, pubObj, goodAttrs(testVersion, pub))), v1fmt),
		"v2":                       h.att(gz(t, h.v2(t, goodAttrs(testVersion, pub))), v2fmt),
		"upper-case key ID digest": h.att(gz(t, h.v1(t, goodAttrs(testVersion, pub).set(attrID, []byte(strings.Repeat("A", 64)+strings.ToUpper(sha256hex(testVersion)))))), v1fmt),
	} {
		if err := verifyAttestation(att, testVersion, pub, h.roots); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestAttestationPolicyRefusals(t *testing.T) {
	h := newTestHSM(t)
	pub := bytes.Repeat([]byte{4}, 65)
	good := func() *attrs { return goodAttrs(testVersion, pub) }
	cases := map[string]struct {
		a    *attrs
		want string
	}{
		"extractable":           {good().set(attrExtractable, []byte{1}), "extractable"},
		"extractable missing":   {good().del(attrExtractable), "extractable"},
		"extractable malformed": {good().set(attrExtractable, []byte{2}), "extractable"},
		"extractable two bytes": {good().set(attrExtractable, []byte{0, 0}), "extractable"},
		"imported":              {good().set(attrLocal, []byte{0}), "imported"},
		"local missing":         {good().del(attrLocal), "imported"},
		"other point":           {good().set(attrECPoint, bytes.Repeat([]byte{9}, 65)), "public key"},
		"no point":              {good().del(attrECPoint), "public key"},
		"RSA key type":          {good().set(attrKeyType, []byte{0}), "not an EC key"},
		"no key type":           {good().del(attrKeyType), "not an EC key"},
		"another version ID":    {good().set(attrID, []byte(strings.Repeat("a", 64)+sha256hex("projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/9"))), "key ID"},
		"first digest only":     {good().set(attrID, []byte(sha256hex(testVersion)+strings.Repeat("a", 64))), "key ID"},
		"short ID":              {good().set(attrID, []byte("abc")), "key ID"},
		"no ID":                 {good().del(attrID), "key ID"},
	}
	for name, tc := range cases {
		att := h.att(gz(t, h.v1(t, tc.a)), v1fmt)
		err := verifyAttestation(att, testVersion, pub, h.roots)
		if !errors.Is(err, ErrAttestation) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", name, err)
		}
		att = h.att(gz(t, h.v2(t, tc.a)), v2fmt)
		if err := verifyAttestation(att, testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
			t.Errorf("%s (v2): err = %v", name, err)
		}
	}
	// None of three objects may borrow the point from one and the flags from another.
	split1 := good().set(attrExtractable, []byte{1})
	split2 := good().set(attrECPoint, bytes.Repeat([]byte{7}, 65))
	if err := verifyAttestation(h.att(gz(t, h.v1(t, split1, split2)), v1fmt), testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
		t.Errorf("properties combined across objects: %v", err)
	}
}

func TestAttestationStructureRefusals(t *testing.T) {
	h := newTestHSM(t)
	pub := bytes.Repeat([]byte{4}, 65)
	good := h.v1(t, goodAttrs(testVersion, pub))
	resign := func(body []byte) []byte { return append(bytes.Clone(body), h.sign(t, body)...) }
	body := good[:len(good)-attestSigSize]

	t.Run("absent", func(t *testing.T) {
		for _, a := range []*kmspb.KeyOperationAttestation{nil, {}, {Format: v1fmt}} {
			if err := verifyAttestation(a, testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
				t.Errorf("%v: %v", a, err)
			}
		}
	})
	t.Run("formats", func(t *testing.T) {
		for _, f := range []kmspb.KeyOperationAttestation_AttestationFormat{kmspb.KeyOperationAttestation_ATTESTATION_FORMAT_UNSPECIFIED, 77} {
			if err := verifyAttestation(h.att(gz(t, good), f), testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
				t.Errorf("format %d: %v", f, err)
			}
		}
		// A v1 blob declared v2 and the reverse do not parse as the other.
		if err := verifyAttestation(h.att(gz(t, good), v2fmt), testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
			t.Errorf("v1 as v2: %v", err)
		}
	})
	t.Run("not gzip", func(t *testing.T) {
		if err := verifyAttestation(h.att(good, v1fmt), testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
			t.Errorf("%v", err)
		}
	})
	t.Run("gzip bomb", func(t *testing.T) {
		bomb := gz(t, make([]byte, maxAttestationSize+1))
		if err := verifyAttestation(h.att(bomb, v1fmt), testVersion, pub, h.roots); err == nil || !strings.Contains(err.Error(), "uncompressed") {
			t.Errorf("%v", err)
		}
	})
	t.Run("short", func(t *testing.T) {
		if err := verifyAttestation(h.att(gz(t, make([]byte, 100)), v1fmt), testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
			t.Errorf("%v", err)
		}
	})
	t.Run("signature by another key", func(t *testing.T) {
		wrong, _ := rsa.GenerateKey(rand.Reader, 2048)
		d := sha256.Sum256(body)
		sig, _ := rsa.SignPKCS1v15(rand.Reader, wrong, crypto.SHA256, d[:])
		if err := verifyAttestation(h.att(gz(t, append(bytes.Clone(body), sig...)), v1fmt), testVersion, pub, h.roots); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Errorf("%v", err)
		}
	})
	t.Run("tampered after signing", func(t *testing.T) {
		bad := bytes.Clone(good)
		bad[len(bad)-attestSigSize-5] ^= 1
		if err := verifyAttestation(h.att(gz(t, bad), v1fmt), testVersion, pub, h.roots); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Errorf("%v", err)
		}
	})
	t.Run("every truncation and every bit flip of a signed body fails cleanly", func(t *testing.T) {
		// The signature is re-made so the structure check, not the signature, is what refuses.
		for n := 0; n < len(body); n++ {
			if err := verifyAttestation(h.att(gz(t, resign(body[:n])), v1fmt), testVersion, pub, h.roots); err == nil {
				t.Fatalf("a body truncated to %d bytes verified", n)
			}
		}
		for i := 0; i < len(body); i += 7 {
			b := bytes.Clone(body)
			b[i] ^= 0x80
			// May still verify when the flipped byte is one the policy does not read; it must not panic.
			verifyAttestation(h.att(gz(t, resign(b)), v1fmt), testVersion, pub, h.roots)
			b2 := h.v2(t, goodAttrs(testVersion, pub))
			b2 = b2[:len(b2)-attestSigSize]
			if i < len(b2) {
				c := bytes.Clone(b2)
				c[i] ^= 0x80
				verifyAttestation(h.att(gz(t, resign(c)), v2fmt), testVersion, pub, h.roots)
			}
		}
	})
	t.Run("huge counts and lengths", func(t *testing.T) {
		a := goodAttrs(testVersion, pub)
		obj := a.object()
		binary.BigEndian.PutUint32(obj[4:], 0xffffffff) // attribute count
		w := append(binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, 1), 3), 8), 0), obj...)
		hdr := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 0), 3), uint32(16+len(w)+attestSigSize)), uint32(len(w)))
		data := append(hdr, w...)
		if err := verifyAttestation(h.att(gz(t, resign(data)), v1fmt), testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
			t.Errorf("count: %v", err)
		}
		o2 := a.object()
		// first attribute's length field is at 12+4
		binary.BigEndian.PutUint32(o2[16:], 0xfffffff0)
		w2 := append(binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, 1), 3), 8), 0), o2...)
		hdr2 := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 0), 3), uint32(16+len(w2)+attestSigSize)), uint32(len(w2)))
		if err := verifyAttestation(h.att(gz(t, resign(append(hdr2, w2...))), v1fmt), testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
			t.Errorf("length: %v", err)
		}
	})
	t.Run("duplicate attribute", func(t *testing.T) {
		a := goodAttrs(testVersion, pub)
		a.order = append(a.order, attrExtractable) // written twice
		if err := verifyAttestation(h.att(gz(t, h.v2(t, a)), v2fmt), testVersion, pub, h.roots); err == nil || !strings.Contains(err.Error(), "twice") {
			// the builder uses the map value for both; the count then has a duplicate tag
			t.Errorf("%v", err)
		}
	})
}

func TestAttestationChainRefusals(t *testing.T) {
	h := newTestHSM(t)
	pub := bytes.Repeat([]byte{4}, 65)
	content := gz(t, h.v1(t, goodAttrs(testVersion, pub)))
	chains := func(c *kmspb.KeyOperationAttestation_CertificateChains) *kmspb.KeyOperationAttestation {
		return &kmspb.KeyOperationAttestation{Format: v1fmt, Content: content, CertChains: c}
	}
	full := h.chains
	cp := func() *kmspb.KeyOperationAttestation_CertificateChains {
		return &kmspb.KeyOperationAttestation_CertificateChains{
			CaviumCerts: append([]string(nil), full.CaviumCerts...), GoogleCardCerts: append([]string(nil), full.GoogleCardCerts...),
			GooglePartitionCerts: append([]string(nil), full.GooglePartitionCerts...)}
	}
	otherRoot := func() *x509.Certificate { // a self-signed look-alike root
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: h.mfrRoot.Subject, SignatureAlgorithm: x509.SHA256WithRSA}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
		c, _ := x509.ParseCertificate(der)
		return c
	}
	if err := verifyAttestation(chains(cp()), testVersion, pub, h.roots); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	drop := func(c *kmspb.KeyOperationAttestation_CertificateChains, list string) {
		switch list {
		case "cavium":
			c.CaviumCerts = c.CaviumCerts[1:]
		case "card":
			c.GoogleCardCerts = nil
		case "partition":
			c.GooglePartitionCerts = nil
		}
	}
	for _, l := range []string{"cavium", "card", "partition"} {
		c := cp()
		drop(c, l)
		if err := verifyAttestation(chains(c), testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
			t.Errorf("without %s: %v", l, err)
		}
	}
	if err := verifyAttestation(chains(nil), testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
		t.Errorf("no chains: %v", err)
	}
	// Roots the chains do not lead to: a look-alike with the same subject, and each root alone swapped.
	for name, r := range map[string]*AttestationRoots{
		"look-alike manufacturer root": {Manufacturer: otherRoot(), Owner: h.ownRoot},
		"look-alike owner root":        {Manufacturer: h.mfrRoot, Owner: otherRoot()},
		"swapped":                      {Manufacturer: h.ownRoot, Owner: h.mfrRoot},
		"missing manufacturer":         {Owner: h.ownRoot},
		"missing owner":                {Manufacturer: h.mfrRoot},
	} {
		if err := verifyAttestation(chains(cp()), testVersion, pub, r); !errors.Is(err, ErrAttestation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := verifyAttestation(chains(cp()), testVersion, pub, nil); !errors.Is(err, ErrAttestation) {
		t.Errorf("nil roots: %v", err)
	}
	// An owner partition certificate for another key (a rogue partition) is refused.
	rogue, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(99), Subject: pkix.Name{CommonName: "rogue"}, SignatureAlgorithm: x509.SHA256WithRSA}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, h.ownRoot, &rogue.PublicKey, h.ownRootKey)
	c := cp()
	c.GooglePartitionCerts = []string{string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	if err := verifyAttestation(chains(c), testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
		t.Errorf("owner partition with another key: %v", err)
	}
	// An attestation signed by the partition key but with the manufacturer chain only (no owner
	// signature possible): both partitions must verify, and here the owner chain is simply absent.
	c = cp()
	c.GoogleCardCerts, c.GooglePartitionCerts = nil, nil
	if err := verifyAttestation(chains(c), testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
		t.Errorf("manufacturer chain only: %v", err)
	}
	// Garbage in the chain fields.
	for name, s := range map[string]string{"not pem": "hello", "bad der": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2}})),
		"wrong type": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1}})), "trailing": full.CaviumCerts[0] + "junk",
		"huge": strings.Repeat("x", maxCertPEMSize+1)} {
		c := cp()
		c.CaviumCerts = append([]string{s}, c.CaviumCerts...)
		if err := verifyAttestation(chains(c), testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Too many certificates.
	c = cp()
	for range maxChainCerts {
		c.CaviumCerts = append(c.CaviumCerts, full.CaviumCerts[0])
	}
	if err := verifyAttestation(chains(c), testVersion, pub, h.roots); !errors.Is(err, ErrAttestation) {
		t.Errorf("too many: %v", err)
	}
	// The chains verify although every certificate has expired (validity is not enforced).
	if time.Now().Before(h.mfrRoot.NotAfter) {
		t.Fatal("the test PKI was meant to be expired")
	}
}

// ---- Open with VerifyAttestation ----

func hsmFake(t *testing.T, h *testHSM, mod func(*attrs)) *fake {
	t.Helper()
	f := newP256(t, kmspb.ProtectionLevel_HSM)
	pubAny, err := audit.ECDSAP256PublicKeyFromSPKI(mustSPKI(t, f))
	if err != nil {
		t.Fatal(err)
	}
	a := goodAttrs(testVersion, pubAny)
	if mod != nil {
		mod(a)
	}
	f.attestation = h.att(gz(t, h.v1(t, a)), v1fmt)
	return f
}

func mustSPKI(t *testing.T, f *fake) []byte {
	t.Helper()
	blk, _ := pem.Decode([]byte(spkiPEM(&f.ec.PublicKey)))
	return blk.Bytes
}

func TestOpenVerifyAttestation(t *testing.T) {
	h := newTestHSM(t)
	ctx := context.Background()
	cfg := func(f *fake) Config {
		return Config{Client: f, KeyVersion: testVersion, VerifyAttestation: true, AttestationRoots: h.roots}
	}
	f := hsmFake(t, h, nil)
	s, err := Open(ctx, cfg(f))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := audit.VerifierOf(s); v == nil {
		t.Fatal("no verifier")
	}
	// Without VerifyAttestation nothing is checked.
	g := hsmFake(t, h, func(a *attrs) { a.set(attrExtractable, []byte{1}) })
	if _, err := Open(ctx, Config{Client: g, KeyVersion: testVersion}); err != nil {
		t.Fatalf("attestation checked without being asked: %v", err)
	}
	g = hsmFake(t, h, func(a *attrs) { a.set(attrExtractable, []byte{1}) })
	if _, err := Open(ctx, cfg(g)); !errors.Is(err, ErrAttestation) {
		t.Fatalf("extractable key: %v", err)
	}
	// A refused Open signs nothing.
	if g.signs.Load() != 0 {
		t.Error("signed before the attestation was verified")
	}
	// The attestation of another key version.
	other := hsmFake(t, h, func(a *attrs) {
		a.set(attrID, []byte(strings.Repeat("0", 64)+sha256hex("projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/2")))
	})
	if _, err := Open(ctx, cfg(other)); !errors.Is(err, ErrAttestation) {
		t.Fatalf("another version's attestation: %v", err)
	}
	// The attestation of a different public key.
	f2 := hsmFake(t, h, nil)
	f3 := newP256(t, kmspb.ProtectionLevel_HSM)
	f3.attestation = f2.attestation
	if _, err := Open(ctx, cfg(f3)); !errors.Is(err, ErrAttestation) {
		t.Fatalf("another key's attestation: %v", err)
	}
	// No attestation at all.
	none := newP256(t, kmspb.ProtectionLevel_HSM)
	if _, err := Open(ctx, cfg(none)); !errors.Is(err, ErrAttestation) {
		t.Fatalf("no attestation: %v", err)
	}
	// A software key has none, and Ed25519 is software only.
	if _, err := Open(ctx, Config{Client: newP256(t, kmspb.ProtectionLevel_SOFTWARE), KeyVersion: testVersion, AllowSoftwareKey: true, VerifyAttestation: true, AttestationRoots: h.roots}); !errors.Is(err, ErrAttestation) {
		t.Fatalf("software key: %v", err)
	}
	if _, err := Open(ctx, Config{Client: newEd(t), KeyVersion: testVersion, Scheme: audit.AlgEd25519, AllowSoftwareKey: true, VerifyAttestation: true, AttestationRoots: h.roots}); !errors.Is(err, ErrAttestation) {
		t.Fatalf("Ed25519: %v", err)
	}
	// The built-in roots do not accept a synthetic chain.
	if _, err := Open(ctx, Config{Client: f, KeyVersion: testVersion, VerifyAttestation: true}); !errors.Is(err, ErrAttestation) {
		t.Fatalf("built-in roots with a synthetic chain: %v", err)
	}
	// Roots without VerifyAttestation are a configuration error: nothing would be verified.
	if _, err := Open(ctx, Config{Client: f, KeyVersion: testVersion, AttestationRoots: h.roots}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("roots without VerifyAttestation: %v", err)
	}
}
