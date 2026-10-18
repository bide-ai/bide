package awss3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/anchorobj"
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
	// OnSkip is called with each object the Reader skips because it claims to be no head (not in the
	// layout, not the canonical encoding of a head). Without it the finding is logged with log/slog at
	// Warn. An object that claims to be a head and fails its signature is an error, not a skip.
	OnSkip func(Finding)
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
	onSkip func(Finding)
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
	onSkip := cfg.OnSkip
	if onSkip == nil {
		onSkip = func(f Finding) { slog.Warn("awss3: skipped an object that is no anchored head: " + f.String()) }
	}
	return &Reader{c: cfg.Client, bucket: cfg.Bucket, owner: cfg.ExpectedBucketOwner, prefix: a.prefix, v: cfg.Verifier, onSkip: onSkip}, nil
}

// ref is one stored version of a key.
type ref struct{ key, version string }

// scan calls fn for each version under a prefix, in the order the layout sorts them (the largest
// size first, each key's newest version first), following the markers, until fn returns true. It
// lists versions and not only current objects: a delete marker hides a key from a plain listing, and
// the locked version under it is still the anchored head. Delete markers are not passed to fn; Audit
// reports them.
func (r *Reader) scan(ctx context.Context, prefix string, fn func(ref) (stop bool, err error)) error {
	var marker, vmarker *string
	pages := 0
	for {
		out, err := r.c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(r.bucket), Prefix: aws.String(prefix),
			ExpectedBucketOwner: aws.String(r.owner), KeyMarker: marker, VersionIdMarker: vmarker})
		if err != nil {
			return wrapErr("list "+prefix, err)
		}
		for _, v := range out.Versions {
			if stop, err := fn(ref{aws.ToString(v.Key), aws.ToString(v.VersionId)}); stop || err != nil {
				return err
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			return nil
		}
		if err := advance(prefix, &pages, marker, vmarker, out.NextKeyMarker, out.NextVersionIdMarker); err != nil {
			return err
		}
		marker, vmarker = out.NextKeyMarker, out.NextVersionIdMarker
	}
}

func (r *Reader) get(ctx context.Context, x ref) ([]byte, error) {
	out, err := r.c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(x.key), VersionId: aws.String(x.version), ExpectedBucketOwner: aws.String(r.owner)})
	if err != nil {
		return nil, wrapErr("read "+x.key, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(io.LimitReader(out.Body, maxObjectBytes+1))
	if err != nil {
		return nil, wrapErr("read "+x.key, err)
	}
	if len(b) > maxObjectBytes {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrObject, x.key, maxObjectBytes)
	}
	return b, nil
}

// loadOne reads and verifies one object. An object that is not in the layout, or does not decode to
// its canonical encoding, claims to be no head: it is reported through OnSkip and skipped (ok false),
// so that one stray object cannot make the partition unreadable. A well-formed head whose signature
// does not verify is an error (ErrBadSignature), never skipped: it may be a real head signed by
// another key, and skipping it would show an older head as the latest.
func (r *Reader) loadOne(ctx context.Context, x ref) (sth audit.SignedTreeHead, ok bool, err error) {
	skip := func(problem string) (audit.SignedTreeHead, bool, error) {
		r.onSkip(Finding{Key: x.key, Version: x.version, Problem: problem})
		return audit.SignedTreeHead{}, false, nil
	}
	partition, name, laid := splitKey(r.prefix, x.key)
	if !laid {
		return skip("the key is not in this package's layout")
	}
	body, err := r.get(ctx, x)
	switch {
	case errors.Is(err, ErrObject):
		return skip(err.Error())
	case err != nil:
		return audit.SignedTreeHead{}, false, err
	}
	sth, err = decodeObject(partition, name, body, r.v)
	switch {
	case errors.Is(err, ErrBadSignature):
		return audit.SignedTreeHead{}, false, fmt.Errorf("%w (key %s; a head of this partition that the log key did not sign: run bide-anchor check)", err, x.key)
	case err != nil:
		return skip(err.Error() + " (run bide-anchor check)")
	}
	return sth, true, nil
}

// load reads and verifies the objects at refs, skipping (and reporting) the ones that claim to be
// no head. Two versions of one key must be the same bytes (the key is derived from them) and count as
// one head.
func (r *Reader) load(ctx context.Context, refs []ref) ([]audit.SignedTreeHead, error) {
	var heads []audit.SignedTreeHead
	seen := map[string]bool{}
	for _, x := range refs {
		sth, ok, err := r.loadOne(ctx, x)
		if err != nil {
			return nil, err
		}
		if ok && !seen[x.key] {
			seen[x.key] = true
			heads = append(heads, sth)
		}
	}
	return heads, nil
}

// checkEquivocation returns an error wrapping ErrEquivocation when two heads of one tree and size
// commit to different roots.
func checkEquivocation(heads []audit.SignedTreeHead) error {
	return anchorobj.CheckEquivocation(heads, ErrEquivocation)
}

// list returns every version under a prefix (see scan).
func (r *Reader) list(ctx context.Context, prefix string) ([]ref, error) {
	var refs []ref
	err := r.scan(ctx, prefix, func(x ref) (bool, error) { refs = append(refs, x); return false, nil })
	return refs, err
}

// Latest returns the head of the largest size anchored for the tree of the given kind (audit.TreeJournal
// or audit.TreeEvents) and run or trail ID. The layout lists the largest size first, so it reads
// only the first verified key and any others of that size. It returns ErrNotFound when none is
// anchored, and an error wrapping ErrEquivocation when two heads of that size commit to different
// roots (it never picks one of them). Two heads of that size with the same root, written by two
// processes anchoring one run around a lease handoff, are fine: it returns the one whose key sorts
// first.
//
// An object that is no head (not in the layout, not canonical) is skipped and reported through
// ReaderConfig.OnSkip, so a stray object cannot make a partition unreadable; if what it skipped claimed a
// larger size than the head returned, a caller that must know uses bide-anchor check or Audit,
// which fail on it. A well-formed head whose signature does not verify is an error (ErrBadSignature),
// not skipped. Latest lists versions, so a delete marker over the newest head does not make an older
// head look like the latest, and relies on S3 listing a key as soon as it is written (strong
// read-after-write consistency for listings since December 2020).
func (r *Reader) Latest(ctx context.Context, kind, runID string) (audit.SignedTreeHead, error) {
	var best []audit.SignedTreeHead
	seen := map[string]bool{}
	err := r.scan(ctx, r.prefix+partitionOf(kind, runID)+"/", func(x ref) (bool, error) {
		if len(best) > 0 {
			// Past the largest size: stop at the first key of another size.
			if _, name, ok := splitKey(r.prefix, x.key); ok {
				if pn, ok := parseName(name); ok && pn.size != best[0].Size {
					return true, nil
				}
			}
		}
		sth, ok, err := r.loadOne(ctx, x)
		if err != nil {
			return true, err
		}
		if ok && !seen[x.key] {
			if len(best) > 0 && sth.Size != best[0].Size {
				return true, nil
			}
			seen[x.key] = true
			best = append(best, sth)
		}
		return false, nil
	})
	if err != nil {
		return audit.SignedTreeHead{}, err
	}
	if len(best) == 0 {
		return audit.SignedTreeHead{}, fmt.Errorf("%w for %s %q", ErrNotFound, kind, runID)
	}
	if err := checkEquivocation(best); err != nil {
		return audit.SignedTreeHead{}, err
	}
	return best[0], nil
}

// Heads returns every head anchored for the tree, in increasing size (heads of one size in key
// order), each verified, with the skipping of Latest. It returns an error wrapping ErrEquivocation
// when two heads of one size commit to different roots, and ErrNotFound when there is none.
func (r *Reader) Heads(ctx context.Context, kind, runID string) ([]audit.SignedTreeHead, error) {
	refs, err := r.list(ctx, r.prefix+partitionOf(kind, runID)+"/")
	if err != nil {
		return nil, err
	}
	heads, err := r.load(ctx, refs)
	if err != nil {
		return nil, err
	}
	if len(heads) == 0 {
		return nil, fmt.Errorf("%w for %s %q", ErrNotFound, kind, runID)
	}
	if err := checkEquivocation(heads); err != nil {
		return nil, err
	}
	slices.Reverse(heads) // S3 lists the largest size first; callers want them in increasing size
	return heads, nil
}

// Head returns the heads anchored for the tree at exactly the given size (more than one only when two
// processes anchored one run at the same size, with the same root), or ErrNotFound; two with different
// roots are an error wrapping ErrEquivocation.
func (r *Reader) Head(ctx context.Context, kind, runID string, size int) ([]audit.SignedTreeHead, error) {
	if size < 0 {
		return nil, fmt.Errorf("awss3: size %d is negative", size)
	}
	refs, err := r.list(ctx, r.prefix+partitionOf(kind, runID)+"/"+invertedSize(size)+"-")
	if err != nil {
		return nil, err
	}
	heads, err := r.load(ctx, refs)
	if err != nil {
		return nil, err
	}
	if len(heads) == 0 {
		return nil, fmt.Errorf("%w for %s %q at size %d", ErrNotFound, kind, runID, size)
	}
	if err := checkEquivocation(heads); err != nil {
		return nil, err
	}
	return heads, nil
}

// Finding is something Audit found wrong with the bucket's lock state. Each Finding is a reason not
// to rely on the anchor until it is explained.
type Finding struct {
	Key     string // the object, or the partition when the finding is about it
	Version string // the object's version, when the finding is about one
	Problem string
}

// String is the finding as one line.
func (f Finding) String() string {
	if f.Version != "" {
		return fmt.Sprintf("%s (version %s): %s", f.Key, f.Version, f.Problem)
	}
	return fmt.Sprintf("%s: %s", f.Key, f.Problem)
}

// claim is an object that is no verified head, and the size its key says it commits to.
type claim struct {
	key, version string
	size         int
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
// version (the key is never rewritten by this package), every object that is not a verified head (and, when its key claims a size larger than the
// largest verified head, that too), and two verified heads of one size with different roots. It
// needs read access and no write access. An empty Findings with Heads is what an intact
// anchor looks like; it is not proof the journal is intact (check the journal against the heads).
func (r *Reader) Audit(ctx context.Context, kind, runID string, now time.Time) (Report, error) {
	prefix := r.prefix + partitionOf(kind, runID) + "/"
	var rep Report
	versions := map[string]int{}
	pages := 0
	var claimed []claim // objects that are no verified head, and the size their key claims
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
			} else if _, name, ok := splitKey(r.prefix, key); ok {
				if pn, ok := parseName(name); ok {
					claimed = append(claimed, claim{key, ver, pn.size})
				}
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		if err := advance(prefix, &pages, marker, vmarker, out.NextKeyMarker, out.NextVersionIdMarker); err != nil {
			return Report{}, err
		}
		marker, vmarker = out.NextKeyMarker, out.NextVersionIdMarker
	}
	for key, n := range versions {
		if n > 1 {
			rep.Findings = append(rep.Findings, Finding{Key: key, Problem: fmt.Sprintf("%d versions of one key: a key holds one head, so another write replaced it", n)})
		}
	}
	slices.SortStableFunc(rep.Heads, func(a, b audit.SignedTreeHead) int { return a.Size - b.Size })
	if err := checkEquivocation(rep.Heads); err != nil {
		rep.Findings = append(rep.Findings, Finding{Key: prefix, Problem: err.Error()})
	}
	largest := -1
	for _, h := range rep.Heads {
		largest = max(largest, h.Size)
	}
	for _, c := range claimed {
		if c.size > largest {
			rep.Findings = append(rep.Findings, Finding{Key: c.key, Version: c.version, Problem: fmt.Sprintf("is not a verified head but its key claims size %d, larger than the largest verified head (%d): the real latest head may be hidden or forged", c.size, largest)})
		}
	}
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
// ignored. It needs no network. It returns the heads sorted by partition, then size.
//
// A directory with no heads is an error wrapping ErrNotFound, and two validly signed heads of one tree
// and size with different roots in one partition are an error wrapping ErrEquivocation. It cannot show
// the objects' lock state (the files carry none): use Reader.Audit for that. It cannot show that the
// directory is complete either: it shows only what was exported, so compare its newest head with the
// live bucket.
func LoadDir(dir string, v audit.Verifier) ([]audit.SignedTreeHead, error) {
	return layout.LoadDir(dir, v, ErrNotFound, ErrEquivocation)
}

// maxListPages bounds how many pages one listing may take: at the 1,000 versions a page holds that is
// a hundred million objects, far past any partition this package writes.
const maxListPages = 100_000

// advance checks that a truncated listing moves forward: it has a next marker that differs from the one
// just sent, and has not run past maxListPages. A server that says "truncated" and gives no way on, or
// repeats a page, would otherwise be asked again for ever. The error wraps ErrUnavailable: the state of
// the bucket is unknown, which is not a verdict about its objects.
func advance(prefix string, pages *int, markerSent, vmarkerSent, nextKey, nextVersion *string) error {
	*pages++
	if *pages > maxListPages {
		return fmt.Errorf("awss3: list %s: more than %d pages: %w", prefix, maxListPages, ErrUnavailable)
	}
	if nextKey == nil && nextVersion == nil || aws.ToString(nextKey) == aws.ToString(markerSent) && aws.ToString(nextVersion) == aws.ToString(vmarkerSent) {
		return fmt.Errorf("awss3: list %s: the listing is truncated but does not advance: %w", prefix, ErrUnavailable)
	}
	return nil
}
