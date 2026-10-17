package awss3

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The key layout. Every anchored head is one object, and its key is derived from the head alone, so
// "the latest head of a trail" and "the heads of size N" are prefix listings of the objects
// themselves. There is no index object an attacker could edit or an operator could forget to update.
//
//	<prefix><partition>/<inverted size>-<head digest>.json
//
// The partition is the lower-case hex SHA-256 of the layout tag, the tree kind and the run or trail
// ID, so a listing of the bucket names no run or trail. The inverted size is MaxInt64 minus the
// head's size, in 19 decimal digits: S3 lists keys in ascending order, so the largest (newest)
// head of a partition sorts first and "latest" is a listing of one key. The digest is the first 16
// bytes (32 hex digits) of the SHA-256 of the object's body, so the same head is always the same
// key (a retry is idempotent) and two different heads of one size (two processes anchoring one run
// around a lease handoff) are two objects, neither overwriting the other.
const (
	// ObjectFormat is the format of an anchored object's body.
	ObjectFormat = "bide.audit.s3-anchor.v1"

	layoutTag    = "bide.audit.s3-anchor.partition.v1\x00"
	sizeDigits   = 19
	digestHex    = 32
	objectSuffix = ".json"
)

// partitionOf is the partition name of a tree kind and run ID.
func partitionOf(kind, runID string) string {
	h := sha256.New()
	h.Write([]byte(layoutTag))
	// Length-prefixed so that no (kind, run ID) pair shares an encoding with another.
	fmt.Fprintf(h, "%d:%s%d:%s", len(kind), kind, len(runID), runID)
	return hex.EncodeToString(h.Sum(nil))
}

// invertedSize is the sortable, fixed-width form of a head size: larger sizes sort first.
func invertedSize(size int) string {
	return fmt.Sprintf("%0*d", sizeDigits, uint64(math.MaxInt64)-uint64(size))
}

// objectName is the name of a body in its partition: inverted size, then its digest.
func objectName(size int, body []byte) string {
	sum := sha256.Sum256(body)
	return invertedSize(size) + "-" + hex.EncodeToString(sum[:16]) + objectSuffix
}

// keyOf is the full key of a body for a head of the given kind, run and size.
func keyOf(prefix, kind, runID string, size int, body []byte) string {
	return prefix + partitionOf(kind, runID) + "/" + objectName(size, body)
}

// parsedName is the parts of an object name.
type parsedName struct {
	size   int
	digest string
}

// parseName splits "<inverted size>-<digest>.json". ok is false for any other name.
func parseName(name string) (parsedName, bool) {
	rest, found := strings.CutSuffix(name, objectSuffix)
	if !found {
		return parsedName{}, false
	}
	inv, digest, found := strings.Cut(rest, "-")
	if !found || len(inv) != sizeDigits || len(digest) != digestHex {
		return parsedName{}, false
	}
	if _, err := hex.DecodeString(digest); err != nil || strings.ToLower(digest) != digest {
		return parsedName{}, false
	}
	for _, c := range inv {
		if c < '0' || c > '9' {
			return parsedName{}, false
		}
	}
	n, err := strconv.ParseUint(inv, 10, 63)
	if err != nil {
		return parsedName{}, false
	}
	return parsedName{size: int(uint64(math.MaxInt64) - n), digest: digest}, true
}

// splitKey splits a key under prefix into its partition and name.
func splitKey(prefix, key string) (partition, name string, ok bool) {
	rest, found := strings.CutPrefix(key, prefix)
	if !found {
		return "", "", false
	}
	partition, name, found = strings.Cut(rest, "/")
	if !found || len(partition) != 64 || strings.Contains(name, "/") {
		return "", "", false
	}
	return partition, name, true
}
