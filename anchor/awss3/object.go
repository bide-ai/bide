package awss3

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bide-ai/bide/audit"
)

// Errors a caller can test with errors.Is.
var (
	// ErrUnavailable wraps an error that means S3 could not be reached or did not answer in time
	// (a transport error, a context that ended, a throttle or a server error). The head may or may
	// not have been written; Publish again with the same head, which is idempotent.
	ErrUnavailable = errors.New("awss3: anchor unavailable")
	// ErrWeak wraps the error of a stored object whose lock, retention or encryption is weaker than
	// the configuration asked for. Publish never returns nil for such an object.
	ErrWeak = errors.New("awss3: stored object is weaker than configured")
	// ErrConflict wraps the error of a key that already holds different bytes than the head being
	// published. A key is derived from the head's digest, so this is never a normal retry.
	ErrConflict = errors.New("awss3: key holds a different object")
	// ErrNotFound is returned by the Reader when a partition holds no head.
	ErrNotFound = errors.New("awss3: no anchored head")
	// ErrObject wraps the error of an object that is not a well-formed anchored head: a key and a
	// body that disagree, a body that is not an anchored object, or a head that does not verify.
	ErrObject = errors.New("awss3: object is not a valid anchored head")
)

// object is the body of an anchored object.
type object struct {
	Format string               `json:"format"` // ObjectFormat
	STH    audit.SignedTreeHead `json:"sth"`
}

// encodeObject is the body of the object that anchors sth. It is deterministic: the same head is
// the same bytes, so the same key.
func encodeObject(sth audit.SignedTreeHead) ([]byte, error) {
	b, err := json.Marshal(object{Format: ObjectFormat, STH: sth})
	if err != nil {
		return nil, fmt.Errorf("awss3: encode head: %w", err)
	}
	return b, nil
}

// decodeObject checks that body is the object the key names (partition and name) and returns its
// head. It checks the body strictly (no unknown fields), the format, the partition (from the head's
// kind and run ID), the size and the digest the name carries. When v is not nil it also verifies the
// head's signature. Every failure wraps ErrObject.
func decodeObject(partition, name string, body []byte, v audit.Verifier) (audit.SignedTreeHead, error) {
	pn, ok := parseName(name)
	if !ok {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %q is not an object name", ErrObject, name)
	}
	var o object
	if err := audit.UnmarshalStrict(body, &o); err != nil {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %s/%s: %v", ErrObject, partition, name, err)
	}
	if o.Format != ObjectFormat {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %s/%s has format %q, want %q", ErrObject, partition, name, o.Format, ObjectFormat)
	}
	sth := o.STH
	if got := partitionOf(sth.Kind, sth.RunID); got != partition {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %s/%s holds a %s head of run %q, which belongs in partition %s", ErrObject, partition, name, sth.Kind, sth.RunID, got)
	}
	if sth.Size != pn.size {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %s/%s is named for size %d but holds a head of size %d", ErrObject, partition, name, pn.size, sth.Size)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:16]) != pn.digest {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %s/%s: the body does not match the digest in its name", ErrObject, partition, name)
	}
	// A body that does not re-encode to itself carries something the head does not (an alias, a
	// duplicate member, different spacing): its digest would not be the head's.
	if again, err := encodeObject(sth); err != nil || !bytes.Equal(again, body) {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %s/%s is not the canonical encoding of its head", ErrObject, partition, name)
	}
	if v != nil {
		if err := sth.Verify(v); err != nil {
			return audit.SignedTreeHead{}, fmt.Errorf("%w: %s/%s: %w", ErrObject, partition, name, err)
		}
	}
	return sth, nil
}
