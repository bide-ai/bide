package gcs

import (
	"context"
	"errors"
	"time"
)

// Store is the part of Cloud Storage this package uses, for one bucket. NewStore adapts a
// *storage.Client to it. It has no method that deletes an object, shortens a retention, releases a
// hold or changes the bucket, so the package cannot call one; MethodPermissions names the IAM
// permission each method needs, and a test checks that the writer and reader roles grant exactly
// those.
type Store interface {
	// Bucket returns the bucket's configuration.
	Bucket(ctx context.Context) (BucketInfo, error)
	// Create writes body as a new object and fails with ErrExists (and writes nothing) when a live
	// object of that name exists (ifGenerationMatch=0). It returns what Cloud Storage reports.
	Create(ctx context.Context, name string, body []byte, o CreateOptions) (ObjectInfo, error)
	// Stat returns the metadata of one generation of an object, or of its live generation when
	// generation is 0. A missing object or generation is an error wrapping ErrNoObject.
	Stat(ctx context.Context, name string, generation int64) (ObjectInfo, error)
	// Read returns the first limit+1 bytes at most of one generation (the live one when 0) and its metadata.
	Read(ctx context.Context, name string, generation int64, limit int64) ([]byte, ObjectInfo, error)
	// List calls fn for every object whose name starts with prefix, in name order and, within a name,
	// in increasing generation order, until fn returns false. With versions it also lists noncurrent
	// generations (an object that was overwritten or deleted while versioning is on).
	List(ctx context.Context, prefix string, versions bool, fn func(ObjectInfo) bool) error
}

// Permissions
//
// MethodPermissions maps each Store method to the IAM permissions it needs. The permission a call
// needs is a fact about Cloud Storage (checked against its documentation); the table is the one place
// this package states it.
var MethodPermissions = map[string][]string{
	"Bucket": {"storage.buckets.get"},
	"Create": {"storage.objects.create"},
	"Stat":   {"storage.objects.get"},
	"Read":   {"storage.objects.get"},
	"List":   {"storage.objects.list"},
}

// Errors a Store returns, wrapped, for the package to branch on.
var (
	// ErrExists is Create's answer to a name that holds a live object (HTTP 412).
	ErrExists = errors.New("gcs: object exists")
	// ErrNoObject is the answer to a name or generation that does not exist (HTTP 404).
	ErrNoObject = errors.New("gcs: no such object")
	// ErrDenied is the answer to a call the caller's identity may not make (HTTP 401 or 403).
	ErrDenied = errors.New("gcs: permission denied")
)

// BucketInfo is a bucket's configuration, in the terms the anchor checks.
type BucketInfo struct {
	// ProjectNumber is the number of the project that owns the bucket. Bucket names are global and a
	// deleted bucket's name can be created again by anyone, so the number is what says the bucket is
	// the one you meant.
	ProjectNumber uint64
	// RetentionPeriod is the bucket's retention policy period, zero when there is no policy.
	RetentionPeriod time.Duration
	// RetentionLocked reports that the retention policy is locked: it can no longer be removed or shortened.
	RetentionLocked bool
	Versioning      bool
	// DefaultKMSKey is the bucket's default Cloud KMS key (a key name, without a version), or "".
	DefaultKMSKey string
	// UniformAccess is uniform bucket-level access: only IAM, no object ACLs.
	UniformAccess bool
	// PublicAccessPrevented is public access prevention in the enforced state.
	PublicAccessPrevented bool
	// ObjectRetention reports that per-object retention can be set on objects of the bucket.
	ObjectRetention bool
	// SoftDeleteDuration is the bucket's soft delete retention, zero when soft delete is off.
	SoftDeleteDuration time.Duration
}

// ObjectRetention is a per-object retention configuration.
type ObjectRetention struct {
	Mode        string // "Locked" or "Unlocked"
	RetainUntil time.Time
}

// ObjectInfo is what Cloud Storage reports about one generation of an object.
type ObjectInfo struct {
	Name       string
	Generation int64
	Size       int64
	Created    time.Time
	// Noncurrent is when the generation stopped being the live one (it was overwritten or deleted),
	// zero while it is live.
	Noncurrent time.Time
	CRC32C     uint32
	// KMSKey is the Cloud KMS key version that encrypts the object, or "".
	KMSKey string
	// RetentionExpiry is when the bucket's retention policy lets the generation be deleted; zero when
	// no policy covers it.
	RetentionExpiry time.Time
	// Retention is the per-object retention, nil when none is set.
	Retention     *ObjectRetention
	TemporaryHold bool
	EventHold     bool
}

// CreateOptions are the attributes Create writes with an object.
type CreateOptions struct {
	KMSKey        string           // Cloud KMS key name; "" for the bucket default
	TemporaryHold bool             // place a temporary hold
	Retention     *ObjectRetention // per-object retention (Locked mode), or nil
}
