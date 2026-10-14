// Package gcpkms is an audit.Signer whose Ed25519 or ECDSA P-256 private key lives in Google Cloud
// KMS. Cloud KMS does not export asymmetric private keys; bide sends it each message (Ed25519) or
// digest (P-256) and gets the signature back.
//
// It lives in its own Go module so that the Google Cloud SDK stays out of the core module.
//
// # What it signs with
//
// Config.Scheme picks the scheme, and the signer never signs with a different one in its place:
//
//   - audit.AlgEd25519 (the default): a CryptoKeyVersion with algorithm EC_SIGN_ED25519, which
//     signs the raw message in the request's data field in PureEdDSA mode (RFC 8032), so audit's
//     verifiers check its signatures with no change. Open refuses a version of another algorithm
//     (ErrNoEd25519).
//   - audit.AlgECDSAP256: a version with algorithm EC_SIGN_P256_SHA256, signing the SHA-256 digest
//     of the message in the request's digest field. Cloud KMS returns an ASN.1 DER signature,
//     which the signer converts to the canonical low-S r||s that audit verifies. Open refuses a
//     version of another algorithm (ErrNoP256).
//
// # Protection level
//
// Google offers EC_SIGN_ED25519 at protection level SOFTWARE only, so an Ed25519 key is never in
// an HSM there. EC_SIGN_P256_SHA256 is available in an HSM. Config.RequireHSM makes Open refuse a
// version whose protection level is not HSM or HSM_SINGLE_TENANT (ErrNotHSM), which means it
// refuses every Ed25519 version: a deployment that must keep its signing key in an HSM uses
// audit.AlgECDSAP256. RequireHSM is off by default for that reason, since a default that refuses
// the default scheme would fail every Ed25519 Open. The level reported on every Sign response must
// equal the level Open pinned.
//
// # What Open checks, and what each call needs
//
// Config.KeyVersion is the full CryptoKeyVersion resource name, including the numeric version ID:
// there is no "latest" alias, so a rotation to a new primary version cannot change which key
// signs. Open reads the version (cloudkms.cryptoKeyVersions.get), refuses one that is not ENABLED
// (ErrVersionState), of the wrong algorithm, or of the wrong protection level, reads its public key
// (cloudkms.cryptoKeyVersions.viewPublicKey), and signs once (cloudkms.cryptoKeyVersions.useToSign;
// one billed operation) to prove the permission and that the key signs under the public key it
// reported. A CryptoKeyVersion with an EC_SIGN algorithm exists only in a key with purpose
// ASYMMETRIC_SIGN, so the algorithm check is also the purpose check and needs no permission on the
// CryptoKey. A custom role holding exactly those three permissions on the key version is enough.
//
// # Integrity checks
//
// Following Google's end-to-end data integrity guide, every request carries a CRC32C of what it
// sends (data_crc32c for Ed25519, digest_crc32c for P-256) and the signer checks that the response
// names the version it asked for, that the response reports verifying the request checksum
// (verified_data_crc32c or verified_digest_crc32c), and that signature_crc32c matches the signature.
// GetPublicKey is checked the same way (name, and pem_crc32c over the PEM). A signature whose
// checksum does not match is retried, up to three attempts in all, as the guide advises for a
// transient corruption; any other mismatch is an error (ErrIntegrity) at once. These checks catch
// corruption in transit; the check that matters for authenticity is the one the signer always
// makes, that the signature verifies under the public key.
//
// # Message size
//
// Google documents no size limit for the data of an EC_SIGN_ED25519 request, so this signer
// enforces none and sends the message as given. Tree heads, grants and approver decisions are small
// (a tree head is about 110 bytes plus the run ID's length) and an evidence package seal signs a
// fixed 60-byte message. If Cloud KMS rejects a message as too large, Sign returns a CodeError with
// code InvalidArgument. Under ECDSA P-256 Cloud KMS signs a 32-byte digest, so there is no limit.
// Cloud KMS cannot sign an empty message under Ed25519 (an empty data field reads as absent), so
// Sign refuses one with ErrEmptyMessage; audit never signs an empty message.
//
// # Errors
//
// A failure from the Cloud KMS client reaches the caller as a CodeError carrying the gRPC status
// code and nothing else, because the message of such an error can carry request detail or the
// text of a failed credential exchange. Test it with errors.Is, for example
// errors.Is(err, gcpkms.CodeError{Code: codes.PermissionDenied}). A cancelled or expired context
// also matches context.Canceled or context.DeadlineExceeded.
//
// # Use
//
//	client, _ := kms.NewKeyManagementClient(ctx)
//	s, err := gcpkms.Open(ctx, gcpkms.Config{
//		Client:     client,
//		KeyVersion: "projects/p/locations/europe-west2/keyRings/bide/cryptoKeys/audit/cryptoKeyVersions/1",
//	})
//	if err != nil { ... }
//	store, err := audit.NewAuditedStore(inner, s, anchor)
//
// Set Scheme: audit.AlgECDSAP256 (and RequireHSM: true for an HSM-protected key) for an
// EC_SIGN_P256_SHA256 key. The client authenticates with Application Default Credentials; none of
// them reach this package.
package gcpkms

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"regexp"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// DefaultTimeout bounds each Cloud KMS call when Config.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// maxSignAttempts is how many times Sign asks Cloud KMS when the checksum of the signature it
// returns does not match (Google's guidance is a limited number of retries).
const maxSignAttempts = 3

// selfTest is the message Open signs once.
const selfTest = "bide.signer.selftest.v1"

// Errors a caller can test with errors.Is.
var (
	// ErrNoEd25519 means Config.Scheme is Ed25519 and the key version's algorithm is not EC_SIGN_ED25519.
	ErrNoEd25519 = errors.New("gcpkms: the key version is not an EC_SIGN_ED25519 signing key")
	// ErrNoP256 means Config.Scheme is audit.AlgECDSAP256 and the key version's algorithm is not
	// EC_SIGN_P256_SHA256.
	ErrNoP256 = errors.New("gcpkms: the key version is not an EC_SIGN_P256_SHA256 signing key")
	// ErrVersionState means the key version is not ENABLED.
	ErrVersionState = errors.New("gcpkms: the key version is not enabled")
	// ErrNotHSM means Config.RequireHSM is set and the key version's protection level is not HSM or
	// HSM_SINGLE_TENANT.
	ErrNotHSM = errors.New("gcpkms: the key version is not protected by an HSM")
	// ErrIntegrity means a Cloud KMS response failed an integrity check: a checksum that does not
	// match, a response for another resource, a missing checksum field, or a protection level that
	// differs from the one Open pinned.
	ErrIntegrity = errors.New("gcpkms: a Cloud KMS response failed an integrity check")
	// ErrBadSignature means the signature Cloud KMS returned does not verify under the key version's
	// public key (was the key replaced?).
	ErrBadSignature = errors.New("gcpkms: the signature does not verify under the key version's public key")
	// ErrEmptyMessage means Sign was given an empty message under Ed25519, which Cloud KMS cannot sign.
	ErrEmptyMessage = errors.New("gcpkms: Cloud KMS cannot sign an empty message")
)

// CodeError is a gRPC status code a Cloud KMS call returned. It carries the code and nothing else,
// so it is safe to log; the message text of the original error is not kept. Match it with
// errors.Is(err, gcpkms.CodeError{Code: codes.PermissionDenied}).
type CodeError struct {
	Code codes.Code
}

// Error names the code.
func (e CodeError) Error() string { return "gcpkms: Cloud KMS returned " + e.Code.String() }

// callError reduces err, which came from the Cloud KMS client, to what is safe to show: a CodeError,
// plus the context error it matched, if any. A context error that grpc turned into a status keeps
// its context sentinel too.
func callError(op string, err error) error {
	code := codes.Unknown
	if st, ok := status.FromError(err); ok {
		code = st.Code()
	}
	e := scrubbed{msg: fmt.Sprintf("gcpkms: %s: Cloud KMS returned %s", op, code), is: []error{CodeError{Code: code}}}
	switch {
	case errors.Is(err, context.DeadlineExceeded) || code == codes.DeadlineExceeded:
		e.is = append(e.is, context.DeadlineExceeded)
	case errors.Is(err, context.Canceled) || code == codes.Canceled:
		e.is = append(e.is, context.Canceled)
	}
	return e
}

// scrubbed is an error with a fixed message whose chain holds only the sentinels it matches.
type scrubbed struct {
	msg string
	is  []error
}

func (e scrubbed) Error() string   { return e.msg }
func (e scrubbed) Unwrap() []error { return e.is }

// API is the part of the Cloud KMS client the signer uses. *kms.KeyManagementClient satisfies it;
// a test supplies a fake or a client connected to an emulator, so no test needs Google Cloud.
type API interface {
	GetCryptoKeyVersion(ctx context.Context, req *kmspb.GetCryptoKeyVersionRequest, opts ...gax.CallOption) (*kmspb.CryptoKeyVersion, error)
	GetPublicKey(ctx context.Context, req *kmspb.GetPublicKeyRequest, opts ...gax.CallOption) (*kmspb.PublicKey, error)
	AsymmetricSign(ctx context.Context, req *kmspb.AsymmetricSignRequest, opts ...gax.CallOption) (*kmspb.AsymmetricSignResponse, error)
}

var _ API = (*kms.KeyManagementClient)(nil)

// versionName is the shape of a CryptoKeyVersion resource name; the version ID is the number
// Cloud KMS assigns, so an alias or "latest" is not accepted.
var versionName = regexp.MustCompile(`^projects/[A-Za-z0-9_:.-]+/locations/[A-Za-z0-9_-]+/keyRings/[A-Za-z0-9_-]+/cryptoKeys/[A-Za-z0-9_-]+/cryptoKeyVersions/[1-9][0-9]*$`)

// Config selects the Cloud KMS key version.
type Config struct {
	// Client calls Cloud KMS (kms.NewKeyManagementClient). Required.
	Client API
	// KeyVersion is the full CryptoKeyVersion resource name,
	// projects/P/locations/L/keyRings/R/cryptoKeys/K/cryptoKeyVersions/N. Required. Signing uses
	// exactly this version.
	KeyVersion string
	// Scheme is the signature scheme of the key version: audit.AlgEd25519 (the default; algorithm
	// EC_SIGN_ED25519) or audit.AlgECDSAP256 (algorithm EC_SIGN_P256_SHA256). A version of the other
	// algorithm is refused, never used under the wrong scheme.
	Scheme audit.Alg
	// RequireHSM makes Open refuse a version whose protection level is not HSM or HSM_SINGLE_TENANT.
	// Google offers Ed25519 at protection level SOFTWARE only, so RequireHSM with the Ed25519 scheme
	// always fails; use audit.AlgECDSAP256. It is off by default because the default scheme is
	// Ed25519.
	RequireHSM bool
	// Timeout bounds each Cloud KMS call; zero means DefaultTimeout.
	Timeout time.Duration
}

// Signer signs with an Ed25519 or ECDSA P-256 Cloud KMS key version (see Config.Scheme). It
// satisfies audit.Signer and is safe for concurrent use.
type Signer struct {
	client  API
	version string
	timeout time.Duration
	pub     []byte
	alg     audit.Alg
	level   kmspb.ProtectionLevel
	v       audit.Verifier // checks every signature against the public key
}

var _ audit.Signer = (*Signer)(nil)

// Open reads the key version, checks that it is an enabled signing key of Config.Scheme (and an HSM
// key if RequireHSM), reads and checks its public key, and signs once to prove the key and the
// caller's cloudkms.cryptoKeyVersions.useToSign permission. It fails with ErrNoEd25519 (or ErrNoP256
// for the P-256 scheme) for a version of another algorithm, ErrVersionState for one that is not
// enabled, and ErrNotHSM when RequireHSM is not met.
func Open(ctx context.Context, cfg Config) (*Signer, error) {
	switch {
	case cfg.Client == nil:
		return nil, fmt.Errorf("gcpkms: Config.Client is required: %w", agent.ErrConfig)
	case cfg.KeyVersion == "":
		return nil, fmt.Errorf("gcpkms: Config.KeyVersion is required: %w", agent.ErrConfig)
	case !versionName.MatchString(cfg.KeyVersion):
		return nil, fmt.Errorf("gcpkms: Config.KeyVersion is not a full CryptoKeyVersion resource name "+
			"(projects/P/locations/L/keyRings/R/cryptoKeys/K/cryptoKeyVersions/N, numeric version, no alias): %w", agent.ErrConfig)
	}
	alg := cfg.Scheme
	if alg == "" {
		alg = audit.AlgEd25519
	}
	if alg != audit.AlgEd25519 && alg != audit.AlgECDSAP256 {
		return nil, fmt.Errorf("gcpkms: Config.Scheme %q is not ed25519 or ecdsa-p256: %w", alg, agent.ErrConfig)
	}
	s := &Signer{client: cfg.Client, version: cfg.KeyVersion, alg: alg, timeout: cfg.Timeout}
	if s.timeout <= 0 {
		s.timeout = DefaultTimeout
	}
	want, sentinel := kmspb.CryptoKeyVersion_EC_SIGN_ED25519, ErrNoEd25519
	if alg == audit.AlgECDSAP256 {
		want, sentinel = kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256, ErrNoP256
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	ver, err := cfg.Client.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: cfg.KeyVersion})
	if err != nil {
		return nil, callError("get the key version "+cfg.KeyVersion, err)
	}
	if ver.GetName() != cfg.KeyVersion {
		return nil, fmt.Errorf("gcpkms: the key version response names %q, want %q: %w", ver.GetName(), cfg.KeyVersion, ErrIntegrity)
	}
	if ver.GetAlgorithm() != want {
		return nil, fmt.Errorf("gcpkms: key version %s has algorithm %s, want %s for the %s scheme: %w", cfg.KeyVersion, ver.GetAlgorithm(), want, alg, sentinel)
	}
	if ver.GetState() != kmspb.CryptoKeyVersion_ENABLED {
		return nil, fmt.Errorf("gcpkms: key version %s is %s, want ENABLED: %w", cfg.KeyVersion, ver.GetState(), ErrVersionState)
	}
	s.level = ver.GetProtectionLevel()
	if s.level == kmspb.ProtectionLevel_PROTECTION_LEVEL_UNSPECIFIED {
		return nil, fmt.Errorf("gcpkms: key version %s reports no protection level: %w", cfg.KeyVersion, ErrIntegrity)
	}
	if cfg.RequireHSM && s.level != kmspb.ProtectionLevel_HSM && s.level != kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
		return nil, fmt.Errorf("gcpkms: key version %s has protection level %s, RequireHSM needs HSM or HSM_SINGLE_TENANT: %w", cfg.KeyVersion, s.level, ErrNotHSM)
	}

	pk, err := cfg.Client.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: cfg.KeyVersion, PublicKeyFormat: kmspb.PublicKey_PEM})
	if err != nil {
		return nil, callError("get the public key of "+cfg.KeyVersion, err)
	}
	pub, err := s.checkPublicKey(pk, want, sentinel)
	if err != nil {
		return nil, err
	}
	v, err := audit.NewVerifier(alg, pub)
	if err != nil {
		return nil, fmt.Errorf("gcpkms: the public key of %s is not a usable %s key: %w", cfg.KeyVersion, alg, err)
	}
	s.pub, s.v = pub, v // pub is freshly parsed, so the signer owns it
	// One signature proves the caller may sign with the key and that Cloud KMS's output verifies
	// under the public key it reported, so a missing permission or a mismatch is found here and not
	// by the first anchor. It is one billed Cloud KMS operation.
	if _, err := s.sign(ctx, []byte(selfTest)); err != nil {
		return nil, fmt.Errorf("gcpkms: self-test signature: %w", err)
	}
	return s, nil
}

// checkPublicKey checks a GetPublicKey response against the version Open pinned and returns the
// public key in the encoding audit.NewVerifier reads.
func (s *Signer) checkPublicKey(pk *kmspb.PublicKey, want kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm, sentinel error) ([]byte, error) {
	switch {
	case pk.GetName() != s.version:
		return nil, fmt.Errorf("gcpkms: the public key response names %q, want %q: %w", pk.GetName(), s.version, ErrIntegrity)
	case pk.GetPemCrc32C() == nil || pk.GetPemCrc32C().GetValue() != int64(crc32c([]byte(pk.GetPem()))):
		return nil, fmt.Errorf("gcpkms: the public key of %s does not match its checksum: %w", s.version, ErrIntegrity)
	case pk.GetAlgorithm() != want:
		return nil, fmt.Errorf("gcpkms: the public key of %s reports algorithm %s, want %s: %w", s.version, pk.GetAlgorithm(), want, sentinel)
	case pk.GetProtectionLevel() != s.level:
		return nil, fmt.Errorf("gcpkms: the public key of %s reports protection level %s, the key version reports %s: %w", s.version, pk.GetProtectionLevel(), s.level, ErrIntegrity)
	}
	blk, rest := pem.Decode([]byte(pk.GetPem()))
	if blk == nil || blk.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("gcpkms: the public key of %s is not one PEM PUBLIC KEY block: %w", s.version, sentinel)
	}
	if s.alg == audit.AlgECDSAP256 {
		pub, err := audit.ECDSAP256PublicKeyFromSPKI(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("gcpkms: parse the public key of %s: %v: %w", s.version, err, sentinel)
		}
		return pub, nil
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("gcpkms: parse the public key of %s: %v: %w", s.version, err, sentinel)
	}
	ed, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("gcpkms: the public key of %s is %T, not Ed25519: %w", s.version, k, sentinel)
	}
	return ed, nil
}

// Alg reports the signature scheme (Config.Scheme, audit.AlgEd25519 by default).
func (s *Signer) Alg() audit.Alg { return s.alg }

// PublicKey returns the public key of the key version (32 bytes for Ed25519, the 65-byte
// uncompressed point for P-256), in the encoding audit.NewVerifier reads. It is read once, when the
// Signer opens.
func (s *Signer) PublicKey() []byte { return bytes.Clone(s.pub) }

// KeyVersion is the CryptoKeyVersion resource name the signer signs with.
func (s *Signer) KeyVersion() string { return s.version }

// ProtectionLevel is the protection level Open found for the key version: "SOFTWARE", "HSM",
// "HSM_SINGLE_TENANT", "EXTERNAL" or "EXTERNAL_VPC".
func (s *Signer) ProtectionLevel() string { return s.level.String() }

var crcTable = crc32.MakeTable(crc32.Castagnoli)

func crc32c(b []byte) uint32 { return crc32.Checksum(b, crcTable) }

// Sign returns Cloud KMS's signature over message in the encoding audit verifies. Under Ed25519 the
// message goes in the request's data field; under P-256 Cloud KMS signs the SHA-256 digest. It
// checks the response's integrity fields and then the signature against the public key before
// returning it, so a key replaced behind the version name is an error here and not a signature
// that fails to verify later.
func (s *Signer) Sign(message []byte) ([]byte, error) {
	return s.sign(context.Background(), message)
}

// sign is Sign under ctx, which Open passes so that cancelling Open stops its self-test signature.
func (s *Signer) sign(ctx context.Context, message []byte) ([]byte, error) {
	req := &kmspb.AsymmetricSignRequest{Name: s.version}
	if s.alg == audit.AlgECDSAP256 {
		h := sha256.Sum256(message)
		req.Digest = &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: h[:]}}
		req.DigestCrc32C = wrapperspb.Int64(int64(crc32c(h[:])))
	} else {
		if len(message) == 0 {
			return nil, ErrEmptyMessage
		}
		req.Data = message
		req.DataCrc32C = wrapperspb.Int64(int64(crc32c(message)))
	}
	for attempt := 1; ; attempt++ {
		sig, retry, err := s.signOnce(ctx, req)
		if err == nil {
			if s.v == nil || !s.v.Verify(message, sig) {
				return nil, fmt.Errorf("gcpkms: %s: %w", s.version, ErrBadSignature)
			}
			return bytes.Clone(sig), nil
		}
		if !retry || attempt == maxSignAttempts {
			return nil, err
		}
	}
}

// signOnce makes one AsymmetricSign call and checks the response. retry reports a failure that
// Google's integrity guide says to retry: the signature's checksum did not match.
func (s *Signer) signOnce(ctx context.Context, req *kmspb.AsymmetricSignRequest) (sig []byte, retry bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	out, err := s.client.AsymmetricSign(ctx, req)
	if err != nil {
		return nil, false, callError("sign with "+s.version, err)
	}
	if out.GetName() != s.version {
		return nil, false, fmt.Errorf("gcpkms: the sign response names %q, want %q: %w", out.GetName(), s.version, ErrIntegrity)
	}
	verified := out.GetVerifiedDataCrc32C()
	if s.alg == audit.AlgECDSAP256 {
		verified = out.GetVerifiedDigestCrc32C()
	}
	if !verified {
		return nil, false, fmt.Errorf("gcpkms: Cloud KMS did not verify the request checksum of %s: %w", s.version, ErrIntegrity)
	}
	if out.GetProtectionLevel() != s.level {
		return nil, false, fmt.Errorf("gcpkms: the sign response reports protection level %s, Open pinned %s: %w", out.GetProtectionLevel(), s.level, ErrIntegrity)
	}
	sig = out.GetSignature()
	if c := out.GetSignatureCrc32C(); c == nil {
		return nil, false, fmt.Errorf("gcpkms: the sign response from %s carries no signature checksum: %w", s.version, ErrIntegrity)
	} else if c.GetValue() != int64(crc32c(sig)) {
		return nil, true, fmt.Errorf("gcpkms: the signature from %s does not match its checksum: %w", s.version, ErrIntegrity)
	}
	if s.alg == audit.AlgECDSAP256 {
		// Cloud KMS returns an ASN.1 DER ECDSA-Sig-Value; audit verifies the canonical low-S r||s.
		sig, err = audit.ECDSAP256SignatureFromDER(sig)
		if err != nil {
			return nil, false, fmt.Errorf("gcpkms: Cloud KMS's signature from %s is not a valid ECDSA signature: %w", s.version, err)
		}
	}
	return sig, false, nil
}

// String describes the signer by its key identity (audit.KeyID of the public key).
func (s *Signer) String() string {
	return fmt.Sprintf("gcpkms.Signer{%s %s}", s.version, audit.KeyID(string(s.alg), s.pub))
}

// Format writes String for every verb.
func (s *Signer) Format(f fmt.State, _ rune) { io.WriteString(f, s.String()) }
