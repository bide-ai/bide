// Package anchorobj is the part of an object-store audit anchor that does not depend on any storage
// SDK: the layout of the objects that hold signed tree heads, their canonical body, the checks a
// Reader makes on an object it reads back, the equivocation check, and the tripwire for run IDs that
// look like personal data. The anchors in anchor/awss3 and anchor/gcs are built on it, so that a head
// is named, encoded and checked the same way whichever store holds it.
//
// Experimental: the package is shared by the anchor modules and its API may change before 1.0
// (docs/trust/stability.md). It is not an anchor by itself and has no I/O.
//
// # What stays per backend
//
// A Layout names the backend: its object Format and the tag that seeds the partition hash. Both must
// differ between backends, so that an object of one anchor can never validate as an object of another
// and a partition name of one is never a partition name of another. Each backend also supplies its own
// error values (Layout.ErrObject and Layout.ErrBadSignature, and the equivocation error passed to
// CheckEquivocation), so errors.Is against a backend's own errors, and the text of its messages, are
// those of the backend.
//
// # The layout
//
// Every anchored head is one object, and its name is derived from the head alone, so "the latest head
// of a trail" and "the heads of size N" are prefix listings of the objects themselves. There is no index
// object an attacker could edit or an operator could forget to update.
//
//	<prefix><partition>/<inverted size>-<head digest>.json
//
// The partition is the lower-case hex SHA-256 of the layout tag, the tree kind and the run or trail ID,
// so a listing of the bucket names no run or trail. The inverted size is MaxInt64 minus the head's
// size, in 19 decimal digits: stores list names in ascending order, so the largest (newest) head of a
// partition sorts first and "latest" is a listing of one name. The digest is the first 16 bytes (32 hex
// digits) of the SHA-256 of the object's body, so the same head is always the same name (a retry is
// idempotent) and two different heads of one size (two processes anchoring one run around a lease
// handoff) are two objects, neither overwriting the other.
package anchorobj

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/bide-ai/bide/audit"
)

const (
	// SizeDigits is the width of the inverted size in an object name.
	SizeDigits = 19
	// DigestHex is the length of the digest in an object name, in hex digits.
	DigestHex = 32
	// ObjectSuffix ends every object name.
	ObjectSuffix = ".json"
	// PartitionLen is the length of a partition name (a hex SHA-256).
	PartitionLen = 64
)

// Layout is one backend's naming and encoding of anchored heads.
type Layout struct {
	// Format is the format string of the object body, for example "bide.audit.s3-anchor.v1".
	Format string
	// PartitionTag seeds the partition hash. It ends in a NUL byte, so that no tag is a prefix of
	// another, and differs from every other backend's.
	PartitionTag string
	// ErrObject is wrapped by every error Decode returns, and by the reader's other "not a valid
	// anchored object" errors.
	ErrObject error
	// ErrBadSignature is wrapped (with ErrObject) by the error Decode returns for a well-formed head
	// whose signature does not verify.
	ErrBadSignature error
}

// Validate reports why l cannot be used: an empty format, a partition tag that does not end in a NUL
// byte (so that one tag could be a prefix of another), or a missing error value (an error wrapping a nil
// error would match nothing). A backend checks its own Layout, in a test or at start.
func (l Layout) Validate() error {
	switch {
	case l.Format == "":
		return errors.New("anchorobj: Layout.Format is empty")
	case !strings.HasSuffix(l.PartitionTag, "\x00") || len(l.PartitionTag) < 2:
		return errors.New("anchorobj: Layout.PartitionTag must be non-empty and end in a NUL byte")
	case l.ErrObject == nil || l.ErrBadSignature == nil:
		return errors.New("anchorobj: Layout.ErrObject and Layout.ErrBadSignature must be set")
	case errors.Is(l.ErrBadSignature, l.ErrObject) || errors.Is(l.ErrObject, l.ErrBadSignature):
		return errors.New("anchorobj: Layout.ErrBadSignature and Layout.ErrObject must be distinct errors")
	}
	return nil
}

// Collides reports whether two layouts could take each other's objects: the same format or the same
// partition tag. Backends must not collide; only a test that sees both layouts can check it, so each
// backend module tests that its Layout does not collide with the other backends'.
func (l Layout) Collides(other Layout) bool {
	return l.Format == other.Format || l.PartitionTag == other.PartitionTag
}

// Partition is the partition name of a tree kind and run ID.
func (l Layout) Partition(kind, runID string) string {
	h := sha256.New()
	h.Write([]byte(l.PartitionTag))
	// Length-prefixed so that no (kind, run ID) pair shares an encoding with another.
	fmt.Fprintf(h, "%d:%s%d:%s", len(kind), kind, len(runID), runID)
	return hex.EncodeToString(h.Sum(nil))
}

// InvertedSize is the sortable, fixed-width form of a head size: larger sizes sort first.
func InvertedSize(size int) string {
	return fmt.Sprintf("%0*d", SizeDigits, uint64(math.MaxInt64)-uint64(size))
}

// ObjectName is the name of a body within its partition: inverted size, then its digest.
func ObjectName(size int, body []byte) string {
	sum := sha256.Sum256(body)
	return InvertedSize(size) + "-" + hex.EncodeToString(sum[:16]) + ObjectSuffix
}

// Key is the full key of a body for a head of the given kind, run and size.
func (l Layout) Key(prefix, kind, runID string, size int, body []byte) string {
	return prefix + l.Partition(kind, runID) + "/" + ObjectName(size, body)
}

// Name is the parts of an object name.
type Name struct {
	Size   int
	Digest string
}

// ParseName splits "<inverted size>-<digest>.json". ok is false for any other name.
func ParseName(name string) (Name, bool) {
	rest, found := strings.CutSuffix(name, ObjectSuffix)
	if !found {
		return Name{}, false
	}
	inv, digest, found := strings.Cut(rest, "-")
	if !found || len(inv) != SizeDigits || len(digest) != DigestHex {
		return Name{}, false
	}
	if _, err := hex.DecodeString(digest); err != nil || strings.ToLower(digest) != digest {
		return Name{}, false
	}
	for _, c := range inv {
		if c < '0' || c > '9' {
			return Name{}, false
		}
	}
	n, err := strconv.ParseUint(inv, 10, 63)
	if err != nil {
		return Name{}, false
	}
	return Name{Size: int(uint64(math.MaxInt64) - n), Digest: digest}, true
}

// SplitKey splits a key under prefix into its partition and name.
func SplitKey(prefix, key string) (partition, name string, ok bool) {
	rest, found := strings.CutPrefix(key, prefix)
	if !found {
		return "", "", false
	}
	partition, name, found = strings.Cut(rest, "/")
	if !found || len(partition) != PartitionLen || strings.Contains(name, "/") {
		return "", "", false
	}
	return partition, name, true
}

// object is the body of an anchored object.
type object struct {
	Format string               `json:"format"`
	STH    audit.SignedTreeHead `json:"sth"`
}

// Encode is the body of the object that anchors sth. It is deterministic: the same head is the same
// bytes, so the same name.
func (l Layout) Encode(sth audit.SignedTreeHead) ([]byte, error) {
	b, err := json.Marshal(object{Format: l.Format, STH: sth})
	if err != nil {
		return nil, fmt.Errorf("encode head: %w", err)
	}
	return b, nil
}

// Decode checks that body is the object the key names (partition and name) and returns its head. It
// checks the body strictly (no unknown fields), the format, the partition (from the head's kind and run
// ID), the size and the digest the name carries. When v is not nil it also verifies the head's
// signature. Every failure wraps l.ErrObject; a signature that does not verify also wraps
// l.ErrBadSignature. A Layout that fails Validate is refused with that error.
//
// With a nil v, Decode is a structural check only: the result says the object is the canonical
// encoding of a head that its name describes, never that the head is authentic. Every Reader passes
// its Verifier.
func (l Layout) Decode(partition, name string, body []byte, v audit.Verifier) (audit.SignedTreeHead, error) {
	if err := l.Validate(); err != nil {
		return audit.SignedTreeHead{}, err
	}
	pn, ok := ParseName(name)
	if !ok {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %q is not an object name", l.ErrObject, name)
	}
	var o object
	if err := audit.UnmarshalStrict(body, &o); err != nil {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %s/%s: %v", l.ErrObject, partition, name, err)
	}
	if o.Format != l.Format {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %s/%s has format %q, want %q", l.ErrObject, partition, name, o.Format, l.Format)
	}
	sth := o.STH
	if got := l.Partition(sth.Kind, sth.RunID); got != partition {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %s/%s holds a %s head of run %q, which belongs in partition %s", l.ErrObject, partition, name, sth.Kind, sth.RunID, got)
	}
	if sth.Size != pn.Size {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %s/%s is named for size %d but holds a head of size %d", l.ErrObject, partition, name, pn.Size, sth.Size)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:16]) != pn.Digest {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %s/%s: the body does not match the digest in its name", l.ErrObject, partition, name)
	}
	// A body that does not re-encode to itself carries something the head does not (an alias, a
	// duplicate member, different spacing): its digest would not be the head's.
	if again, err := l.Encode(sth); err != nil || !bytes.Equal(again, body) {
		return audit.SignedTreeHead{}, fmt.Errorf("%w: %s/%s is not the canonical encoding of its head", l.ErrObject, partition, name)
	}
	if v != nil {
		if err := sth.Verify(v); err != nil {
			return audit.SignedTreeHead{}, fmt.Errorf("%w: %w: %s/%s: %w", l.ErrObject, l.ErrBadSignature, partition, name, err)
		}
	}
	return sth, nil
}

// CheckEquivocation returns an error wrapping errEquivocation when two heads of one tree and size
// commit to different roots: the log key signed two histories. Two heads of one size with the same
// root (two processes anchoring one run around a lease handoff) are not equivocation. heads need not
// be sorted. They must be the heads of one tree (one kind and run ID): heads of two trees are an error,
// not a verdict, since two runs may share a size.
func CheckEquivocation(heads []audit.SignedTreeHead, errEquivocation error) error {
	if errEquivocation == nil {
		return errors.New("anchorobj: CheckEquivocation needs the equivocation error to wrap")
	}
	first := map[int]audit.SignedTreeHead{}
	for _, h := range heads {
		if h.Kind != heads[0].Kind || h.RunID != heads[0].RunID {
			return fmt.Errorf("anchorobj: CheckEquivocation was given heads of more than one tree (%s %q and %s %q)", heads[0].Kind, heads[0].RunID, h.Kind, h.RunID)
		}
		p, ok := first[h.Size]
		if !ok {
			first[h.Size] = h
			continue
		}
		if !p.SameTree(h.TreeHead) {
			a, b := p.Hash(), h.Hash()
			return fmt.Errorf("%w: %s %q has two different signed heads of size %d (%x and %x): the log key signed two histories", errEquivocation, h.Kind, h.RunID, h.Size, a[:8], b[:8])
		}
	}
	return nil
}
