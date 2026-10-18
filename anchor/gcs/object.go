package gcs

import (
	"errors"
	"fmt"

	"github.com/bide-ai/bide/audit"
)

// Errors a caller can test with errors.Is.
var (
	// ErrUnavailable wraps an error that means Cloud Storage could not be reached or did not answer in
	// time (a transport error, a context that ended, a throttle or a server error). The head may or may
	// not have been written; Publish again with the same head, which is idempotent.
	ErrUnavailable = errors.New("gcs: anchor unavailable")
	// ErrWeak wraps the error of a stored object whose lock, retention or encryption is weaker than
	// the configuration asked for. Publish never returns nil for such an object.
	ErrWeak = errors.New("gcs: stored object is weaker than configured")
	// ErrConflict wraps the error of a name that already holds different bytes than the head being
	// published. A name is derived from the head's digest, so this is never a normal retry.
	ErrConflict = errors.New("gcs: name holds a different object")
	// ErrNotFound is returned by the Reader when a partition holds no head.
	ErrNotFound = errors.New("gcs: no anchored head")
	// ErrBadSignature wraps (with ErrObject) the error of an object that is a well-formed head of its
	// partition whose signature does not verify under the log key: a forgery, a head signed by another
	// key (a rotated one, say), or a re-signed one. The Reader never skips it, since skipping would
	// show an older head as the latest.
	ErrBadSignature = errors.New("gcs: head does not verify under the log key")
	// ErrEquivocation wraps the error of two validly signed heads of one tree and size that commit to
	// different roots: the log key signed two histories. That is evidence of tampering or of a broken
	// writer, and the Reader returns it and never picks one of the two. Two heads of one size with the
	// same root (two processes anchoring one run around a lease handoff) are not equivocation.
	ErrEquivocation = errors.New("gcs: two different signed heads of one size")
	// ErrObject wraps the error of an object that is not a well-formed anchored head: a key and a
	// body that disagree, a body that is not an anchored object, or a head that does not verify.
	ErrObject = errors.New("gcs: object is not a valid anchored head")
)

// encodeObject is the body of the object that anchors sth. It is deterministic: the same head is
// the same bytes, so the same name.
func encodeObject(sth audit.SignedTreeHead) ([]byte, error) {
	b, err := layout.Encode(sth)
	if err != nil {
		return nil, fmt.Errorf("gcs: %w", err)
	}
	return b, nil
}

// decodeObject checks that body is the object the name describes (partition and name) and returns its
// head; see anchorobj.Layout.Decode. Every failure wraps ErrObject.
func decodeObject(partition, name string, body []byte, v audit.Verifier) (audit.SignedTreeHead, error) {
	return layout.Decode(partition, name, body, v)
}
