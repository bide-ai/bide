package gcs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/anchorobj"
)

// maxObjectBytes bounds what the Reader reads for one object: a head is a few hundred bytes plus the
// run ID, and no object this package writes is near this size.
const maxObjectBytes = 1 << 20

// ReaderConfig configures a Reader.
type ReaderConfig struct {
	// Store is the bucket, with read-only credentials. Required.
	Store Store
	// ExpectedProjectNumber is the number of the project that must own the bucket. Required.
	ExpectedProjectNumber uint64
	// Prefix is the prefix the Anchor was configured with (default "bide-anchor/").
	Prefix string
	// KMSKey, when set, is the Cloud KMS key the heads must be encrypted with; Audit reports an
	// object encrypted with another key (or a Google-managed one) as a finding.
	KMSKey string
	// Retention, when set, is the retention the Anchor was configured with: Audit reports a bucket whose
	// retention policy is shorter. Without it Audit judges the policy against MinRetention only.
	Retention time.Duration
	// OnSkip is called with each object the Reader skips because it claims to be no head (not in the
	// layout, not the canonical encoding of a head). Without it the finding is logged with log/slog at
	// Warn. An object that claims to be a head and fails its signature is an error, not a skip.
	OnSkip func(Finding)
	// Verifier verifies every head's signature: the log's public key. Required; a Reader that does
	// not check signatures would trust anyone able to write the bucket.
	Verifier audit.Verifier
}

// Reader reads anchored heads back from Cloud Storage and verifies them. Every head it returns has a
// valid signature under the Verifier, a body that is the canonical encoding of its head, and a name
// that names that head. It reads every generation of every name, noncurrent ones included, so an
// overwrite or a delete of the live object hides nothing from it.
type Reader struct {
	s      Store
	prefix string
	kms    string
	retain time.Duration
	v      audit.Verifier
	onSkip func(Finding)
}

// NewReader returns a Reader. It refuses (an error wrapping agent.ErrConfig) an incomplete
// configuration and a bucket owned by another project than ExpectedProjectNumber.
func NewReader(ctx context.Context, cfg ReaderConfig) (*Reader, error) {
	if cfg.Store == nil {
		return nil, badConfig("ReaderConfig.Store is nil")
	}
	if cfg.ExpectedProjectNumber == 0 {
		return nil, badConfig("ReaderConfig.ExpectedProjectNumber is zero")
	}
	prefix, err := checkPrefix(cfg.Prefix)
	if err != nil {
		return nil, err
	}
	if cfg.Verifier == nil {
		return nil, badConfig("ReaderConfig.Verifier is nil")
	}
	if cfg.KMSKey != "" && !validKMSKey(cfg.KMSKey) {
		return nil, badConfig("ReaderConfig.KMSKey %q is not projects/P/locations/L/keyRings/R/cryptoKeys/K", cfg.KMSKey)
	}
	b, err := cfg.Store.Bucket(ctx)
	if err != nil {
		return nil, wrapErr("read the bucket's configuration", err)
	}
	if b.ProjectNumber != cfg.ExpectedProjectNumber {
		return nil, fmt.Errorf("gcs: %w: the bucket is owned by project %d, not the expected %d: %w", ErrWrongProject, b.ProjectNumber, cfg.ExpectedProjectNumber, agent.ErrConfig)
	}
	onSkip := cfg.OnSkip
	if onSkip == nil {
		onSkip = func(f Finding) { slog.Warn("gcs: skipped an object that is no anchored head: " + f.String()) }
	}
	return &Reader{s: cfg.Store, prefix: prefix, kms: cfg.KMSKey, retain: cfg.Retention, v: cfg.Verifier, onSkip: onSkip}, nil
}

// scan calls fn for each generation under a prefix, in the order the layout sorts them (the largest
// size first, each name's oldest generation first), until fn returns true. It lists noncurrent
// generations and not only live objects: a delete or an overwrite of the live object makes it
// noncurrent, and the retained generation is still the anchored head.
func (r *Reader) scan(ctx context.Context, prefix string, fn func(ObjectInfo) (stop bool, err error)) error {
	var ferr error
	err := r.s.List(ctx, prefix, true, func(o ObjectInfo) bool {
		stop, err := fn(o)
		ferr = err
		return !stop && err == nil
	})
	if err != nil {
		return wrapErr("list "+prefix, err)
	}
	return ferr
}

func (r *Reader) get(ctx context.Context, x ObjectInfo) ([]byte, error) {
	b, _, err := r.s.Read(ctx, x.Name, x.Generation, maxObjectBytes)
	if err != nil {
		return nil, wrapErr("read "+x.Name, err)
	}
	if len(b) > maxObjectBytes {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrObject, x.Name, maxObjectBytes)
	}
	return b, nil
}

// loadOne reads and verifies one generation. An object that is not in the layout, or does not decode
// to its canonical encoding, claims to be no head: it is reported through OnSkip and skipped (ok
// false), so that one stray object cannot make the partition unreadable. A well-formed head whose
// signature does not verify is an error (ErrBadSignature), never skipped: it may be a real head signed
// by another key, and skipping it would show an older head as the latest.
func (r *Reader) loadOne(ctx context.Context, x ObjectInfo) (sth audit.SignedTreeHead, ok bool, err error) {
	skip := func(problem string) (audit.SignedTreeHead, bool, error) {
		r.onSkip(Finding{Key: x.Name, Version: gen(x), Problem: problem})
		return audit.SignedTreeHead{}, false, nil
	}
	partition, name, laid := splitKey(r.prefix, x.Name)
	if !laid {
		return skip("the name is not in this package's layout")
	}
	body, err := r.get(ctx, x)
	switch {
	case errors.Is(err, ErrObject):
		return skip(err.Error())
	case errors.Is(err, ErrNoObject):
		// Listed and then gone (deleted at the end of its retention, or hidden): fail closed, naming it.
		// Skipping it would show an older head as the latest.
		return audit.SignedTreeHead{}, false, fmt.Errorf("%w: %s generation %s (deleted at the end of its retention, or hidden): run Reader.Audit to see the rest: %w", ErrVanished, x.Name, gen(x), err)
	case err != nil:
		return audit.SignedTreeHead{}, false, err
	}
	sth, err = decodeObject(partition, name, body, r.v)
	switch {
	case errors.Is(err, ErrBadSignature):
		return audit.SignedTreeHead{}, false, fmt.Errorf("%w (object %s; a head of this partition that the log key did not sign: run bide-anchor check)", err, x.Name)
	case err != nil:
		return skip(err.Error() + " (run bide-anchor check)")
	}
	return sth, true, nil
}

func gen(x ObjectInfo) string { return strconv.FormatInt(x.Generation, 10) }

// load reads and verifies the generations at refs, skipping (and reporting) the ones that claim to be
// no head. Two generations of one name must be the same bytes (the name is derived from them), so one
// that is not fails the digest check and is skipped and reported; the rest count as one head.
func (r *Reader) load(ctx context.Context, refs []ObjectInfo) ([]audit.SignedTreeHead, error) {
	var heads []audit.SignedTreeHead
	seen := map[string]bool{}
	for _, x := range refs {
		sth, ok, err := r.loadOne(ctx, x)
		if err != nil {
			return nil, err
		}
		if ok && !seen[x.Name] {
			seen[x.Name] = true
			heads = append(heads, sth)
		}
	}
	return heads, nil
}

// list returns every generation under a prefix (see scan).
func (r *Reader) list(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var refs []ObjectInfo
	err := r.scan(ctx, prefix, func(x ObjectInfo) (bool, error) { refs = append(refs, x); return false, nil })
	return refs, err
}

// Latest returns the head of the largest size anchored for the tree of the given kind (audit.TreeJournal
// or audit.TreeEvents) and run or trail ID. The layout lists the largest size first, so it reads
// only the first verified object and any others of that size. It returns ErrNotFound when none is
// anchored, and an error wrapping ErrEquivocation when two heads of that size commit to different
// roots (it never picks one of them). Two heads of that size with the same root, written by two
// processes anchoring one run around a lease handoff, are fine: it returns the one whose name sorts
// first.
//
// An object that is no head (not in the layout, not canonical) is skipped and reported through
// ReaderConfig.OnSkip, so a stray object cannot make a partition unreadable; if what it skipped claimed a
// larger size than the head returned, a caller that must know uses bide-anchor check or Audit,
// which fail on it. A well-formed head whose signature does not verify is an error (ErrBadSignature),
// not skipped. A generation that was listed and cannot be read (deleted at the end of its retention, or
// hidden) is an error wrapping ErrVanished, never a skip: under a locked policy it is a signal, and an
// older head must not be shown as the latest. Audit reports it as a finding. Latest lists generations, so an overwrite or a delete of the newest head's live object
// does not make an older head look like the latest.
func (r *Reader) Latest(ctx context.Context, kind, runID string) (audit.SignedTreeHead, error) {
	var best []audit.SignedTreeHead
	seen := map[string]bool{}
	err := r.scan(ctx, r.prefix+partitionOf(kind, runID)+"/", func(x ObjectInfo) (bool, error) {
		if len(best) > 0 {
			// Past the largest size: stop at the first name of another size.
			if _, name, ok := splitKey(r.prefix, x.Name); ok {
				if pn, ok := parseName(name); ok && pn.size != best[0].Size {
					return true, nil
				}
			}
		}
		sth, ok, err := r.loadOne(ctx, x)
		if err != nil {
			return true, err
		}
		if ok && !seen[x.Name] {
			if len(best) > 0 && sth.Size != best[0].Size {
				return true, nil
			}
			seen[x.Name] = true
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

// Heads returns every head anchored for the tree, in increasing size (heads of one size in name
// order), each verified, with the skipping of Latest and its ErrVanished. It returns an error wrapping
// ErrEquivocation when two heads of one size commit to different roots, and ErrNotFound when there is none.
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
	// The layout lists the largest size first and, within a size, names in order: increasing size, with
	// the heads of one size staying in name order.
	slices.SortStableFunc(heads, func(a, b audit.SignedTreeHead) int { return a.Size - b.Size })
	return heads, nil
}

// Head returns the heads anchored for the tree at exactly the given size (more than one only when two
// processes anchored one run at the same size, with the same root), or ErrNotFound; two with different
// roots are an error wrapping ErrEquivocation.
func (r *Reader) Head(ctx context.Context, kind, runID string, size int) ([]audit.SignedTreeHead, error) {
	if size < 0 {
		return nil, fmt.Errorf("gcs: size %d is negative", size)
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

// checkEquivocation returns an error wrapping ErrEquivocation when two heads of one tree and size
// commit to different roots.
func checkEquivocation(heads []audit.SignedTreeHead) error {
	return anchorobj.CheckEquivocation(heads, ErrEquivocation)
}

// Finding is something Audit found wrong with the bucket's lock state. Each Finding is a reason not
// to rely on the anchor until it is explained.
type Finding struct {
	Key     string // the object name, or the partition when the finding is about it
	Version string // the generation, when the finding is about one
	Problem string
}

// String is the finding as one line.
func (f Finding) String() string {
	if f.Version != "" {
		return fmt.Sprintf("%s (generation %s): %s", f.Key, f.Version, f.Problem)
	}
	return fmt.Sprintf("%s: %s", f.Key, f.Problem)
}

// claim is an object that is no verified head, and the size its name says it commits to.
type claim struct {
	name, generation string
	size             int
}

// Report is the result of Audit.
type Report struct {
	Heads    []audit.SignedTreeHead // every head of the tree, in increasing size, each verified
	Findings []Finding              // what is wrong with how they are stored, if anything
}

// Audit reads every generation of every object of a tree and reports its retention state: Heads are
// the verified heads, and Findings lists every noncurrent generation (something overwrote or deleted
// the live object), every generation no retention covers (it can be deleted), every generation whose
// retention has ended (it can now be deleted), every per-object retention that is not Locked, every
// object not encrypted with the configured key (when ReaderConfig.KMSKey is set), every name with more
// than one generation (the name is never rewritten by this package), every object that is not a
// verified head (and, when its name claims a size larger than the largest verified head, that too), and
// two verified heads of one size with different roots, and each bucket-level protection that is gone
// (the retention policy absent, unlocked or under the minimum; versioning, uniform bucket-level access
// or public access prevention off; with ReaderConfig.KMSKey, another default key). It needs read access and no write access. An
// error (a listing or read that is unavailable or denied) means the state is unknown: the audit did not
// pass, retry it. An
// empty Findings with Heads is what an intact anchor looks like; it is not proof the journal is intact
// (check the journal against the heads).
func (r *Reader) Audit(ctx context.Context, kind, runID string, now time.Time) (Report, error) {
	prefix := r.prefix + partitionOf(kind, runID) + "/"
	b, err := r.s.Bucket(ctx)
	if err != nil {
		return Report{}, wrapErr("read the bucket's configuration", err)
	}
	infos, err := r.list(ctx, prefix)
	if err != nil {
		return Report{}, err
	}
	var rep Report
	rep.Findings = append(rep.Findings, r.bucketFindings(b)...)
	versions := map[string]int{}
	var claimed []claim // objects that are no verified head, and the size their name claims
	for _, x := range infos {
		versions[x.Name]++
		f, sth, err := r.auditGeneration(ctx, x, now)
		rep.Findings = append(rep.Findings, f...)
		if err != nil {
			return Report{}, err
		}
		if sth != nil {
			rep.Heads = append(rep.Heads, *sth)
		} else if _, name, ok := splitKey(r.prefix, x.Name); ok {
			if pn, ok := parseName(name); ok {
				claimed = append(claimed, claim{x.Name, gen(x), pn.size})
			}
		}
	}
	for name, n := range versions {
		if n > 1 {
			rep.Findings = append(rep.Findings, Finding{Key: name, Problem: fmt.Sprintf("%d generations of one name: a name holds one head, so another write replaced it", n)})
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
			rep.Findings = append(rep.Findings, Finding{Key: c.name, Version: c.generation, Problem: fmt.Sprintf("is not a verified head but its name claims size %d, larger than the largest verified head (%d): the real latest head may be hidden or forged", c.size, largest)})
		}
	}
	slices.SortFunc(rep.Findings, func(a, b Finding) int { return strings.Compare(a.String(), b.String()) })
	return rep, nil
}

// bucketFindings reports the bucket-level protections that New would have refused the bucket for. The
// lock lives on the bucket and not on the objects: a policy that is not locked can be removed or
// shortened by the owner, and the retention dates of the objects go with it.
func (r *Reader) bucketFindings(b BucketInfo) []Finding {
	var fs []Finding
	add := func(p string) { fs = append(fs, Finding{Key: "(bucket)", Problem: "the bucket " + p}) }
	switch {
	case b.RetentionPeriod == 0:
		add("has no retention policy: nothing keeps a head from being deleted")
	case !b.RetentionLocked:
		add("has a retention policy that is not locked: its owner can remove or shorten it, and the retention of every head goes with it")
	case b.RetentionPeriod < MinRetention:
		add(fmt.Sprintf("has a retention policy of %s, below the minimum %s", b.RetentionPeriod, MinRetention))
	case r.retain > 0 && b.RetentionPeriod < r.retain:
		add(fmt.Sprintf("has a retention policy of %s, shorter than the configured %s", b.RetentionPeriod, r.retain))
	}
	if !b.Versioning {
		add("does not have object versioning on: an overwrite leaves no earlier generation")
	}
	if !b.UniformAccess {
		add("does not have uniform bucket-level access on: object ACLs could grant access outside IAM")
	}
	if !b.PublicAccessPrevented {
		add("does not enforce public access prevention")
	}
	if r.kms != "" && b.DefaultKMSKey != r.kms {
		add(fmt.Sprintf("has default key %q, not the configured %q", b.DefaultKMSKey, r.kms))
	}
	return fs
}

func (r *Reader) auditGeneration(ctx context.Context, x ObjectInfo, now time.Time) ([]Finding, *audit.SignedTreeHead, error) {
	body, err := r.get(ctx, x)
	var fs []Finding
	add := func(p string) { fs = append(fs, Finding{Key: x.Name, Version: gen(x), Problem: p}) }
	if errors.Is(err, ErrNoObject) {
		add("the generation vanished between the listing and the read (deleted, retention ended?)")
		return fs, nil, nil
	}
	if err != nil && !errors.Is(err, ErrObject) {
		return nil, nil, err
	}
	if !x.Noncurrent.IsZero() {
		add("a noncurrent generation: the live object was overwritten or deleted since " + x.Noncurrent.UTC().Format(time.RFC3339) + " (this generation is intact and still read)")
	}
	covered := false
	if !x.RetentionExpiry.IsZero() {
		covered = true
		if !x.RetentionExpiry.After(now) {
			add("its retention has ended: it can be deleted")
		}
	}
	if x.Retention != nil {
		covered = true
		if x.Retention.Mode != Locked {
			add("per-object retention is " + x.Retention.Mode + ": an authorised user can shorten or remove it")
		}
		if !x.Retention.RetainUntil.After(now) && x.RetentionExpiry.IsZero() {
			add("its retention has ended: it can be deleted")
		}
	}
	if !covered {
		add("no retention covers it: it can be deleted")
	}
	if r.kms != "" && !kmsMatches(r.kms, x.KMSKey) {
		add("encrypted with " + strconv.Quote(x.KMSKey) + ", not a version of the configured key")
	}
	partition, name, ok := splitKey(r.prefix, x.Name)
	if !ok {
		add("the name is not in this package's layout")
		return fs, nil, nil
	}
	if err != nil {
		add(err.Error())
		return fs, nil, nil
	}
	h, err := decodeObject(partition, name, body, r.v)
	if err != nil {
		add(err.Error())
		return fs, nil, nil
	}
	return fs, &h, nil
}

// LoadDir reads a downloaded set of anchored objects (for example the output of "gcloud storage cp -r" of
// the anchor prefix) and verifies each against v. The set is every regular file under dir whose path
// ends in <partition>/<object name>; any other file is an error, so a stray or renamed file is not
// ignored. It needs no network. It returns the heads sorted by partition, then size. A directory with no
// heads is an error wrapping ErrNotFound, and two validly signed heads of one tree and size with
// different roots are an error wrapping ErrEquivocation (audit/anchorobj's Layout.LoadDir, shared with
// the other anchors). It cannot show the objects' lock state (the files carry none): use Reader.Audit for
// that, and it cannot show that the directory is complete: it shows only what was exported, so compare
// its newest head with the live bucket.
func LoadDir(dir string, v audit.Verifier) ([]audit.SignedTreeHead, error) {
	return layout.LoadDir(dir, v, ErrNotFound, ErrEquivocation)
}
