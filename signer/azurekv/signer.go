// Package azurekv is an audit.Signer whose ECDSA P-256 private key lives in Azure Key Vault or Azure
// Key Vault Managed HSM. The service does not export the private key; bide sends it the SHA-256
// digest of each message and gets the signature back.
//
// It lives in its own Go module so that the Azure SDK stays out of the core module.
//
// # Why only ECDSA P-256
//
// Key Vault and Managed HSM offer EC keys on P-256, P-256K, P-384 and P-521 (signing with ES256,
// ES256K, ES384 and ES512) and RSA keys; they offer no Ed25519 (the key type and curve tables are in
// "Key types, algorithms, and operations": https://learn.microsoft.com/azure/key-vault/keys/about-keys-details
// and, for Managed HSM, https://learn.microsoft.com/azure/key-vault/managed-hsm/about-keys-details).
// So this signer's scheme is audit.AlgECDSAP256 and nothing else. Open refuses a key of any other
// type or curve (ErrNoP256) instead of signing under a different scheme. A deployment that needs
// Ed25519 in a cloud service uses signer/awskms or the PKCS#11 signer.
//
// # What it signs with
//
// The Sign operation is "sign hash": the service does not hash (https://learn.microsoft.com/azure/key-vault/keys/about-keys-details,
// "Sign and Verify"). The signer hashes the message with SHA-256 and sends the 32-byte digest with
// algorithm ES256 ("ECDSA for SHA-256 digests and keys created with curve P-256",
// https://learn.microsoft.com/rest/api/keyvault/keys/sign/sign). ES256 is the RFC 7518 section 3.4
// algorithm, whose signature is the 64-byte concatenation r||s, 32 big-endian bytes each, not ASN.1
// DER. Microsoft's pages point at RFC 7518 for ES256 and do not restate the encoding, so the signer
// does not rely on it silently: it accepts exactly 64 bytes, converts them with
// audit.ECDSAP256SignatureFromRaw (which folds a high s to the low s that audit verifies), and
// checks the result against the public key before returning it. A response in any other encoding
// fails that check and is an error, never a signature.
//
// # The key identifier is pinned
//
// Config.KeyID is a full key identifier with its version, such as
// https://myvault.vault.azure.net/keys/bide-audit/0123456789abcdef0123456789abcdef. A base
// identifier (no version) is refused: it would name "the latest version", and a rotation or a
// newly created version would then change which key signs. Open reads that exact version, and
// every Sign names that version.
//
// # What Open checks
//
// Open reads the key (Get Key, https://learn.microsoft.com/rest/api/keyvault/keys/get-key/get-key)
// and refuses it unless all of these hold:
//
//   - key type EC-HSM (HSM-protected; vault Premium or Managed HSM) or, only with
//     Config.AllowSoftwareKey, EC; curve P-256; a valid public point;
//   - attributes: enabled, now not before nbf and before exp, not exportable (an exportable key can be
//     released out of the service under its release policy, so its private key is not confined to it),
//     and, for HSM-required use, an hsmPlatform other than "0" (a FIPS 140-2 Level 1 software module);
//   - key_ops includes "sign";
//   - the identifier the service returns names the pinned name and version.
//
// It then signs once, to prove the caller's keys/sign permission and that the service's output
// verifies under the public key it reported (one billed operation).
//
// # HSM by default
//
// Key Vault standard-tier EC keys are protected by software; Premium-tier EC-HSM keys and every
// Managed HSM key are protected by an HSM. The signer requires an HSM-protected key unless
// Config.AllowSoftwareKey is set, because a signer for audit logs under a bank-grade custody
// policy should not quietly accept a software key. Setting AllowSoftwareKey is an explicit
// decision, and the key is then in a software module of the service.
//
// # Credentials
//
// The signer takes no credential, token, secret or certificate path as configuration. By default
// it authenticates with azidentity.NewDefaultAzureCredential, which reads the standard Azure
// environment (workload identity, managed identity, Azure CLI sign-in, AZURE_CLIENT_* variables).
// Config.Credential accepts any other azidentity credential, such as a managed identity
// credential bound to one identity; Config.Client replaces the whole client (for a test).
// Grant the identity the "Key Vault Crypto User" role (or the keys/get and keys/sign permissions
// on a legacy access policy; Managed HSM: "Managed HSM Crypto User") on this key only.
//
// # Errors
//
// The Azure SDK's errors print the service's response, and a credential error can print a token
// endpoint's reply. The signer never returns them: a failed call comes back as *Error holding
// the operation, the HTTP status and Azure's error code (for example "Forbidden", "KeyDisabled"),
// and nothing else. Context errors (a timeout) keep their context.DeadlineExceeded or
// context.Canceled identity for errors.Is.
//
// # Use
//
//	s, err := azurekv.Open(ctx, azurekv.Config{
//		KeyID: "https://myvault.vault.azure.net/keys/bide-audit/0123456789abcdef0123456789abcdef",
//	})
//	if err != nil { ... }
//	store, err := audit.NewAuditedStore(inner, s, anchor)
package azurekv

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// DefaultTimeout bounds each Key Vault call when Config.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// selfTest is the message Open signs once.
const selfTest = "bide.signer.selftest.v1"

// Errors a caller can test with errors.Is.
var (
	// ErrNoP256 means the key is not an ECDSA P-256 signing key: another key type, another curve,
	// or a public point that is not on P-256.
	ErrNoP256 = errors.New("azurekv: the key is not an ECDSA P-256 signing key")
	// ErrKeyUnusable means the key cannot sign now: it is disabled, not yet valid, expired, or its
	// key_ops lack "sign".
	ErrKeyUnusable = errors.New("azurekv: the key cannot be used for signing")
	// ErrNotHSM means the key is software-protected and Config.AllowSoftwareKey is not set.
	ErrNotHSM = errors.New("azurekv: the key is not HSM-protected")
	// ErrExportable means the key is marked exportable, so its private key can leave the service.
	ErrExportable = errors.New("azurekv: the key is exportable")
	// ErrKeyMismatch means the service answered for a different key or version than the pinned one.
	ErrKeyMismatch = errors.New("azurekv: the service answered for a different key than the pinned identifier")
)

// Error is a failed Key Vault or credential call, reduced to what is safe to log: the operation, the
// HTTP status and Azure's error code. It carries no response body, header, URL or token.
type Error struct {
	// Op is the operation: "get key" or "sign".
	Op string
	// Status is the HTTP status, or zero when there was no response (a credential or transport failure).
	Status int
	// Code is Azure's error code (for example "Forbidden" or "KeyDisabled"), or one of
	// "AuthenticationFailed", "AuthenticationRequired", "Timeout", "Canceled" and "Unclassified".
	Code string
	// cause is only ever context.DeadlineExceeded or context.Canceled.
	cause error
}

// Error describes the failure without any response content.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("azurekv: " + e.Op + " failed")
	if e.Status != 0 {
		fmt.Fprintf(&b, ": HTTP %d", e.Status)
	}
	if e.Code != "" {
		b.WriteString(" (" + e.Code + ")")
	}
	return b.String()
}

// Unwrap returns context.DeadlineExceeded or context.Canceled for a timed-out or canceled call, and
// nil otherwise.
func (e *Error) Unwrap() error { return e.cause }

// codeChars bounds an error code taken from a response.
var codeChars = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,64}$`)

// sanitize reduces err to an *Error.
func sanitize(op string, err error) error {
	e := &Error{Op: op, Code: "Unclassified"}
	var re *azcore.ResponseError
	var af *azidentity.AuthenticationFailedError
	var ar *azidentity.AuthenticationRequiredError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		e.Code, e.cause = "Timeout", context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		e.Code, e.cause = "Canceled", context.Canceled
	case errors.As(err, &ar):
		e.Code = "AuthenticationRequired"
	case errors.As(err, &af):
		e.Code = "AuthenticationFailed"
	case errors.As(err, &re):
		e.Status = re.StatusCode
		e.Code = "Unclassified"
		if codeChars.MatchString(re.ErrorCode) {
			e.Code = re.ErrorCode
		}
	}
	return e
}

// API is the part of the azkeys client the signer uses. *azkeys.Client satisfies it; a test supplies
// a fake, so no test needs Azure.
type API interface {
	GetKey(ctx context.Context, name string, version string, options *azkeys.GetKeyOptions) (azkeys.GetKeyResponse, error)
	Sign(ctx context.Context, name string, version string, parameters azkeys.SignParameters, options *azkeys.SignOptions) (azkeys.SignResponse, error)
}

var _ API = (*azkeys.Client)(nil)

// newAPI builds the Key Vault client; a test replaces it.
var newAPI = func(vaultURL string, cred azcore.TokenCredential) (API, error) {
	return azkeys.NewClient(vaultURL, cred, nil)
}

// Config selects the Key Vault key.
type Config struct {
	// KeyID is the full key identifier with its version:
	// https://<vault>.vault.azure.net/keys/<name>/<32-hex-version> (or <hsm>.managedhsm.azure.net).
	// Required. A base identifier without a version is refused.
	KeyID string
	// AllowSoftwareKey permits a software-protected EC key. The default refuses it (ErrNotHSM): see
	// "HSM by default" in the package comment.
	AllowSoftwareKey bool
	// HostSuffixes lists the DNS suffixes a vault or Managed HSM host may end in. Empty means the
	// Azure public cloud, Azure China and Azure US Government suffixes for vaults and Managed
	// HSMs. Set it for a cloud outside that list, such as Azure Stack Hub.
	HostSuffixes []string
	// Credential authenticates the calls. Optional; nil means azidentity.NewDefaultAzureCredential.
	// Ignored when Client is set.
	Credential azcore.TokenCredential
	// Client calls Key Vault. Optional; nil builds an azkeys.Client for the key's vault from
	// Credential. A test supplies a fake.
	Client API
	// Timeout bounds each Key Vault call; zero means DefaultTimeout.
	Timeout time.Duration
	// Now reports the time checked against the key's not-before and expiry. Optional; nil means
	// time.Now. For a test.
	Now func() time.Time
}

var defaultHostSuffixes = []string{
	".vault.azure.net", ".managedhsm.azure.net",
	".vault.azure.cn", ".managedhsm.azure.cn",
	".vault.usgovcloudapi.net", ".managedhsm.usgovcloudapi.net",
}

var (
	keyNameRE    = regexp.MustCompile(`^[0-9A-Za-z-]{1,127}$`)
	keyVersionRE = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
)

// keyRef is a parsed, pinned key identifier.
type keyRef struct {
	vaultURL, name, version string
}

func (r keyRef) String() string { return r.vaultURL + "/keys/" + r.name + "/" + r.version }

// parseKeyID checks a full, versioned key identifier.
func parseKeyID(id string, suffixes []string) (keyRef, error) {
	bad := func(why string) (keyRef, error) {
		return keyRef{}, fmt.Errorf("azurekv: Config.KeyID %s: %w", why, agent.ErrConfig)
	}
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return bad("must be an https URL such as https://<vault>.vault.azure.net/keys/<name>/<version>")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.Contains(u.Host, ":") {
		return bad("must not carry credentials, a port (even an empty one), a query or a fragment")
	}
	host := strings.ToLower(u.Hostname())
	if len(suffixes) == 0 {
		suffixes = defaultHostSuffixes
	}
	for _, s := range suffixes {
		if s == "" {
			return bad("has an empty entry in Config.HostSuffixes, which would match every host")
		}
	}
	if !slices.ContainsFunc(suffixes, func(s string) bool {
		s = strings.ToLower(s)
		if !strings.HasPrefix(s, ".") {
			return host == s // a suffix without a leading dot names one exact host, never a lookalike
		}
		return strings.HasSuffix(host, s) && len(host) > len(s)
	}) {
		return bad("host " + host + " is not a Key Vault or Managed HSM host (set Config.HostSuffixes for another cloud)")
	}
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if (len(parts) == 2 || len(parts) == 3 && parts[2] == "") && parts[0] == "keys" && keyNameRE.MatchString(parts[1]) {
		return bad("has no version: pin the version (a base identifier would follow rotation)")
	}
	if len(parts) != 3 || parts[0] != "keys" || !keyNameRE.MatchString(parts[1]) || !keyVersionRE.MatchString(parts[2]) {
		return bad("must have the form https://<vault>.vault.azure.net/keys/<name>/<32-hex-version>")
	}
	return keyRef{vaultURL: "https://" + u.Host, name: parts[1], version: parts[2]}, nil
}

// Signer signs with an ECDSA P-256 Key Vault key. It satisfies audit.Signer and is safe for
// concurrent use.
type Signer struct {
	client    API
	ref       keyRef
	timeout   time.Duration
	now       func() time.Time
	pub       []byte
	hsm       bool
	notBefore time.Time // zero when the key sets none
	expires   time.Time // zero when the key sets none
	v         audit.Verifier
}

var _ audit.Signer = (*Signer)(nil)

// Open reads and checks the pinned key (see the package comment) and signs once to prove the key
// and the caller's keys/sign permission.
func Open(ctx context.Context, cfg Config) (*Signer, error) {
	ref, err := parseKeyID(cfg.KeyID, cfg.HostSuffixes)
	if err != nil {
		return nil, err
	}
	s := &Signer{client: cfg.Client, ref: ref, timeout: cfg.Timeout, now: cfg.Now}
	if s.timeout <= 0 {
		s.timeout = DefaultTimeout
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.client == nil {
		cred := cfg.Credential
		if cred == nil {
			if cred, err = azidentity.NewDefaultAzureCredential(nil); err != nil {
				return nil, sanitize("create the credential", err)
			}
		}
		c, err := newAPI(ref.vaultURL, cred)
		if err != nil {
			return nil, fmt.Errorf("azurekv: create the client: %w", agent.ErrConfig)
		}
		s.client = c
	}
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	got, err := s.client.GetKey(cctx, ref.name, ref.version, nil)
	if err != nil {
		return nil, sanitize("get key", err)
	}
	if err := s.adopt(got.KeyBundle, cfg.AllowSoftwareKey); err != nil {
		return nil, err
	}
	// One signature proves the caller may sign with the key and that the service's output verifies
	// under the public key it reported, so a missing permission or a mismatch is found here and not
	// by the first anchor. It is one billed Key Vault operation.
	if _, err := s.Sign([]byte(selfTest)); err != nil {
		return nil, fmt.Errorf("azurekv: self-test signature: %w", err)
	}
	return s, nil
}

// asciiEqualFold compares a and b ignoring ASCII case only. strings.EqualFold folds Unicode, under
// which U+212A (Kelvin sign) equals "k"; an identifier is ASCII, so nothing else may match.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// sameKeyID reports whether the service's identifier names exactly the pinned vault, name and
// version (identifiers are case-insensitive, https://learn.microsoft.com/azure/key-vault/general/about-keys-secrets-certificates).
// An identifier with credentials, a port, a query or a fragment names nothing the signer pinned.
func sameKeyID(id *azkeys.ID, ref keyRef) bool {
	if id == nil {
		return false
	}
	u, err := url.Parse(string(*id))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return false
	}
	if !asciiEqualFold(u.Host, strings.TrimPrefix(ref.vaultURL, "https://")) { // ASCII only: Unicode folding equates U+212A with k
		return false
	}
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	return len(parts) == 3 && asciiEqualFold(parts[0], "keys") && asciiEqualFold(parts[1], ref.name) && asciiEqualFold(parts[2], ref.version)
}

// adopt checks the key bundle and records the public key and the validity window.
func (s *Signer) adopt(kb azkeys.KeyBundle, allowSoftware bool) error {
	k := kb.Key
	if k == nil || k.Kty == nil || k.Crv == nil {
		return fmt.Errorf("azurekv: key %s has no public key material: %w", s.ref.name, ErrNoP256)
	}
	if !sameKeyID(k.KID, s.ref) {
		return fmt.Errorf("azurekv: key %s: %w", s.ref.name, ErrKeyMismatch)
	}
	switch *k.Kty {
	case azkeys.KeyTypeECHSM:
		s.hsm = true
	case azkeys.KeyTypeEC:
		if !allowSoftware {
			return fmt.Errorf("azurekv: key %s is a software EC key; use an EC-HSM key or set Config.AllowSoftwareKey: %w", s.ref.name, ErrNotHSM)
		}
	default:
		return fmt.Errorf("azurekv: key %s has type %q, want EC-HSM or EC: %w", s.ref.name, *k.Kty, ErrNoP256)
	}
	if *k.Crv != azkeys.CurveNameP256 {
		return fmt.Errorf("azurekv: key %s is on curve %q, want P-256: %w", s.ref.name, *k.Crv, ErrNoP256)
	}
	a := kb.Attributes
	if a == nil {
		return fmt.Errorf("azurekv: key %s has no attributes: %w", s.ref.name, ErrKeyUnusable)
	}
	if a.Enabled != nil && !*a.Enabled {
		return fmt.Errorf("azurekv: key %s is disabled: %w", s.ref.name, ErrKeyUnusable)
	}
	if (a.Exportable != nil && *a.Exportable) || kb.ReleasePolicy != nil {
		return fmt.Errorf("azurekv: key %s can be exported under a release policy: %w", s.ref.name, ErrExportable)
	}
	if s.hsm && a.HSMPlatform != nil && *a.HSMPlatform == "0" {
		if !allowSoftware {
			return fmt.Errorf("azurekv: key %s reports hsmPlatform 0, a software module; set Config.AllowSoftwareKey to accept it: %w", s.ref.name, ErrNotHSM)
		}
		s.hsm = false
	}
	if !slices.ContainsFunc(k.KeyOps, func(op *azkeys.KeyOperation) bool { return op != nil && *op == azkeys.KeyOperationSign }) {
		return fmt.Errorf("azurekv: key %s does not allow the sign operation (key_ops): %w", s.ref.name, ErrKeyUnusable)
	}
	if a.NotBefore != nil {
		s.notBefore = *a.NotBefore
	}
	if a.Expires != nil {
		s.expires = *a.Expires
	}
	if err := s.checkWindow(); err != nil {
		return err
	}
	pub, err := p256Point(k.X, k.Y)
	if err != nil {
		return fmt.Errorf("azurekv: key %s: %v: %w", s.ref.name, err, ErrNoP256)
	}
	v, err := audit.NewVerifier(audit.AlgECDSAP256, pub)
	if err != nil {
		return fmt.Errorf("azurekv: key %s: the public point is not on P-256: %w", s.ref.name, ErrNoP256)
	}
	s.pub, s.v = pub, v
	return nil
}

// p256Point builds the 65-byte SEC1 uncompressed point from the JWK coordinates. A coordinate
// shorter than 32 bytes is left-padded (RFC 7518 forbids shortening, and the on-curve check in
// audit.NewVerifier rejects any value that was altered).
func p256Point(x, y []byte) ([]byte, error) {
	if len(x) == 0 || len(x) > 32 || len(y) == 0 || len(y) > 32 {
		return nil, fmt.Errorf("the public coordinates are %d and %d bytes, want at most 32", len(x), len(y))
	}
	pub := make([]byte, 65)
	pub[0] = 4
	copy(pub[1+32-len(x):33], x)
	copy(pub[33+32-len(y):], y)
	return pub, nil
}

// checkWindow refuses a key outside its not-before and expiry dates.
func (s *Signer) checkWindow() error {
	now := s.now()
	if !s.notBefore.IsZero() && now.Before(s.notBefore) {
		return fmt.Errorf("azurekv: key %s is not valid before %s: %w", s.ref.name, s.notBefore.UTC().Format(time.RFC3339), ErrKeyUnusable)
	}
	if !s.expires.IsZero() && !now.Before(s.expires) {
		return fmt.Errorf("azurekv: key %s expired at %s: %w", s.ref.name, s.expires.UTC().Format(time.RFC3339), ErrKeyUnusable)
	}
	return nil
}

// Alg reports the signature scheme, audit.AlgECDSAP256.
func (s *Signer) Alg() audit.Alg { return audit.AlgECDSAP256 }

// PublicKey returns the 65-byte uncompressed P-256 point, in the encoding audit.NewVerifier reads.
// It is read once, when the Signer opens.
func (s *Signer) PublicKey() []byte { return bytes.Clone(s.pub) }

// KeyID is the pinned key identifier, with its version.
func (s *Signer) KeyID() string { return s.ref.String() }

// HSMProtected reports whether the key is HSM-protected (key type EC-HSM, and not hsmPlatform 0).
func (s *Signer) HSMProtected() bool { return s.hsm }

// Sign returns the service's signature over the SHA-256 digest of message, as the canonical low-S
// r||s that audit verifies. It refuses a key outside its not-before or expiry dates without calling
// the service, and checks the signature against the public key before returning it, so a wrong
// answer is an error here and not a signature that fails to verify later.
func (s *Signer) Sign(message []byte) ([]byte, error) {
	if err := s.checkWindow(); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(message)
	alg := azkeys.SignatureAlgorithmES256
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	out, err := s.client.Sign(ctx, s.ref.name, s.ref.version, azkeys.SignParameters{Algorithm: &alg, Value: digest[:]}, nil)
	if err != nil {
		return nil, sanitize("sign", err)
	}
	if !sameKeyID(out.KID, s.ref) {
		return nil, fmt.Errorf("azurekv: sign: %w", ErrKeyMismatch)
	}
	sig, err := audit.ECDSAP256SignatureFromRaw(out.Result)
	if err != nil {
		return nil, fmt.Errorf("azurekv: the service's signature from %s is not a 64-byte r||s ECDSA signature (%d bytes)", s.ref.name, len(out.Result))
	}
	if s.v == nil || !s.v.Verify(message, sig) {
		return nil, fmt.Errorf("azurekv: the service's signature does not verify under the key's public key (was the key replaced?)")
	}
	return bytes.Clone(sig), nil
}

// String describes the signer by its key identity (audit.KeyID of the public key).
func (s *Signer) String() string {
	return fmt.Sprintf("azurekv.Signer{%s %s}", s.ref, audit.KeyID(string(audit.AlgECDSAP256), s.pub))
}

// Format writes String for every verb.
func (s *Signer) Format(f fmt.State, _ rune) { io.WriteString(f, s.String()) }
