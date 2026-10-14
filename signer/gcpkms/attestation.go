package gcpkms

import (
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"

	"cloud.google.com/go/kms/apiv1/kmspb"
)

// ErrAttestation means Config.VerifyAttestation is set and the key version's HSM attestation does not
// prove what it must: the attestation is absent, in an unknown format, not signed through chains that
// lead to the pinned roots, or does not show this key version's public key as an HSM-generated,
// non-extractable EC key. The error says which check failed.
var ErrAttestation = errors.New("gcpkms: the key version's HSM attestation was not verified")

// Limits on what a (signed, but still untrusted until verified) attestation may ask the parser to do.
const (
	maxAttestationSize = 1 << 20 // decompressed
	maxChainCerts      = 16
	maxCertPEMSize     = 16 << 10
	maxAttributes      = 256
	attestSigSize      = 256 // an RSA-2048 PKCS#1 v1.5 signature over the attestation, at its end
)

// PKCS#11 / Marvell attribute tags and values the policy reads.
const (
	attrKeyType     = 0x0100
	attrID          = 0x0102
	attrECPoint     = 0x0120 // the EC public point for an EC key (Marvell's OBJ_ATTR_MODULUS)
	attrExtractable = 0x0162
	attrLocal       = 0x0163
	ckkEC           = 0x03
)

func attErr(format string, args ...any) error {
	return fmt.Errorf("gcpkms: attestation: "+format+": %w", append(args, ErrAttestation)...)
}

// verifyAttestation checks the HSM attestation of a key version, following Google's reference
// verifier (kms/attestations/verify_attestation_chains.py in python-docs-samples) and then reading
// the attested attributes:
//
//  1. the manufacturer chain root -> card -> partition and the owner chain (root -> card, root ->
//     partition, with the same public keys as the manufacturer's) are built from the pinned roots and
//     the certificates in the attestation; any other certificate is refused;
//  2. the attestation is signed (RSA-2048, PKCS#1 v1.5, SHA-256, the last 256 bytes) by both
//     partition certificates;
//  3. one attested EC object (the key's private-key object) has the public point pub, is not extractable, was generated on the HSM
//     (not imported), and its key ID carries the SHA-256 of the key version's resource name.
//
// Validity periods of the certificates are not checked, as in Google's verifier: the attestation
// states a fact about when the key was created, the HSM certificates are long lived, and a root that
// has expired (Marvell's did on 2025-11-16) does not make the statement false. There is no
// revocation.
func verifyAttestation(att *kmspb.KeyOperationAttestation, version string, pub []byte, roots *AttestationRoots) error {
	if att == nil || len(att.GetContent()) == 0 {
		return attErr("the key version has no attestation (only HSM key versions do)")
	}
	switch att.GetFormat() {
	case kmspb.KeyOperationAttestation_CAVIUM_V1_COMPRESSED, kmspb.KeyOperationAttestation_CAVIUM_V2_COMPRESSED:
	default:
		return attErr("unsupported format %s", att.GetFormat())
	}
	if roots == nil || roots.Manufacturer == nil || roots.Owner == nil {
		return attErr("both trust roots are required")
	}
	content, err := gunzipLimited(att.GetContent())
	if err != nil {
		return attErr("content: %v", err)
	}
	if len(content) <= attestSigSize {
		return attErr("content is shorter than its signature")
	}
	mfrPart, ownPart, err := buildChains(att.GetCertChains(), roots)
	if err != nil {
		return err
	}
	data, sig := content[:len(content)-attestSigSize], content[len(content)-attestSigSize:]
	for name, c := range map[string]*x509.Certificate{"manufacturer partition": mfrPart, "owner partition": ownPart} {
		if err := verifyRSASignature(c, data, sig); err != nil {
			return attErr("the %s certificate does not verify the attestation signature", name)
		}
	}
	objs, err := parseObjects(content)
	if err != nil {
		return attErr("%v", err)
	}
	return checkObjects(objs, version, pub)
}

func gunzipLimited(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	out, err := io.ReadAll(io.LimitReader(r, maxAttestationSize+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxAttestationSize {
		return nil, fmt.Errorf("larger than %d bytes uncompressed", maxAttestationSize)
	}
	return out, nil
}

func verifyRSASignature(c *x509.Certificate, data, sig []byte) error {
	k, ok := c.PublicKey.(*rsa.PublicKey)
	if !ok || k.Size() != attestSigSize {
		return errors.New("not an RSA-2048 key")
	}
	h := sha256.Sum256(data)
	return rsa.VerifyPKCS1v15(k, crypto.SHA256, h[:], sig)
}

// issuedBy reports whether parent's key signed child. Like Google's verifier it checks the issuer
// name and the signature only: the HSM certificates carry no CA constraints to check.
func issuedBy(parent, child *x509.Certificate) bool {
	return bytes.Equal(child.RawIssuer, parent.RawSubject) &&
		parent.CheckSignature(child.SignatureAlgorithm, child.RawTBSCertificate, child.Signature) == nil
}

func sameKey(a, b *x509.Certificate) bool {
	return bytes.Equal(a.RawSubjectPublicKeyInfo, b.RawSubjectPublicKeyInfo)
}

// buildChains builds the manufacturer and owner chains the way Google's verifier does and returns the
// two partition certificates. Every certificate in the attestation must be used.
func buildChains(cc *kmspb.KeyOperationAttestation_CertificateChains, roots *AttestationRoots) (mfrPartition, ownerPartition *x509.Certificate, err error) {
	var pool []*x509.Certificate
	for _, list := range [][]string{cc.GetCaviumCerts(), cc.GetGoogleCardCerts(), cc.GetGooglePartitionCerts()} {
		for _, p := range list {
			cs, err := parseCerts(p)
			if err != nil {
				return nil, nil, attErr("certificate chain: %v", err)
			}
			pool = append(pool, cs...)
		}
	}
	if len(pool) == 0 || len(pool) > maxChainCerts {
		return nil, nil, attErr("%d certificates in the chains, want 1 to %d", len(pool), maxChainCerts)
	}
	take := func(issuer *x509.Certificate, pred func(*x509.Certificate) bool) *x509.Certificate {
		for i, c := range pool {
			if issuedBy(issuer, c) && pred(c) {
				pool = append(pool[:i], pool[i+1:]...)
				return c
			}
		}
		return nil
	}
	anyCert := func(*x509.Certificate) bool { return true }
	mfrCard := take(roots.Manufacturer, anyCert)
	if mfrCard == nil {
		return nil, nil, attErr("no card certificate issued by the manufacturer root")
	}
	mfrPartition = take(mfrCard, anyCert)
	if mfrPartition == nil {
		return nil, nil, attErr("no partition certificate issued by the manufacturer card")
	}
	ownerCard := take(roots.Owner, func(c *x509.Certificate) bool { return sameKey(c, mfrCard) })
	ownerPartition = take(roots.Owner, func(c *x509.Certificate) bool { return sameKey(c, mfrPartition) })
	if ownerCard == nil || ownerPartition == nil {
		return nil, nil, attErr("the owner card and partition certificates do not match the manufacturer's")
	}
	if len(pool) != 0 {
		return nil, nil, attErr("%d certificates are not part of the chains", len(pool))
	}
	return mfrPartition, ownerPartition, nil
}

func parseCerts(p string) ([]*x509.Certificate, error) {
	if len(p) > maxCertPEMSize {
		return nil, errors.New("certificate too large")
	}
	var out []*x509.Certificate
	rest := []byte(p)
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("PEM block %q is not a certificate", blk.Type)
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse: %v", err)
		}
		out = append(out, c)
		if len(out) > maxChainCerts {
			return nil, errors.New("too many certificates")
		}
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("text outside the PEM blocks")
	}
	if len(out) == 0 {
		return nil, errors.New("no certificate")
	}
	return out, nil
}

// reader is a bounds-checked big-endian reader.
type reader struct {
	b   []byte
	pos int
}

func (r *reader) u32() (uint32, error) {
	if len(r.b)-r.pos < 4 {
		return 0, errors.New("truncated")
	}
	v := binary.BigEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *reader) bytes(n uint32) ([]byte, error) {
	if uint64(n) > uint64(len(r.b)-r.pos) {
		return nil, errors.New("truncated")
	}
	v := r.b[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return v, nil
}

// parseObject reads an object (handle, attribute count, size, then count tag/length/value triples)
// from r, which must end where the object ends.
func parseObject(r *reader) (map[uint32][]byte, error) {
	if _, err := r.u32(); err != nil { // handle
		return nil, err
	}
	count, err := r.u32()
	if err != nil {
		return nil, err
	}
	if _, err := r.u32(); err != nil { // size
		return nil, err
	}
	if count > maxAttributes {
		return nil, fmt.Errorf("%d attributes", count)
	}
	attrs := make(map[uint32][]byte) // not sized by count: the count is untrusted
	for range count {
		tag, err := r.u32()
		if err != nil {
			return nil, err
		}
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		v, err := r.bytes(n)
		if err != nil {
			return nil, err
		}
		if _, dup := attrs[tag]; dup {
			return nil, fmt.Errorf("attribute 0x%x twice", tag)
		}
		attrs[tag] = v
	}
	if r.pos != len(r.b) {
		return nil, errors.New("bytes after the last attribute")
	}
	return attrs, nil
}

// parseObjects reads the attested objects. Both Cavium formats share this layout, confirmed against a
// real CAVIUM_V2_COMPRESSED attestation of a Cloud HSM key and a real version 1 one: a response header
// of big-endian words (response code, request flags, total size, buffer size, and in version 2 the
// buffer size again, so the header is 16 or 20 bytes plus any padding), the attestation buffer, which
// is the last "buffer size" bytes before the 256-byte signature, and in the buffer an info header
// (version, flags, offset of the first object, offset of the second object or 0, all 16-bit) followed
// by one or two objects.
func parseObjects(content []byte) ([]map[uint32][]byte, error) {
	body := content[:len(content)-attestSigSize]
	r := &reader{b: body}
	for range 3 { // response code, request flags, total size
		if _, err := r.u32(); err != nil {
			return nil, err
		}
	}
	buf, err := r.u32()
	if err != nil {
		return nil, err
	}
	if uint64(buf) > uint64(len(body)-r.pos) || buf < 8 {
		return nil, fmt.Errorf("buffer size %d does not fit the %d bytes after the header", buf, len(body)-r.pos)
	}
	attest := body[len(body)-int(buf):]
	first := uint32(binary.BigEndian.Uint16(attest[4:]))
	second := uint32(binary.BigEndian.Uint16(attest[6:]))
	ends := []uint32{buf}
	if second != 0 {
		ends = []uint32{second, buf}
	}
	starts := []uint32{first, second}
	var objs []map[uint32][]byte
	for i, end := range ends {
		if starts[i] < 8 || end > buf || starts[i] >= end {
			return nil, errors.New("object offsets out of range")
		}
		o, err := parseObject(&reader{b: attest[starts[i]:end]})
		if err != nil {
			return nil, err
		}
		objs = append(objs, o)
	}
	return objs, nil
}

func boolAttr(o map[uint32][]byte, tag uint32) (v, ok bool) {
	b, has := o[tag]
	if !has || len(b) != 1 || b[0] > 1 {
		return false, false
	}
	return b[0] == 1, true
}

// checkObjects looks for an attested object that proves pub is an HSM-generated, non-extractable EC key
// of the key version. It returns the failure of the first object when none does.
func checkObjects(objs []map[uint32][]byte, version string, pub []byte) error {
	var first error
	for _, o := range objs {
		err := checkObject(o, version, pub)
		if err == nil {
			return nil
		}
		if first == nil {
			first = err
		}
	}
	if first == nil { // no objects: never a success
		return attErr("no attested objects")
	}
	return first
}

func checkObject(o map[uint32][]byte, version string, pub []byte) error {
	if kt := o[attrKeyType]; len(kt) != 1 || kt[0] != ckkEC {
		return attErr("the attested key is not an EC key")
	}
	if !bytes.Equal(o[attrECPoint], pub) || len(pub) == 0 {
		return attErr("the attested public key is not the key version's public key")
	}
	if ext, ok := boolAttr(o, attrExtractable); !ok || ext {
		return attErr("the attested key is extractable or does not say it is not")
	}
	if local, _ := boolAttr(o, attrLocal); !local { // missing or malformed reads as false
		return attErr("the attested key was not generated on the HSM (it may be imported)")
	}
	// The key ID is two SHA-256 digests in hex; the second is the digest of the key version's resource
	// name (Google, "Verify attestations", "Key version resource ID").
	id := o[attrID]
	if len(id) != 128 {
		return attErr("the attested key ID is %d bytes, want 128", len(id))
	}
	sum := sha256.Sum256([]byte(version))
	if !bytes.EqualFold(id[64:], []byte(hex.EncodeToString(sum[:]))) {
		return attErr("the attested key ID is not that of key version %s", version)
	}
	return nil
}
