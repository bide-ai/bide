// Package fakekms is a KMS stand-in for tests: it holds Ed25519 and ECDSA P-256 keys by ARN and
// answers GetPublicKey and Sign as AWS KMS does for those key specs (Ed25519 signs the raw message
// with ED25519_SHA_512; ECDSA P-256 signs a 32-byte SHA-256 digest with ECDSA_SHA_256 and returns
// DER). No test calls AWS.
package fakekms

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"math/big"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/bide-ai/bide/signer/awskms"
)

type key struct {
	ed *ed25519.PrivateKey
	ec *ecdsa.PrivateKey
}

// KMS is the fake. It satisfies awskms.API.
type KMS struct {
	mu    sync.Mutex
	keys  map[string]key
	alias map[string]string // alias ARN -> key ARN
	// Answer, when set, is the key ARN GetPublicKey returns whatever was asked, as a redirected or
	// faulty endpoint might.
	Answer string
	Signs  int // Sign calls made
}

// New returns an empty fake.
func New() *KMS { return &KMS{keys: map[string]key{}, alias: map[string]string{}} }

// AddAlias makes the alias ARN resolve to the key ARN.
func (k *KMS) AddAlias(aliasARN, keyARN string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.alias[aliasARN] = keyARN
}

// AddEd25519 creates an Ed25519 key under arn.
func (k *KMS) AddEd25519(arn string) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys[arn] = key{ed: &priv}
}

// AddP256 creates an ECDSA P-256 key under arn.
func (k *KMS) AddP256(arn string) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys[arn] = key{ec: priv}
}

// Client returns the fake as the client for a region, in the shape hsmkeys.Options.KMSClient takes.
// It fails for a key that is not in that region, as a regional endpoint would not find it.
func (k *KMS) Client(_ context.Context, region string) (awskms.API, error) {
	return regional{k, region}, nil
}

type regional struct {
	*KMS
	region string
}

func (r regional) find(id string) (string, key, error) {
	parts := strings.Split(id, ":")
	if len(parts) < 6 || parts[3] != r.region {
		return "", key{}, errors.New("NotFoundException: no such key in this region")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if target, ok := r.alias[id]; ok {
		id = target
	}
	kk, ok := r.keys[id]
	if !ok {
		return "", key{}, errors.New("NotFoundException")
	}
	return id, kk, nil
}

func (r regional) GetPublicKey(_ context.Context, in *kms.GetPublicKeyInput, _ ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
	arn, kk, err := r.find(*in.KeyId)
	if err != nil {
		return nil, err
	}
	if r.Answer != "" {
		arn = r.Answer
	}
	out := &kms.GetPublicKeyOutput{KeyId: &arn, KeyUsage: types.KeyUsageTypeSignVerify}
	if kk.ed != nil {
		out.KeySpec = types.KeySpecEccNistEdwards25519
		out.SigningAlgorithms = []types.SigningAlgorithmSpec{types.SigningAlgorithmSpecEd25519Sha512}
		out.PublicKey, _ = x509.MarshalPKIXPublicKey(kk.ed.Public())
		return out, nil
	}
	out.KeySpec = types.KeySpecEccNistP256
	out.SigningAlgorithms = []types.SigningAlgorithmSpec{types.SigningAlgorithmSpecEcdsaSha256}
	out.PublicKey, _ = x509.MarshalPKIXPublicKey(&kk.ec.PublicKey)
	return out, nil
}

func (r regional) Sign(_ context.Context, in *kms.SignInput, _ ...func(*kms.Options)) (*kms.SignOutput, error) {
	_, kk, err := r.find(*in.KeyId)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.Signs++
	r.mu.Unlock()
	if kk.ed != nil {
		if in.SigningAlgorithm != types.SigningAlgorithmSpecEd25519Sha512 || in.MessageType != types.MessageTypeRaw {
			return nil, errors.New("ValidationException: ED25519_SHA_512 needs a RAW message")
		}
		return &kms.SignOutput{Signature: ed25519.Sign(*kk.ed, in.Message)}, nil
	}
	if in.SigningAlgorithm != types.SigningAlgorithmSpecEcdsaSha256 || in.MessageType != types.MessageTypeDigest || len(in.Message) != sha256.Size {
		return nil, errors.New("ValidationException: ECDSA_SHA_256 needs a 32-byte DIGEST")
	}
	rr, ss, err := ecdsa.Sign(rand.Reader, kk.ec, in.Message)
	if err != nil {
		return nil, err
	}
	der, _ := asn1.Marshal(struct{ R, S *big.Int }{rr, ss})
	return &kms.SignOutput{Signature: der}, nil
}
