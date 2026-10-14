// Package awskms is an audit.Signer whose Ed25519 or ECDSA P-256 private key lives in AWS KMS. KMS does not
// export asymmetric private keys; bide sends KMS each message and gets the signature back.
//
// It lives in its own Go module so that the AWS SDK stays out of the core module.
//
// # What it signs with
//
// Config.Scheme picks the scheme, and the signer never signs with a different one in its place:
//
//   - audit.AlgEd25519 (the default): a key of spec ECC_NIST_EDWARDS25519 with usage SIGN_VERIFY,
//     signing ED25519_SHA_512 over the raw message, which is pure Ed25519 (RFC 8032), so audit's
//     verifiers check its signatures with no change. Open refuses a key of another spec
//     (ErrNoEd25519).
//   - audit.AlgECDSAP256: a key of spec ECC_NIST_P256, signing ECDSA_SHA_256 over the SHA-256 digest
//     of the message (MessageType DIGEST). KMS returns an ASN.1 DER signature, which the signer
//     converts to the canonical low-S r||s that audit verifies. Open refuses a key of another spec
//     (ErrNoP256).
//
// Open signs once to prove the key and the caller's kms:Sign permission (one billed request).
//
// # The 4096-byte limit
//
// Under Ed25519 KMS signs a raw message of at most 4096 bytes, and Ed25519 signs the whole message.
// Tree heads, grants and approver decisions are small, but each carries caller-chosen text: a tree
// head is about 110 bytes plus the run ID's length, and an approver decision includes the tool
// call's arguments. A run ID near 4000 bytes, or large arguments, exceed the limit, and Sign
// returns ErrMessageTooLarge naming the size. An evidence package seal signs a fixed 60-byte
// message (evidence format v6) and fits. Under ECDSA P-256 KMS signs a 32-byte digest, so there is
// no limit.
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
// Set Scheme: audit.AlgECDSAP256 for an ECC_NIST_P256 key.
//
// Grant the application's role kms:GetPublicKey and kms:Sign on the key, and nothing else.
package awskms

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
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
	// ErrNoEd25519 means Config.Scheme is Ed25519 and the KMS key is not an Ed25519 signing key.
	ErrNoEd25519 = errors.New("awskms: the KMS key is not an Ed25519 signing key")
	// ErrNoP256 means Config.Scheme is audit.AlgECDSAP256 and the KMS key is not an ECDSA P-256
	// signing key.
	ErrNoP256 = errors.New("awskms: the KMS key is not an ECDSA P-256 signing key")
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
	// Scheme is the signature scheme of the KMS key: audit.AlgEd25519 (the default; key spec
	// ECC_NIST_EDWARDS25519, signing ED25519_SHA_512) or audit.AlgECDSAP256 (key spec ECC_NIST_P256,
	// signing ECDSA_SHA_256 over the SHA-256 digest of the message). A key of the other spec is
	// refused, never used under the wrong scheme.
	Scheme audit.Alg
	// Timeout bounds each KMS call; zero means DefaultTimeout.
	Timeout time.Duration
	// GrantTokens are passed to GetPublicKey and Sign. Optional.
	GrantTokens []string
}

// Signer signs with an Ed25519 or ECDSA P-256 KMS key (see Config.Scheme). It satisfies audit.Signer and is safe for concurrent use.
type Signer struct {
	client  API
	keyARN  string
	timeout time.Duration
	grants  []string
	pub     []byte
	alg     audit.Alg
	v       audit.Verifier // checks every signature against the public key
}

var _ audit.Signer = (*Signer)(nil)

// Open resolves the key, checks that it is a signing key of Config.Scheme, reads its public key, and
// signs once to prove the key and the caller's kms:Sign permission. It fails with ErrNoEd25519 (or
// ErrNoP256 for the P-256 scheme), naming the key's spec, for a key of any other kind.
func Open(ctx context.Context, cfg Config) (*Signer, error) {
	switch {
	case cfg.Client == nil:
		return nil, fmt.Errorf("awskms: Config.Client is required: %w", agent.ErrConfig)
	case cfg.KeyID == "":
		return nil, fmt.Errorf("awskms: Config.KeyID is required: %w", agent.ErrConfig)
	}
	alg := cfg.Scheme
	if alg == "" {
		alg = audit.AlgEd25519
	}
	if alg != audit.AlgEd25519 && alg != audit.AlgECDSAP256 {
		return nil, fmt.Errorf("awskms: Config.Scheme %q is not ed25519 or ecdsa-p256: %w", alg, agent.ErrConfig)
	}
	s := &Signer{client: cfg.Client, alg: alg, timeout: cfg.Timeout, grants: slices.Clone(cfg.GrantTokens)}
	if s.timeout <= 0 {
		s.timeout = DefaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	out, err := cfg.Client.GetPublicKey(callCtx, &kms.GetPublicKeyInput{KeyId: &cfg.KeyID, GrantTokens: s.grants})
	if err != nil {
		return nil, fmt.Errorf("awskms: get the public key of %s: %w", cfg.KeyID, err)
	}
	spec, signAlg, sentinel := types.KeySpecEccNistEdwards25519, types.SigningAlgorithmSpecEd25519Sha512, ErrNoEd25519
	if alg == audit.AlgECDSAP256 {
		spec, signAlg, sentinel = types.KeySpecEccNistP256, types.SigningAlgorithmSpecEcdsaSha256, ErrNoP256
	}
	if out.KeySpec != spec {
		return nil, fmt.Errorf("awskms: key %s has spec %q, want %s for the %s scheme: %w", cfg.KeyID, out.KeySpec, spec, alg, sentinel)
	}
	if out.KeyUsage != types.KeyUsageTypeSignVerify {
		return nil, fmt.Errorf("awskms: key %s has usage %q, want SIGN_VERIFY: %w", cfg.KeyID, out.KeyUsage, sentinel)
	}
	if !slices.Contains(out.SigningAlgorithms, signAlg) {
		return nil, fmt.Errorf("awskms: key %s does not offer %s: %w", cfg.KeyID, signAlg, sentinel)
	}
	if out.KeyId == nil || *out.KeyId == "" {
		return nil, fmt.Errorf("awskms: KMS returned no key ARN for %s", cfg.KeyID)
	}
	if err := checkResolved(cfg.KeyID, *out.KeyId); err != nil {
		return nil, err
	}
	var pub []byte
	if alg == audit.AlgECDSAP256 {
		pub, err = audit.ECDSAP256PublicKeyFromSPKI(out.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("awskms: parse the public key of %s: %v: %w", cfg.KeyID, err, sentinel)
		}
	} else {
		pk, err := x509.ParsePKIXPublicKey(out.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("awskms: parse the public key of %s: %w", cfg.KeyID, err)
		}
		ed, ok := pk.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("awskms: the public key of %s is %T, not Ed25519: %w", cfg.KeyID, pk, sentinel)
		}
		pub = ed
	}
	v, err := audit.NewVerifier(alg, pub)
	if err != nil {
		return nil, fmt.Errorf("awskms: the public key of %s is not a usable %s key: %w", cfg.KeyID, alg, err)
	}
	s.keyARN, s.pub, s.v = *out.KeyId, bytes.Clone(pub), v
	// One signature proves the caller may sign with the key and that KMS's output verifies under
	// the public key it reported, so a missing kms:Sign permission or a mismatch is found here and
	// not by the first anchor. It is one billed KMS request, bounded by ctx as well as by the timeout.
	if _, err := s.sign(ctx, []byte(selfTest)); err != nil {
		return nil, fmt.Errorf("awskms: self-test signature: %w", err)
	}
	return s, nil
}

// checkResolved refuses a key ARN that KMS returned for requested when it is not the key requested:
// a redirected endpoint or a wrong answer naming another key, or another account's, would otherwise
// be pinned and signed with. A key ARN or key ID must come back as that key; an alias (by ARN or by
// name) must resolve to a key ARN, and for an alias ARN in the same partition, region and account.
func checkResolved(requested, got string) error {
	bad := func(why string) error {
		return fmt.Errorf("awskms: KMS resolved %s to a key that is not the one requested (%s): %w", requested, why, agent.ErrConfig)
	}
	gp := strings.Split(got, ":")
	if len(gp) != 6 || gp[0] != "arn" || gp[2] != "kms" || !strings.HasPrefix(gp[5], "key/") {
		return bad("the answer is not a key ARN")
	}
	if rp := strings.Split(requested, ":"); len(rp) == 6 && rp[0] == "arn" && rp[2] == "kms" {
		switch {
		case strings.HasPrefix(rp[5], "key/"):
			if got != requested {
				return bad("a different key ARN came back")
			}
		case rp[1] != gp[1] || rp[3] != gp[3] || rp[4] != gp[4]:
			return bad("the key is in another partition, region or account than the alias")
		}
		return nil
	}
	if !strings.HasPrefix(requested, "alias/") && !strings.HasSuffix(got, ":key/"+requested) {
		return bad("a different key ID came back")
	}
	return nil
}

// Alg reports the signature scheme (Config.Scheme, audit.AlgEd25519 by default).
func (s *Signer) Alg() audit.Alg { return s.alg }

// PublicKey returns the public key of the KMS key (32 bytes for Ed25519, the 65-byte uncompressed
// point for P-256), in the encoding audit.NewVerifier reads. It is read once, when the Signer opens.
func (s *Signer) PublicKey() []byte { return bytes.Clone(s.pub) }

// KeyARN is the ARN of the KMS key the signer signs with.
func (s *Signer) KeyARN() string { return s.keyARN }

// Sign returns KMS's signature over message in the encoding audit verifies. Under Ed25519 the
// message is at most MaxMessageSize bytes (longer is ErrMessageTooLarge, with no call to KMS);
// under P-256 KMS signs a 32-byte digest and there is no limit. It checks the signature against the public key
// before returning it, so a key replaced behind the ARN is an error here and not a signature that
// fails to verify later.
func (s *Signer) Sign(message []byte) ([]byte, error) { return s.sign(context.Background(), message) }

// sign is Sign with a caller's context, which Open passes so that cancelling Open stops its self-test.
func (s *Signer) sign(ctx context.Context, message []byte) ([]byte, error) {
	in := &kms.SignInput{KeyId: &s.keyARN, GrantTokens: s.grants}
	if s.alg == audit.AlgECDSAP256 {
		// KMS signs the SHA-256 digest; a digest is 32 bytes, so the 4096-byte limit never applies.
		h := sha256.Sum256(message)
		in.Message, in.MessageType, in.SigningAlgorithm = h[:], types.MessageTypeDigest, types.SigningAlgorithmSpecEcdsaSha256
	} else {
		if len(message) > MaxMessageSize {
			return nil, fmt.Errorf("awskms: a %d-byte message exceeds the %d bytes KMS signs: %w", len(message), MaxMessageSize, ErrMessageTooLarge)
		}
		in.Message, in.MessageType, in.SigningAlgorithm = message, types.MessageTypeRaw, types.SigningAlgorithmSpecEd25519Sha512
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	out, err := s.client.Sign(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("awskms: sign with %s: %w", s.keyARN, err)
	}
	sig := out.Signature
	if s.alg == audit.AlgECDSAP256 {
		// KMS returns an ASN.1 DER ECDSA-Sig-Value; audit verifies the canonical low-S r||s.
		sig, err = audit.ECDSAP256SignatureFromDER(sig)
		if err != nil {
			return nil, fmt.Errorf("awskms: KMS's signature from %s is not a valid ECDSA signature: %w", s.keyARN, err)
		}
	}
	if s.v == nil || !s.v.Verify(message, sig) {
		return nil, fmt.Errorf("awskms: KMS's signature does not verify under the key's public key (was the key replaced?)")
	}
	return bytes.Clone(sig), nil
}

// String describes the signer by its key identity (audit.KeyID of the public key).
func (s *Signer) String() string {
	return fmt.Sprintf("awskms.Signer{%s %s}", s.keyARN, audit.KeyID(string(s.alg), s.pub))
}

// Format writes String for every verb.
func (s *Signer) Format(f fmt.State, _ rune) { io.WriteString(f, s.String()) }
