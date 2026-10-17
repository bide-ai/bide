// Package awss3test is an in-memory S3 bucket with versioning and Object Lock, for testing code that
// uses the awss3 package without a network. It implements the awss3.Client methods the way S3
// behaves for them: conditional writes, versions, locks that refuse a delete, an expected-owner
// check on every request, and faults that can be injected before or after a write commits.
package awss3test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Version is one stored version of a key.
type Version struct {
	ID            string
	Body          []byte
	Marker        bool // a delete marker
	LockMode      types.ObjectLockMode
	RetainUntil   time.Time
	LegalHold     types.ObjectLockLegalHoldStatus
	SSE           types.ServerSideEncryption
	KMSKey        string
	ChecksumSHA25 string
}

// Fake is a bucket. The zero value is not usable; use New.
type Fake struct {
	mu sync.Mutex

	// Configuration, set by a test before use (New sets a correct anchor bucket).
	Bucket        string
	Owner         string // the account that owns the bucket
	Versioning    types.BucketVersioningStatus
	LockEnabled   bool
	DefaultMode   types.ObjectLockRetentionMode // "" for no default retention
	DefaultDays   int32
	DefaultKMS    bool // the bucket's default encryption is SSE-KMS
	NoEncryptRead bool // GetBucketEncryption fails with AccessDenied

	// Clock is the bucket's time (default time.Now).
	Clock func() time.Time

	// Hook is called before each request with its operation name and a 1-based count of calls to that
	// operation. A non-nil error is returned instead of performing it.
	Hook func(op string, n int) error
	// AfterCommit is called after a PutObject has stored its version. A non-nil error is returned in
	// place of the success response: a lost response.
	AfterCommit func(n int) error
	// Tamper, when set, rewrites what GetObject returns for a key (a stale or damaged read).
	Tamper func(key string, v *Version)

	keys   map[string][]*Version
	order  []string
	calls  map[string]int
	nextID int
	// Seen records the ExpectedBucketOwner of every request, by operation.
	Seen map[string][]string
}

// New returns a bucket that is a correct anchor bucket: owned by account "111122223333", versioned,
// with Object Lock and a 365-day compliance default retention, and SSE-KMS default encryption.
func New(bucket string) *Fake {
	return &Fake{Bucket: bucket, Owner: "111122223333", Versioning: types.BucketVersioningStatusEnabled,
		LockEnabled: true, DefaultMode: types.ObjectLockRetentionModeCompliance, DefaultDays: 365, DefaultKMS: true,
		keys: map[string][]*Version{}, calls: map[string]int{}, Seen: map[string][]string{}}
}

func (f *Fake) now() time.Time {
	if f.Clock != nil {
		return f.Clock()
	}
	return time.Now()
}

type apiErr struct {
	code, msg string
	status    int
}

func (e *apiErr) Error() string                 { return e.code + ": " + e.msg }
func (e *apiErr) ErrorCode() string             { return e.code }
func (e *apiErr) ErrorMessage() string          { return e.msg }
func (e *apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }
func (e *apiErr) HTTPStatusCode() int           { return e.status }

// APIError returns an S3-style error with the given code and HTTP status.
func APIError(code string, status int) error { return &apiErr{code, code, status} }

// begin records a call and applies the owner check and the hook. It holds the lock on success.
func (f *Fake) begin(op string, bucket, owner *string) error {
	f.mu.Lock()
	f.calls[op]++
	n := f.calls[op]
	f.Seen[op] = append(f.Seen[op], aws.ToString(owner))
	if f.Hook != nil {
		hook := f.Hook
		f.mu.Unlock()
		err := hook(op, n)
		f.mu.Lock()
		if err != nil {
			f.mu.Unlock()
			return err
		}
	}
	if aws.ToString(bucket) != f.Bucket {
		f.mu.Unlock()
		return APIError("NoSuchBucket", 404)
	}
	if o := aws.ToString(owner); o != "" && o != f.Owner {
		f.mu.Unlock()
		return APIError("AccessDenied", 403)
	}
	return nil
}

// Calls is the number of requests of an operation so far.
func (f *Fake) Calls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

// HeadBucket checks the bucket and the expected owner.
func (f *Fake) HeadBucket(_ context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	if err := f.begin("HeadBucket", in.Bucket, in.ExpectedBucketOwner); err != nil {
		return nil, err
	}
	f.mu.Unlock()
	return &s3.HeadBucketOutput{}, nil
}

// GetBucketVersioning returns the bucket's versioning status.
func (f *Fake) GetBucketVersioning(_ context.Context, in *s3.GetBucketVersioningInput, _ ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error) {
	if err := f.begin("GetBucketVersioning", in.Bucket, in.ExpectedBucketOwner); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	return &s3.GetBucketVersioningOutput{Status: f.Versioning}, nil
}

// GetObjectLockConfiguration returns the bucket's Object Lock configuration, or the not-found error S3 returns when Object Lock is not enabled.
func (f *Fake) GetObjectLockConfiguration(_ context.Context, in *s3.GetObjectLockConfigurationInput, _ ...func(*s3.Options)) (*s3.GetObjectLockConfigurationOutput, error) {
	if err := f.begin("GetObjectLockConfiguration", in.Bucket, in.ExpectedBucketOwner); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	if !f.LockEnabled {
		return nil, APIError("ObjectLockConfigurationNotFoundError", 404)
	}
	cfg := &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled}
	if f.DefaultMode != "" {
		cfg.Rule = &types.ObjectLockRule{DefaultRetention: &types.DefaultRetention{Mode: f.DefaultMode, Days: aws.Int32(f.DefaultDays)}}
	}
	return &s3.GetObjectLockConfigurationOutput{ObjectLockConfiguration: cfg}, nil
}

// GetBucketEncryption returns the bucket's default encryption, or AccessDenied when NoEncryptRead is set.
func (f *Fake) GetBucketEncryption(_ context.Context, in *s3.GetBucketEncryptionInput, _ ...func(*s3.Options)) (*s3.GetBucketEncryptionOutput, error) {
	if err := f.begin("GetBucketEncryption", in.Bucket, in.ExpectedBucketOwner); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	if f.NoEncryptRead {
		return nil, APIError("AccessDenied", 403)
	}
	alg := types.ServerSideEncryptionAes256
	if f.DefaultKMS {
		alg = types.ServerSideEncryptionAwsKms
	}
	return &s3.GetBucketEncryptionOutput{ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
		Rules: []types.ServerSideEncryptionRule{{ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: alg}}}}}, nil
}

// PutObject stores a new version, honouring If-None-Match, the lock headers and SSE-KMS, as S3 does.
func (f *Fake) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if err := f.begin("PutObject", in.Bucket, in.ExpectedBucketOwner); err != nil {
		return nil, err
	}
	key := aws.ToString(in.Key)
	body, err := io.ReadAll(in.Body)
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}
	sum := sha256.Sum256(body)
	sumB64 := base64.StdEncoding.EncodeToString(sum[:])
	if in.IfNoneMatch != nil && aws.ToString(in.IfNoneMatch) == "*" {
		if vs := f.keys[key]; len(vs) > 0 && !vs[len(vs)-1].Marker {
			f.mu.Unlock()
			return nil, APIError("PreconditionFailed", 412)
		}
	}
	if in.ObjectLockMode != "" && !f.LockEnabled {
		f.mu.Unlock()
		return nil, APIError("InvalidRequest", 400)
	}
	f.nextID++
	v := &Version{ID: "v" + strconv.Itoa(f.nextID), Body: body, SSE: in.ServerSideEncryption, KMSKey: aws.ToString(in.SSEKMSKeyId), ChecksumSHA25: sumB64}
	if f.Versioning != types.BucketVersioningStatusEnabled {
		v.ID = "null"
	}
	if v.SSE == types.ServerSideEncryptionAwsKms && !strings.HasPrefix(v.KMSKey, "arn:") {
		v.KMSKey = "arn:aws:kms:eu-west-1:" + f.Owner + ":key/" + strings.TrimPrefix(v.KMSKey, "alias/")
	}
	switch {
	case in.ObjectLockMode != "":
		v.LockMode = in.ObjectLockMode
		v.RetainUntil = aws.ToTime(in.ObjectLockRetainUntilDate)
	case f.DefaultMode != "":
		v.LockMode = types.ObjectLockMode(f.DefaultMode)
		v.RetainUntil = f.now().Add(time.Duration(f.DefaultDays) * 24 * time.Hour)
	}
	v.LegalHold = in.ObjectLockLegalHoldStatus
	if _, ok := f.keys[key]; !ok {
		f.order = append(f.order, key)
		sort.Strings(f.order)
	}
	f.keys[key] = append(f.keys[key], v)
	after, n := f.AfterCommit, f.calls["PutObject"]
	f.mu.Unlock()
	if after != nil {
		if err := after(n); err != nil {
			return nil, err
		}
	}
	return &s3.PutObjectOutput{VersionId: aws.String(v.ID), ChecksumSHA256: aws.String(sumB64), ServerSideEncryption: v.SSE, SSEKMSKeyId: aws.String(v.KMSKey)}, nil
}

func (f *Fake) find(key, version string) (*Version, bool) {
	vs := f.keys[key]
	if version == "" {
		if len(vs) == 0 || vs[len(vs)-1].Marker {
			return nil, false
		}
		return vs[len(vs)-1], true
	}
	for _, v := range vs {
		if v.ID == version && !v.Marker {
			return v, true
		}
	}
	return nil, false
}

// GetObject returns a version (the current one without a VersionId) with its lock and encryption metadata.
func (f *Fake) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if err := f.begin("GetObject", in.Bucket, in.ExpectedBucketOwner); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	key := aws.ToString(in.Key)
	v, ok := f.find(key, aws.ToString(in.VersionId))
	if !ok {
		return nil, APIError("NoSuchKey", 404)
	}
	c := *v
	if f.Tamper != nil {
		f.Tamper(key, &c)
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(c.Body)), VersionId: aws.String(c.ID),
		ObjectLockMode: c.LockMode, ObjectLockRetainUntilDate: timeOrNil(c.RetainUntil), ObjectLockLegalHoldStatus: c.LegalHold,
		ServerSideEncryption: c.SSE, SSEKMSKeyId: aws.String(c.KMSKey), ChecksumSHA256: aws.String(c.ChecksumSHA25)}, nil
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

const pageSize = 2 // small, so tests exercise pagination

// ListObjectVersions lists versions and delete markers, each key's newest first, in pages of two.
func (f *Fake) ListObjectVersions(_ context.Context, in *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	if err := f.begin("ListObjectVersions", in.Bucket, in.ExpectedBucketOwner); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	type entry struct {
		key string
		v   *Version
	}
	var all []entry
	for _, k := range f.order {
		if !strings.HasPrefix(k, aws.ToString(in.Prefix)) {
			continue
		}
		vs := f.keys[k]
		for i := len(vs) - 1; i >= 0; i-- { // each key's newest version first, as S3 lists them
			all = append(all, entry{k, vs[i]})
		}
	}
	start := 0
	if km := aws.ToString(in.KeyMarker); km != "" {
		for i, e := range all {
			if e.key == km && e.v.ID == aws.ToString(in.VersionIdMarker) {
				start = i + 1
				break
			}
		}
	}
	out := &s3.ListObjectVersionsOutput{}
	for i := start; i < len(all); i++ {
		if i-start == pageSize {
			out.IsTruncated = aws.Bool(true)
			last := all[i-1]
			out.NextKeyMarker, out.NextVersionIdMarker = aws.String(last.key), aws.String(last.v.ID)
			break
		}
		e := all[i]
		if e.v.Marker {
			out.DeleteMarkers = append(out.DeleteMarkers, types.DeleteMarkerEntry{Key: aws.String(e.key), VersionId: aws.String(e.v.ID)})
		} else {
			out.Versions = append(out.Versions, types.ObjectVersion{Key: aws.String(e.key), VersionId: aws.String(e.v.ID)})
		}
	}
	return out, nil
}

// Attacker operations, for tests of what a locked bucket refuses.

// Delete deletes a version as S3 does: a version still under compliance retention, or under governance
// retention without bypass, or under a legal hold, is refused (AccessDenied). Without a version it
// adds a delete marker, which is allowed and leaves the locked versions.
func (f *Fake) Delete(key, version string, bypassGovernance bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if version == "" {
		f.nextID++
		f.keys[key] = append(f.keys[key], &Version{ID: "v" + strconv.Itoa(f.nextID), Marker: true})
		return nil
	}
	for i, v := range f.keys[key] {
		if v.ID != version {
			continue
		}
		locked := v.RetainUntil.After(f.now())
		switch {
		case v.LegalHold == types.ObjectLockLegalHoldStatusOn:
			return APIError("AccessDenied", 403)
		case locked && v.LockMode == types.ObjectLockModeCompliance:
			return APIError("AccessDenied", 403)
		case locked && v.LockMode == types.ObjectLockModeGovernance && !bypassGovernance:
			return APIError("AccessDenied", 403)
		}
		f.keys[key] = append(f.keys[key][:i:i], f.keys[key][i+1:]...)
		return nil
	}
	return APIError("NoSuchVersion", 404)
}

// Put stores a body under key as another writer would, with no lock (or the bucket default).
func (f *Fake) Put(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	v := &Version{ID: "v" + strconv.Itoa(f.nextID), Body: append([]byte(nil), body...)}
	if f.DefaultMode != "" {
		v.LockMode = types.ObjectLockMode(f.DefaultMode)
		v.RetainUntil = f.now().Add(time.Duration(f.DefaultDays) * 24 * time.Hour)
	}
	if _, ok := f.keys[key]; !ok {
		f.order = append(f.order, key)
		sort.Strings(f.order)
	}
	f.keys[key] = append(f.keys[key], v)
}

// Versions returns the stored versions of key, oldest first.
func (f *Fake) Versions(key string) []Version {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Version
	for _, v := range f.keys[key] {
		out = append(out, *v)
	}
	return out
}

// Keys returns every key that has a current (not deleted) version, in order.
func (f *Fake) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, k := range f.order {
		if _, ok := f.find(k, ""); ok {
			out = append(out, k)
		}
	}
	return out
}
