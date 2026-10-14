package awskms

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"math/big"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/bide-ai/bide/audit"
)

type derMutatingKMS struct {
	*fakeKMS
	mut func(der []byte, digest []byte) []byte
}

func (d derMutatingKMS) Sign(ctx context.Context, in *kms.SignInput, o ...func(*kms.Options)) (*kms.SignOutput, error) {
	out, err := d.fakeKMS.Sign(ctx, in, o...)
	if err != nil {
		return nil, err
	}
	out.Signature = d.mut(out.Signature, in.Message)
	return out, nil
}

// Malformed DER from KMS (trailing bytes, BER padding, negative integers) must not sign.
func TestP256BadDERRefused(t *testing.T) {
	muts := map[string]func([]byte, []byte) []byte{
		"trailing": func(d, m []byte) []byte { return append(append([]byte{}, d...), 0) },
		"padded": func(d, m []byte) []byte {
			var sig struct{ R, S *big.Int }
			asn1.Unmarshal(d, &sig)
			// r with a redundant leading zero byte: 0x30 len 02 (len+1) 00 00 ... (BER, not DER)
			rb := append([]byte{0, 0}, sig.R.Bytes()...)
			sb := sig.S.Bytes()
			body := append(append([]byte{0x02, byte(len(rb))}, rb...), append([]byte{0x02, byte(len(sb))}, sb...)...)
			return append([]byte{0x30, byte(len(body))}, body...)
		},
		"other-digest": func(d, m []byte) []byte {
			k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			h := sha256.Sum256(m)
			r, sv, _ := ecdsa.Sign(rand.Reader, k, h[:])
			out, _ := asn1.Marshal(struct{ R, S *big.Int }{r, sv})
			return out
		},
	}
	for name, mut := range muts {
		f := newP256Fake(t)
		// open with a plain client, then swap the client to the mutating one
		sg := openP256(t, f)
		sg.client = derMutatingKMS{f, mut}
		if sig, err := sg.Sign([]byte("m")); err == nil {
			t.Fatalf("%s: signed %x", name, sig)
		}
	}
}

// KeySpec says P-256 but the SPKI is another curve.
func TestP256SPKICurveMismatch(t *testing.T) {
	f := newP256Fake(t)
	k, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	c := spkiKMS{f, der}
	if _, err := Open(context.Background(), Config{Client: c, KeyID: testARN, Scheme: audit.AlgECDSAP256}); err == nil {
		t.Fatal("opened a P-384 SPKI as P-256")
	}
}

type spkiKMS struct {
	*fakeKMS
	der []byte
}

func (s spkiKMS) GetPublicKey(ctx context.Context, in *kms.GetPublicKeyInput, o ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
	out, err := s.fakeKMS.GetPublicKey(ctx, in, o...)
	if err == nil {
		out.PublicKey, out.KeySpec = s.der, types.KeySpecEccNistP256
	}
	return out, err
}
