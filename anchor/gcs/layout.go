package gcs

import "github.com/bide-ai/bide/audit/anchorobj"

// The object layout is audit/anchorobj's (see its package documentation): every anchored head is one
// object whose name is derived from the head alone,
//
//	<prefix><partition>/<inverted size>-<head digest>.json
//
// This package contributes what must differ between backends, the partition tag and the object format,
// and keeps thin unexported wrappers.
const (
	// ObjectFormat is the format of an anchored object's body.
	ObjectFormat = "bide.audit.gcs-anchor.v1"

	layoutTag = "bide.audit.gcs-anchor.partition.v1\x00"
)

// layout is this backend's naming and encoding of anchored heads.
var layout = anchorobj.Layout{Format: ObjectFormat, PartitionTag: layoutTag, ErrObject: ErrObject, ErrBadSignature: ErrBadSignature}

// Layout returns this backend's layout: its object format, partition tag and error values. It is a copy;
// changing it changes nothing in the package. A program that links several anchors uses it to check
// that no two layouts collide (anchorobj.Layout.Collides).
func Layout() anchorobj.Layout { return layout }

func partitionOf(kind, runID string) string { return layout.Partition(kind, runID) }

func invertedSize(size int) string { return anchorobj.InvertedSize(size) }

func keyOf(prefix, kind, runID string, size int, body []byte) string {
	return layout.Key(prefix, kind, runID, size, body)
}

// parsedName is the parts of an object name.
type parsedName struct {
	size   int
	digest string
}

func parseName(name string) (parsedName, bool) {
	n, ok := anchorobj.ParseName(name)
	return parsedName{size: n.Size, digest: n.Digest}, ok
}

func splitKey(prefix, key string) (partition, name string, ok bool) {
	return anchorobj.SplitKey(prefix, key)
}
