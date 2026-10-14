// Package awskms is an audit.Signer whose Ed25519 private key lives in AWS KMS. KMS does not
// export asymmetric private keys; bide sends KMS each message and gets the signature back.
//
// It lives in its own Go module so that the AWS SDK stays out of the core module.
//
// # What it signs with
//
// bide's audit log and approver decisions are Ed25519, so the KMS key must be an asymmetric key
// of key spec ECC_NIST_EDWARDS25519 with key usage SIGN_VERIFY, which signs with
// ED25519_SHA_512. That algorithm is pure Ed25519 (RFC 8032) over the raw message, so audit's
// verifiers check its signatures with no change. Open signs once to prove the key and the
// caller's kms:Sign permission (one billed request), and refuses a key of another spec (ErrNoEd25519)
// and never signs with a different algorithm in its place.
//
// # The 4096-byte limit
//
// KMS signs a raw message of at most 4096 bytes, and Ed25519 signs the whole message (there is no
// pre-hashed form that audit's verifiers accept). Tree heads, grants and approver decisions are small, but each carries
// caller-chosen text: a tree head is about 110 bytes plus the run ID's length, and an approver
// decision includes the tool call's arguments. A run ID near 4000 bytes, or large arguments,
// exceed the limit, and Sign returns ErrMessageTooLarge naming the size. An evidence package seal (audit.EvidencePackage.Seal) signs the package's whole JSON encoding
// and exceeds the limit once the package proves more than a few actions: Sign then returns
// ErrMessageTooLarge. Seal such packages with a signer that has no limit (a PKCS#11 token, a
// software key), or do not seal them; the actions in an unsealed package still verify through
// their inclusion proofs. See docs/guides/key-custody.md.
//
// # Use
//
//	cfg, _ := config.LoadDefaultConfig(ctx)
//	s, err := awskms.Open(ctx, awskms.Config{
//		Client: kms.NewFromConfig(cfg),
//		KeyID:  "alias/bide-audit",
//	})
//	if err != nil { ... }
//	store, err := audit.NewAuditedStore(inner, s, anchor)
//
// Grant the application's role kms:GetPublicKey and kms:Sign on the key, and nothing else.
package awskms

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// MaxMessageSize is the longest message KMS signs (the raw-message limit of kms:Sign).
const MaxMessageSize = 4096

// DefaultTimeout bounds each KMS call when Config.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// selfTest is the message Open signs once.
const selfTest = "bide.signer.selftest.v1"

// Errors a caller can test with errors.Is.
var (
	// ErrNoEd25519 means the KMS key is not an Ed25519 signing key.
	ErrNoEd25519 = errors.New("awskms: the KMS key is not an Ed25519 signing key")
	// ErrMessageTooLarge means the message is longer than KMS signs (MaxMessageSize).
	ErrMessageTooLarge = errors.New("awskms: the message is longer than KMS signs")
)

// API is the part of the KMS client the signer uses. *kms.Client satisfies it; a test supplies a
// fake, so no test needs AWS.
type API interface {
	GetPublicKey(ctx context.Context, in *kms.GetPublicKeyInput, optFns ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error)
	Sign(ctx context.Context, in *kms.SignInput, optFns ...func(*kms.Options)) (*kms.SignOutput, error)
}

// Config selects the KMS key.
type Config struct {
	// Client calls KMS (kms.NewFromConfig(awsConfig)). Required.
	Client API
	// KeyID names the key: a key ID, a key ARN, or an alias name or ARN. Required. Open resolves
	// it once and signs with the key's ARN, so an alias moved to another key afterwards cannot
	// change which key signs.
	KeyID string
	// Timeout bounds each KMS call; zero means DefaultTimeout.
	Timeout time.Duration
	// GrantTokens are passed to GetPublicKey and Sign. Optional.
	GrantTokens []string
}

// Signer signs with an Ed25519 KMS key. It satisfies audit.Signer and is safe for concurrent use.
type Signer struct {
	client  API
	keyARN  string
	timeout time.Duration
	grants  []string
	pub     ed25519.PublicKey
}

var _ audit.Signer = (*Signer)(nil)

// Open resolves the key, checks that it is an Ed25519 signing key, and reads its public key. It
// fails with ErrNoEd25519, naming the key's spec, for any other key.
func Open(ctx context.Context, cfg Config) (*Signer, error) {
	switch {
	case cfg.Client == nil:
		return nil, fmt.Errorf("awskms: Config.Client is required: %w", agent.ErrConfig)
	case cfg.KeyID == "":
		return nil, fmt.Errorf("awskms: Config.KeyID is required: %w", agent.ErrConfig)
	}
	s := &Signer{client: cfg.Client, timeout: cfg.Timeout, grants: slices.Clone(cfg.GrantTokens)}
	if s.timeout <= 0 {
		s.timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	out, err := cfg.Client.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: &cfg.KeyID, GrantTokens: s.grants})
	if err != nil {
		return nil, fmt.Errorf("awskms: get the public key of %s: %w", cfg.KeyID, err)
	}
	if out.KeySpec != types.KeySpecEccNistEdwards25519 {
		return nil, fmt.Errorf("awskms: key %s has spec %q, want ECC_NIST_EDWARDS25519 (bide's audit log signs with Ed25519): %w", cfg.KeyID, out.KeySpec, ErrNoEd25519)
	}
	if out.KeyUsage != types.KeyUsageTypeSignVerify {
		return nil, fmt.Errorf("awskms: key %s has usage %q, want SIGN_VERIFY: %w", cfg.KeyID, out.KeyUsage, ErrNoEd25519)
	}
	if !slices.Contains(out.SigningAlgorithms, types.SigningAlgorithmSpecEd25519Sha512) {
		return nil, fmt.Errorf("awskms: key %s does not offer ED25519_SHA_512: %w", cfg.KeyID, ErrNoEd25519)
	}
	if out.KeyId == nil || *out.KeyId == "" {
		return nil, fmt.Errorf("awskms: KMS returned no key ARN for %s", cfg.KeyID)
	}
	pk, err := x509.ParsePKIXPublicKey(out.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("awskms: parse the public key of %s: %w", cfg.KeyID, err)
	}
	pub, ok := pk.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("awskms: the public key of %s is %T, not Ed25519: %w", cfg.KeyID, pk, ErrNoEd25519)
	}
	if _, err := audit.NewVerifier(audit.AlgEd25519, pub); err != nil {
		return nil, fmt.Errorf("awskms: the public key of %s is not a usable Ed25519 key: %w", cfg.KeyID, err)
	}
	s.keyARN, s.pub = *out.KeyId, bytes.Clone(pub)
	// One signature proves the caller may sign with the key and that KMS's output verifies under
	// the public key it reported, so a missing kms:Sign permission or a mismatch is found here and
	// not by the first anchor. It is one billed KMS request.
	if _, err := s.Sign([]byte(selfTest)); err != nil {
		return nil, fmt.Errorf("awskms: self-test signature: %w", err)
	}
	return s, nil
}

// Alg reports the signature scheme (audit.AlgEd25519).
func (*Signer) Alg() audit.Alg { return audit.AlgEd25519 }

// PublicKey returns the 32-byte Ed25519 public key of the KMS key, in the encoding
// audit.NewVerifier reads. It is read once, when the Signer opens.
func (s *Signer) PublicKey() []byte { return bytes.Clone(s.pub) }

// KeyARN is the ARN of the KMS key the signer signs with.
func (s *Signer) KeyARN() string { return s.keyARN }

// Sign returns KMS's Ed25519 signature over message, which is at most MaxMessageSize bytes (longer
// is ErrMessageTooLarge, with no call to KMS). It checks the signature against the public key
// before returning it, so a key replaced behind the ARN is an error here and not a signature that
// fails to verify later.
func (s *Signer) Sign(message []byte) ([]byte, error) {
	if len(message) > MaxMessageSize {
		return nil, fmt.Errorf("awskms: a %d-byte message exceeds the %d bytes KMS signs: %w", len(message), MaxMessageSize, ErrMessageTooLarge)
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	out, err := s.client.Sign(ctx, &kms.SignInput{
		KeyId:            &s.keyARN,
		Message:          message,
		MessageType:      types.MessageTypeRaw,
		SigningAlgorithm: types.SigningAlgorithmSpecEd25519Sha512,
		GrantTokens:      s.grants,
	})
	if err != nil {
		return nil, fmt.Errorf("awskms: sign with %s: %w", s.keyARN, err)
	}
	if len(out.Signature) != ed25519.SignatureSize || !ed25519.Verify(s.pub, message, out.Signature) {
		return nil, fmt.Errorf("awskms: KMS's signature does not verify under the key's public key (was the key replaced?)")
	}
	return bytes.Clone(out.Signature), nil
}

// String describes the signer by its key identity (audit.KeyID of the public key).
func (s *Signer) String() string {
	return fmt.Sprintf("awskms.Signer{%s %s}", s.keyARN, audit.KeyID(string(audit.AlgEd25519), s.pub))
}

// Format writes String for every verb.
func (s *Signer) Format(f fmt.State, _ rune) { io.WriteString(f, s.String()) }
