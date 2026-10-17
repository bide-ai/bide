package awss3

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// maxObjectBytes bounds what the Reader reads for one object: a head is a few hundred bytes plus the
// run ID, and no object this package writes is near this size.
const maxObjectBytes = 1 << 20

// ReaderConfig configures a Reader.
type ReaderConfig struct {
	// Client is the S3 client, with read-only credentials. Required.
	Client Client
	// Bucket is the bucket. Required.
	Bucket string
	// ExpectedBucketOwner is the account that must own the bucket. Required (12 digits).
	ExpectedBucketOwner string
	// Prefix is the prefix the Anchor was configured with (default "bide-anchor/").
	Prefix string
	// Verifier verifies every head's signature: the log's public key. Required; a Reader that does
	// not check signatures would trust anyone able to write the bucket.
	Verifier audit.Verifier
}

// Reader reads anchored heads back from S3 and verifies them. Every head it returns has a valid
// signature under the Verifier, a body that is the canonical encoding of its head, and a key that
// names that head. An object that fails any of that is an error wrapping ErrObject, never skipped:
// a bucket that holds a forged or damaged object is a finding.
type Reader struct {
	c      Client
	bucket string
	owner  string
	prefix string
	v      audit.Verifier
}

// NewReader returns a Reader. It refuses (an error wrapping agent.ErrConfig) an incomplete configuration.
func NewReader(cfg ReaderConfig) (*Reader, error) {
	a, err := newAnchor(Config{Client: cfg.Client, Bucket: cfg.Bucket, ExpectedBucketOwner: cfg.ExpectedBucketOwner,
		Prefix: cfg.Prefix, Retention: MinRetention, EncryptionNone: true})
	if err != nil {
		return nil, err
	}
	if cfg.Verifier == nil {
		return nil, fmt.Errorf("awss3: ReaderConfig.Verifier is nil: %w", agent.ErrConfig)
	}
	return &Reader{c: cfg.Client, bucket: cfg.Bucket, owner: cfg.ExpectedBucketOwner, prefix: a.prefix, v: cfg.Verifier}, nil
}

// list returns the keys under a prefix in S3's order (ascending), following continuation tokens, at most limit (0 for all).
func (r *Reader) list(ctx context.Context, prefix string, limit int) ([]string, error) {
	var keys []string
	var token *string
	for {
		in := &s3.ListObjectsV2Input{Bucket: aws.String(r.bucket), Prefix: aws.String(prefix),
			ExpectedBucketOwner: aws.String(r.owner), ContinuationToken: token}
		if limit > 0 {
			in.MaxKeys = aws.Int32(int32(limit - len(keys)))
		}
		out, err := r.c.ListObjectsV2(ctx, in)
		if err != nil {
			return nil, wrapErr("list "+prefix, err)
		}
		for _, o := range out.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
		if limit > 0 && len(keys) >= limit || !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			return keys, nil
		}
		token = out.NextContinuationToken
	}
}

func (r *Reader) get(ctx context.Context, key string) ([]byte, error) {
	out, err := r.c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key), ExpectedBucketOwner: aws.String(r.owner)})
	if err != nil {
		return nil, wrapErr("read "+key, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(io.LimitReader(out.Body, maxObjectBytes+1))
	if err != nil {
		return nil, wrapErr("read "+key, err)
	}
	if len(b) > maxObjectBytes {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrObject, key, maxObjectBytes)
	}
	return b, nil
}

// load reads and verifies the objects at keys.
func (r *Reader) load(ctx context.Context, keys []string) ([]audit.SignedTreeHead, error) {
	var heads []audit.SignedTreeHead
	for _, key := range keys {
		partition, name, ok := splitKey(r.prefix, key)
		if !ok {
			return nil, fmt.Errorf("%w: key %q is not in this package's layout under %q", ErrObject, key, r.prefix)
		}
		body, err := r.get(ctx, key)
		if err != nil {
			return nil, err
		}
		sth, err := decodeObject(partition, name, body, r.v)
		if err != nil {
			return nil, err
		}
		heads = append(heads, sth)
	}
	return heads, nil
}

// Latest returns the head of the largest size anchored for the tree of the given kind (audit.TreeJournal
// or audit.TreeEvents) and run or trail ID: one listing of one key, then one read. When two heads of that
// size exist (two processes anchored one run around a lease handoff) it returns the one whose key sorts
// first. It returns ErrNotFound when none is anchored.
//
// Latest relies on S3 listing a key as soon as it is written, which S3 guarantees (strong read-after-write
// consistency for listings since December 2020).
func (r *Reader) Latest(ctx context.Context, kind, runID string) (audit.SignedTreeHead, error) {
	keys, err := r.list(ctx, r.prefix+partitionOf(kind, runID)+"/", 1)
	if err != nil {
		return audit.SignedTreeHead{}, err
	}
	if len(keys) == 0 {
		return audit.SignedTreeHead{}, fmt.Errorf("%w for %s %q", ErrNotFound, kind, runID)
	}
	heads, err := r.load(ctx, keys)
	if err != nil {
		return audit.SignedTreeHead{}, err
	}
	return heads[0], nil
}

// Heads returns every head anchored for the tree, in increasing size (heads of one size in key
// order), each verified. A key that is not a head, or a head that is not for this tree, is an error.
func (r *Reader) Heads(ctx context.Context, kind, runID string) ([]audit.SignedTreeHead, error) {
	keys, err := r.list(ctx, r.prefix+partitionOf(kind, runID)+"/", 0)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w for %s %q", ErrNotFound, kind, runID)
	}
	slices.Reverse(keys) // S3 lists the largest size first; callers want them in increasing size
	return r.load(ctx, keys)
}

// Head returns the heads anchored for the tree at exactly the given size (more than one only when two
// processes anchored one run at the same size), or ErrNotFound.
func (r *Reader) Head(ctx context.Context, kind, runID string, size int) ([]audit.SignedTreeHead, error) {
	if size < 0 {
		return nil, fmt.Errorf("awss3: size %d is negative", size)
	}
	keys, err := r.list(ctx, r.prefix+partitionOf(kind, runID)+"/"+invertedSize(size)+"-", 0)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w for %s %q at size %d", ErrNotFound, kind, runID, size)
	}
	return r.load(ctx, keys)
}

// Finding is something Audit found wrong with the bucket's lock state. Each Finding is a reason not
// to rely on the anchor until it is explained.
type Finding struct {
	Key     string // the object, or the partition when the finding is about it
	Version string // the object's version, when the finding is about one
	Problem string
}

func (f Finding) String() string {
	if f.Version != "" {
		return fmt.Sprintf("%s (version %s): %s", f.Key, f.Version, f.Problem)
	}
	return fmt.Sprintf("%s: %s", f.Key, f.Problem)
}

// Report is the result of Audit.
type Report struct {
	Heads    []audit.SignedTreeHead // every head of the tree, in increasing size, each verified
	Findings []Finding              // what is wrong with how they are stored, if anything
}

// Audit reads every version of every object of a tree and reports their lock state: Heads are
// the verified heads, and Findings lists every delete marker, every version that is not locked,
// every version whose retention has ended (it can now be deleted), every governance-mode lock
// (a principal with s3:BypassGovernanceRetention can delete it), every key with more than one
// version (the key is never rewritten by this package), and every object that is not a verified head. It
// needs read access and no write access. An empty Findings with Heads is what an intact
// anchor looks like; it is not proof the journal is intact (check the journal against the heads).
func (r *Reader) Audit(ctx context.Context, kind, runID string, now time.Time) (Report, error) {
	prefix := r.prefix + partitionOf(kind, runID) + "/"
	var rep Report
	versions := map[string]int{}
	var marker, vmarker *string
	for {
		out, err := r.c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(r.bucket), Prefix: aws.String(prefix),
			ExpectedBucketOwner: aws.String(r.owner), KeyMarker: marker, VersionIdMarker: vmarker})
		if err != nil {
			return Report{}, wrapErr("list versions of "+prefix, err)
		}
		for _, d := range out.DeleteMarkers {
			rep.Findings = append(rep.Findings, Finding{Key: aws.ToString(d.Key), Version: aws.ToString(d.VersionId), Problem: "a delete marker: something tried to delete this object (it is hidden from a plain listing, and the locked version is intact)"})
		}
		for _, v := range out.Versions {
			key, ver := aws.ToString(v.Key), aws.ToString(v.VersionId)
			versions[key]++
			f, sth, err := r.auditVersion(ctx, key, ver, now)
			rep.Findings = append(rep.Findings, f...)
			if err != nil {
				return Report{}, err
			}
			if sth != nil {
				rep.Heads = append(rep.Heads, *sth)
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		marker, vmarker = out.NextKeyMarker, out.NextVersionIdMarker
	}
	for key, n := range versions {
		if n > 1 {
			rep.Findings = append(rep.Findings, Finding{Key: key, Problem: fmt.Sprintf("%d versions of one key: a key holds one head, so another write replaced it", n)})
		}
	}
	slices.SortStableFunc(rep.Heads, func(a, b audit.SignedTreeHead) int { return a.Size - b.Size })
	slices.SortFunc(rep.Findings, func(a, b Finding) int { return strings.Compare(a.String(), b.String()) })
	return rep, nil
}

func (r *Reader) auditVersion(ctx context.Context, key, ver string, now time.Time) ([]Finding, *audit.SignedTreeHead, error) {
	out, err := r.c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key), VersionId: aws.String(ver), ExpectedBucketOwner: aws.String(r.owner)})
	if err != nil {
		return nil, nil, wrapErr("read "+key, err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(io.LimitReader(out.Body, maxObjectBytes+1))
	if err != nil {
		return nil, nil, wrapErr("read "+key, err)
	}
	var fs []Finding
	add := func(p string) { fs = append(fs, Finding{Key: key, Version: ver, Problem: p}) }
	switch {
	case out.ObjectLockMode == "":
		add("not locked: it can be deleted or overwritten")
	case out.ObjectLockMode == types.ObjectLockModeGovernance:
		add("locked in governance mode: a principal with s3:BypassGovernanceRetention can delete it")
	}
	if out.ObjectLockMode != "" && (out.ObjectLockRetainUntilDate == nil || !out.ObjectLockRetainUntilDate.After(now)) {
		add("its retention has ended: it can be deleted")
	}
	var sth *audit.SignedTreeHead
	partition, name, ok := splitKey(r.prefix, key)
	if !ok {
		add("the key is not in this package's layout")
		return fs, nil, nil
	}
	if len(body) > maxObjectBytes {
		add("larger than the largest object this package writes")
		return fs, nil, nil
	}
	h, err := decodeObject(partition, name, body, r.v)
	if err != nil {
		add(err.Error())
		return fs, nil, nil
	}
	sth = &h
	return fs, sth, nil
}

// LoadDir reads a downloaded set of anchored objects (for example the output of "aws s3 sync" of the
// anchor prefix) and verifies each against v. The set is every regular file under dir whose path
// ends in <partition>/<object name>; any other file is an error, so a stray or renamed file is not
// ignored. It needs no network. It returns the heads sorted by partition, then size, and cannot show
// the objects' lock state (the files carry none): use Reader.Audit for that.
func LoadDir(dir string, v audit.Verifier) ([]audit.SignedTreeHead, error) {
	if v == nil {
		return nil, fmt.Errorf("awss3: LoadDir needs a Verifier: %w", agent.ErrConfig)
	}
	type item struct {
		partition string
		sth       audit.SignedTreeHead
	}
	var items []item
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrObject, path)
		}
		partition, name := filepath.Base(filepath.Dir(path)), d.Name()
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		body, err := io.ReadAll(io.LimitReader(f, maxObjectBytes+1))
		if err != nil {
			return err
		}
		if len(body) > maxObjectBytes {
			return fmt.Errorf("%w: %s is larger than %d bytes", ErrObject, path, maxObjectBytes)
		}
		sth, err := decodeObject(partition, name, body, v)
		if err != nil {
			return err
		}
		items = append(items, item{partition, sth})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(items, func(a, b item) int {
		if c := strings.Compare(a.partition, b.partition); c != 0 {
			return c
		}
		return a.sth.Size - b.sth.Size
	})
	heads := make([]audit.SignedTreeHead, len(items))
	for i, it := range items {
		heads[i] = it.sth
	}
	return heads, nil
}
