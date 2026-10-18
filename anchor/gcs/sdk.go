package gcs

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sdkStore is a Store over a *storage.Client and one bucket.
type sdkStore struct {
	c      *storage.Client
	bucket string
}

// NewStore returns the Store for a bucket of a Cloud Storage client. The client's credentials come
// from Application Default Credentials (or whatever the caller configured); this package never reads
// or logs them. A client built with the HTTP/JSON API reports per-object retention; one built with
// gRPC does not, and Config.ObjectRetention then fails its read-back.
func NewStore(c *storage.Client, bucket string) Store { return &sdkStore{c: c, bucket: bucket} }

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func (s *sdkStore) Bucket(ctx context.Context) (BucketInfo, error) {
	a, err := s.c.Bucket(s.bucket).Attrs(ctx)
	if err != nil {
		return BucketInfo{}, classify(err)
	}
	b := BucketInfo{
		ProjectNumber: a.ProjectNumber, Versioning: a.VersioningEnabled,
		UniformAccess:         a.UniformBucketLevelAccess.Enabled || a.BucketPolicyOnly.Enabled,
		PublicAccessPrevented: a.PublicAccessPrevention == storage.PublicAccessPreventionEnforced,
		ObjectRetention:       a.ObjectRetentionMode != "",
	}
	if a.RetentionPolicy != nil {
		b.RetentionPeriod, b.RetentionLocked = a.RetentionPolicy.RetentionPeriod, a.RetentionPolicy.IsLocked
	}
	if a.Encryption != nil {
		b.DefaultKMSKey = a.Encryption.DefaultKMSKeyName
	}
	if a.SoftDeletePolicy != nil {
		b.SoftDeleteDuration = a.SoftDeletePolicy.RetentionDuration
	}
	return b, nil
}

func (s *sdkStore) Create(ctx context.Context, name string, body []byte, o CreateOptions) (ObjectInfo, error) {
	w := s.c.Bucket(s.bucket).Object(name).If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	w.ContentType = "application/json"
	w.ChunkSize = 0 // one request: the object is a few hundred bytes
	w.CRC32C, w.SendCRC32C = crc32.Checksum(body, castagnoli), true
	w.KMSKeyName = o.KMSKey
	w.TemporaryHold = o.TemporaryHold
	if o.Retention != nil {
		w.Retention = &storage.ObjectRetention{Mode: o.Retention.Mode, RetainUntil: o.Retention.RetainUntil}
	}
	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		return ObjectInfo{}, classify(err)
	}
	if err := w.Close(); err != nil {
		return ObjectInfo{}, classify(err)
	}
	return toInfo(w.Attrs()), nil
}

func (s *sdkStore) handle(name string, generation int64) *storage.ObjectHandle {
	h := s.c.Bucket(s.bucket).Object(name)
	if generation != 0 {
		h = h.Generation(generation)
	}
	return h
}

func (s *sdkStore) Stat(ctx context.Context, name string, generation int64) (ObjectInfo, error) {
	a, err := s.handle(name, generation).Attrs(ctx)
	if err != nil {
		return ObjectInfo{}, classify(err)
	}
	return toInfo(a), nil
}

func (s *sdkStore) Read(ctx context.Context, name string, generation, limit int64) ([]byte, ObjectInfo, error) {
	h := s.handle(name, generation)
	r, err := h.NewReader(ctx)
	if err != nil {
		return nil, ObjectInfo{}, classify(err)
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, ObjectInfo{}, classify(err)
	}
	// Attributes of the generation that was read, not of whichever is live now.
	g := generation
	if g == 0 {
		g = r.Attrs.Generation
	}
	a, err := s.handle(name, g).Attrs(ctx)
	if err != nil {
		return nil, ObjectInfo{}, classify(err)
	}
	return b, toInfo(a), nil
}

func (s *sdkStore) List(ctx context.Context, prefix string, versions bool, fn func(ObjectInfo) bool) error {
	it := s.c.Bucket(s.bucket).Objects(ctx, &storage.Query{Prefix: prefix, Versions: versions})
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return classify(err)
		}
		if !fn(toInfo(a)) {
			return nil
		}
	}
}

func toInfo(a *storage.ObjectAttrs) ObjectInfo {
	if a == nil {
		return ObjectInfo{}
	}
	o := ObjectInfo{
		Name: a.Name, Generation: a.Generation, Size: a.Size, Created: a.Created, Noncurrent: a.Deleted,
		CRC32C: a.CRC32C, KMSKey: a.KMSKeyName, RetentionExpiry: a.RetentionExpirationTime,
		TemporaryHold: a.TemporaryHold, EventHold: a.EventBasedHold,
	}
	if a.Retention != nil {
		o.Retention = &ObjectRetention{Mode: a.Retention.Mode, RetainUntil: a.Retention.RetainUntil}
	}
	return o
}

// classify maps an SDK error to the errors this package branches on: ErrExists (412), ErrNoObject
// (404 and the SDK's not-exist errors), ErrDenied (401, 403), and ErrUnavailable for a context that
// ended, a request that did not complete, a throttle (408, 429) or a server error (5xx). The original
// error stays in the chain.
func classify(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w (%w)", ErrUnavailable, err)
	case errors.Is(err, storage.ErrObjectNotExist), errors.Is(err, storage.ErrBucketNotExist):
		return fmt.Errorf("%w (%w)", ErrNoObject, err)
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch {
		case ge.Code == http.StatusPreconditionFailed:
			return fmt.Errorf("%w (%w)", ErrExists, err)
		case ge.Code == http.StatusNotFound:
			return fmt.Errorf("%w (%w)", ErrNoObject, err)
		case ge.Code == http.StatusUnauthorized, ge.Code == http.StatusForbidden:
			return fmt.Errorf("%w (%w)", ErrDenied, err)
		case ge.Code == http.StatusRequestTimeout, ge.Code == http.StatusTooManyRequests, ge.Code >= http.StatusInternalServerError:
			return fmt.Errorf("%w (%w)", ErrUnavailable, err)
		}
		return err
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.OK {
		switch st.Code() {
		case codes.PermissionDenied, codes.Unauthenticated:
			return fmt.Errorf("%w (%w)", ErrDenied, err)
		case codes.NotFound:
			return fmt.Errorf("%w (%w)", ErrNoObject, err)
		}
	}
	// No API answer: the request did not complete (a transport error, a reset connection, a gRPC
	// status that is not an HTTP one). Treat it as unavailable: the caller publishes again.
	return fmt.Errorf("%w (%w)", ErrUnavailable, err)
}
