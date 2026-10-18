package awss3

import (
	"github.com/bide-ai/bide/audit/anchorobj"
)

// The key layout is audit/anchorobj's (see its package documentation for the format): every anchored
// head is one object whose key is derived from the head alone,
//
//	<prefix><partition>/<inverted size>-<head digest>.json
//
// This package contributes what must differ between backends, the partition tag and the object format
// (see layout below), and keeps thin unexported wrappers so that the rest of the package reads as it
// did.
const (
	// ObjectFormat is the format of an anchored object's body.
	ObjectFormat = "bide.audit.s3-anchor.v1"

	layoutTag    = "bide.audit.s3-anchor.partition.v1\x00"
	sizeDigits   = anchorobj.SizeDigits
	digestHex    = anchorobj.DigestHex
	objectSuffix = anchorobj.ObjectSuffix
)

// layout is this backend's naming and encoding of anchored heads.
var layout = anchorobj.Layout{Format: ObjectFormat, PartitionTag: layoutTag, ErrObject: ErrObject, ErrBadSignature: ErrBadSignature}

// partitionOf is the partition name of a tree kind and run ID.
func partitionOf(kind, runID string) string { return layout.Partition(kind, runID) }

// invertedSize is the sortable, fixed-width form of a head size: larger sizes sort first.
func invertedSize(size int) string { return anchorobj.InvertedSize(size) }

// objectName is the name of a body in its partition: inverted size, then its digest.
func objectName(size int, body []byte) string { return anchorobj.ObjectName(size, body) }

// keyOf is the full key of a body for a head of the given kind, run and size.
func keyOf(prefix, kind, runID string, size int, body []byte) string {
	return layout.Key(prefix, kind, runID, size, body)
}

// parsedName is the parts of an object name.
type parsedName struct {
	size   int
	digest string
}

// parseName splits "<inverted size>-<digest>.json". ok is false for any other name.
func parseName(name string) (parsedName, bool) {
	n, ok := anchorobj.ParseName(name)
	return parsedName{size: n.Size, digest: n.Digest}, ok
}

// splitKey splits a key under prefix into its partition and name.
func splitKey(prefix, key string) (partition, name string, ok bool) {
	return anchorobj.SplitKey(prefix, key)
}
