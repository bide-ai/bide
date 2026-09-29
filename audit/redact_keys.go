package audit

import (
	"errors"
	"fmt"
	"io"
)

// A type that holds a private key refuses to hand the key to an encoder or a formatter. A signer
// kept in a config struct that is logged, printed with %+v, or marshaled to JSON or gob would
// otherwise publish the key that anchors the audit trail. Each such type formats as a fixed
// string that names it and marks the key redacted, whatever the verb, and encoding it with
// encoding/json or encoding/gob fails: the Ed25519 signer refuses both, the ML-DSA signer refuses
// JSON (gob already rejects its key, which has no exported fields), and a hybrid signer fails
// through its two components. Verifiers hold only public keys and encode as usual.

// errKeyEncoding is returned by the JSON and gob encodings of a type holding a private key.
var errKeyEncoding = errors.New("audit: refusing to encode a private key")

// String returns a description of the signer that does not include the key.
func (Ed25519Signer) String() string { return "audit.Ed25519Signer{Priv:[redacted]}" }

// Format writes String for every verb, so no fmt verb prints the key.
func (s Ed25519Signer) Format(f fmt.State, _ rune) { io.WriteString(f, s.String()) }

// MarshalJSON refuses: the signer holds a private key.
func (Ed25519Signer) MarshalJSON() ([]byte, error) { return nil, errKeyEncoding }

// GobEncode refuses: the signer holds a private key.
func (Ed25519Signer) GobEncode() ([]byte, error) { return nil, errKeyEncoding }

// String returns a description of the signer that does not include the key.
func (MLDSASigner) String() string { return "audit.MLDSASigner{Priv:[redacted]}" }

// Format writes String for every verb, so no fmt verb prints the key.
func (s MLDSASigner) Format(f fmt.State, _ rune) { io.WriteString(f, s.String()) }

// MarshalJSON refuses: the signer holds a private key.
func (MLDSASigner) MarshalJSON() ([]byte, error) { return nil, errKeyEncoding }

// String returns a description of the signer that does not include either key.
func (HybridSigner) String() string { return "audit.HybridSigner{Ed:[redacted], ML:[redacted]}" }

// Format writes String for every verb, so no fmt verb prints either key.
func (s HybridSigner) Format(f fmt.State, _ rune) { io.WriteString(f, s.String()) }

// String returns a description of the store that does not include its signing key.
func (*AuditedStore) String() string { return "audit.AuditedStore{priv:[redacted]}" }

// Format writes String for every verb, so no fmt verb prints the signing key.
func (a *AuditedStore) Format(f fmt.State, _ rune) { io.WriteString(f, a.String()) }
