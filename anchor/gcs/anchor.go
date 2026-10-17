// Package gcs is an audit.Anchor that writes every signed tree head as an object in a Google Cloud
// Storage bucket whose retention policy is locked (a WORM store), and a Reader that reads the heads
// back and verifies them.
//
// It lives in its own Go module so that the Google Cloud SDK stays out of the core module.
//
// # What it gives you, and what it does not
//
// An anchored head is an object that Cloud Storage will not delete before its retention date, even for
// a project owner, because the bucket's retention policy is locked: a locked policy can be lengthened
// but never removed or shortened, and it covers noncurrent generations too. A bank that publishes its
// heads to a bucket in a project the application's operators do not control gets tamper evidence: the
// operator can rewrite the journal and re-sign it with the log key, but cannot make the earlier heads
// disappear, so the rewrite is found by checking the journal against them.
//
// What it does not give you: the bucket's owner can still delete objects after their retention date,
// delete the project once its lien is removed (the lien is best effort and an owner can remove it),
// disable or destroy the Cloud KMS key (the heads become unreadable, not altered), and make the live
// object of a name noncurrent (versioning keeps the generation, and the Reader reads it); nothing stops
// the application from not publishing, which a monitor finds as a head older than the journal's size.
// Use a bucket in a project separate from the one that runs the application, with a retention at least
// as long as the journals it covers. A timestamp here is Google's, not an RFC 3161 token.
//
// # Use
//
//	client, _ := storage.NewClient(ctx) // Application Default Credentials
//	a, err := gcs.New(ctx, gcs.Config{
//		Store:                 gcs.NewStore(client, "bank-bide-anchor"),
//		ExpectedProjectNumber: 123456789012,
//		Retention:             7 * 365 * 24 * time.Hour,
//		KMSKey:                "projects/p/locations/eu/keyRings/r/cryptoKeys/anchor",
//	})
//	if err != nil { ... } // the bucket is not what an anchor needs: see New
//	store, err := audit.NewAuditedStore(inner, signer, a)
//
// New refuses a bucket that is owned by another project, whose retention policy is absent, not locked
// or shorter than the configured retention, that lacks versioning, uniform bucket-level access, enforced
// public access prevention, or the configured default key. Credentials come from Application Default
// Credentials and are never part of the configuration.
//
// # One object per head
//
// AuditedStore publishes a head for every journal write, and each head is an object that cannot be
// deleted until its retention date: at one write per second that is 31 million objects a year, kept
// for the whole retention. Plan the cost of storage, requests and Cloud KMS before you set a retention.
//
// # Run and trail IDs are in the bucket
//
// A signed head carries its run or trail ID in clear, because the ID is part of the signed bytes.
// Object names do not name it (they hold a hash), but the objects do, and an object cannot be erased
// before its retention date. Do not put personal data in a run ID or a trail ID that is anchored here
// (an email address, an account number, a name). Config.OnWarn reports an ID that looks like one, and
// Config.RefuseSuspectIDs makes Publish refuse it.
package gcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// MinRetention is the shortest retention New accepts. A head that can be deleted a few minutes after
// it was written is not an anchor.
const MinRetention = 24 * time.Hour

// DefaultPrefix is the object name prefix used when Config.Prefix is empty.
const DefaultPrefix = "bide-anchor/"

// Locked is the per-object retention mode that cannot be shortened or removed.
const Locked = "Locked"

// Config configures an Anchor.
type Config struct {
	// Store is the bucket. Required; NewStore adapts a *storage.Client.
	Store Store
	// ExpectedProjectNumber is the number of the Google Cloud project that must own the bucket (the
	// number, not the project ID). Required. Bucket names are global and can be created again after a
	// bucket is deleted, so the owner is checked at start and on every Reader.
	ExpectedProjectNumber uint64
	// Prefix is prepended to every object name (default "bide-anchor/"). A prefix that is set must end in "/".
	Prefix string
	// Retention is how long each head is kept. Required, at least MinRetention. The bucket's locked
	// retention policy must be at least this long; with ObjectRetention it is also set on each object.
	Retention time.Duration
	// ObjectRetention also sets a Locked per-object retention on each head, counted from the time of
	// the write. It needs a bucket created with object retention enabled; the locked bucket policy is
	// what is required either way, and this adds a per-head date that can be later than the policy's.
	ObjectRetention bool
	// LegalHold also places a temporary hold on each head. A hold has no end date and is released
	// with storage.objects.update, which the writer role does not hold: release it with a separate identity.
	LegalHold bool
	// KMSKey is the Cloud KMS key name (projects/P/locations/L/keyRings/R/cryptoKeys/K, no version)
	// each head is encrypted with. Required unless EncryptionNone is set; it must also be the bucket's
	// default key.
	KMSKey string
	// EncryptionNone allows a bucket without a customer-managed key. For tests against a server that
	// has no Cloud KMS; do not set it in production.
	EncryptionNone bool
	// OnWarn is called with a non-fatal finding: a soft delete policy on the bucket, or (with
	// RefuseSuspectIDs false) an ID that looks like personal data. Without it the finding is logged
	// with log/slog at Warn.
	OnWarn func(error)
	// RefuseSuspectIDs makes Publish refuse a run or trail ID that looks like personal data (an
	// email address, a long run of digits) instead of warning.
	RefuseSuspectIDs bool
	// Now is the clock (default time.Now). It is used only for the per-object retention date.
	Now func() time.Time
}

// Anchor writes signed tree heads to Cloud Storage under a locked retention policy. It is safe for
// concurrent use.
type Anchor struct {
	s      Store
	cfg    Config
	prefix string
	now    func() time.Time
}

var _ audit.Anchor = (*Anchor)(nil)

// New validates the bucket and returns an Anchor. It refuses (an error wrapping agent.ErrConfig) a
// configuration without a store, project number, retention or key, and a bucket that is owned by
// another project, lacks a retention policy, has one that is not locked or is shorter than the
// configured retention, lacks versioning, uniform bucket-level access, enforced public access
// prevention or the configured default key, or (with ObjectRetention) cannot hold per-object
// retention. It reads the bucket's configuration (one call) and writes nothing.
func New(ctx context.Context, cfg Config) (*Anchor, error) {
	a, err := newAnchor(cfg)
	if err != nil {
		return nil, err
	}
	if err := a.validateBucket(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func badConfig(format string, args ...any) error {
	return fmt.Errorf("gcs: "+format+": %w", append(args, agent.ErrConfig)...)
}

func newAnchor(cfg Config) (*Anchor, error) {
	switch {
	case cfg.Store == nil:
		return nil, badConfig("Config.Store is nil")
	case cfg.ExpectedProjectNumber == 0:
		return nil, badConfig("Config.ExpectedProjectNumber is zero: set the number of the project that owns the bucket")
	case cfg.Retention < MinRetention:
		return nil, badConfig("Config.Retention %s is below the minimum %s", cfg.Retention, MinRetention)
	case cfg.KMSKey == "" && !cfg.EncryptionNone:
		return nil, badConfig("Config.KMSKey is empty: set the Cloud KMS key the heads are encrypted with")
	case cfg.KMSKey != "" && cfg.EncryptionNone:
		return nil, badConfig("Config.KMSKey and Config.EncryptionNone are both set")
	case cfg.KMSKey != "" && !validKMSKey(cfg.KMSKey):
		return nil, badConfig("Config.KMSKey %q is not projects/P/locations/L/keyRings/R/cryptoKeys/K (no key version)", cfg.KMSKey)
	}
	prefix, err := checkPrefix(cfg.Prefix)
	if err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	cfg.Prefix = prefix
	return &Anchor{s: cfg.Store, cfg: cfg, prefix: prefix, now: now}, nil
}

func checkPrefix(prefix string) (string, error) {
	if prefix == "" {
		return DefaultPrefix, nil
	}
	if !strings.HasSuffix(prefix, "/") || strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "//") {
		return "", badConfig("Config.Prefix %q must end in / and not start with / or hold //", prefix)
	}
	return prefix, nil
}

// validKMSKey reports whether s names a Cloud KMS crypto key, with no version.
func validKMSKey(s string) bool {
	p := strings.Split(s, "/")
	return len(p) == 8 && p[0] == "projects" && p[2] == "locations" && p[4] == "keyRings" && p[6] == "cryptoKeys" &&
		p[1] != "" && p[3] != "" && p[5] != "" && p[7] != ""
}

func (a *Anchor) warn(err error) {
	if a.cfg.OnWarn != nil {
		a.cfg.OnWarn(err)
		return
	}
	slog.Warn(err.Error())
}

// validateBucket refuses a bucket that cannot hold an anchor.
func (a *Anchor) validateBucket(ctx context.Context) error {
	b, err := a.s.Bucket(ctx)
	if err != nil {
		return wrapErr("read the bucket's configuration", err)
	}
	refuse := func(format string, args ...any) error { return badConfig("the bucket "+format, args...) }
	if b.ProjectNumber != a.cfg.ExpectedProjectNumber {
		return refuse("is owned by project %d, not the expected %d", b.ProjectNumber, a.cfg.ExpectedProjectNumber)
	}
	switch {
	case b.RetentionPeriod == 0:
		return refuse("has no retention policy: set one and lock it")
	case !b.RetentionLocked:
		return refuse("has a retention policy that is not locked, so it can be removed or shortened: lock it (irreversible)")
	case b.RetentionPeriod < MinRetention:
		return refuse("has a retention policy of %s, below the minimum %s", b.RetentionPeriod, MinRetention)
	case b.RetentionPeriod < a.cfg.Retention:
		return refuse("has a retention policy of %s, shorter than the configured %s", b.RetentionPeriod, a.cfg.Retention)
	case !b.Versioning:
		return refuse("does not have object versioning on, so an overwrite would leave no earlier generation")
	case !b.UniformAccess:
		return refuse("does not have uniform bucket-level access on: object ACLs could grant access outside IAM")
	case !b.PublicAccessPrevented:
		return refuse("does not enforce public access prevention")
	case a.cfg.ObjectRetention && !b.ObjectRetention:
		return refuse("cannot hold per-object retention: it was not created with object retention enabled")
	case a.cfg.KMSKey != "" && b.DefaultKMSKey != a.cfg.KMSKey:
		return refuse("has default key %q, not the configured %q", b.DefaultKMSKey, a.cfg.KMSKey)
	}
	if b.SoftDeleteDuration > 0 {
		a.warn(fmt.Errorf("gcs: the bucket has a soft delete policy of %s; how it interacts with objects under a locked retention policy is not documented by Google, and this package does not rely on it", b.SoftDeleteDuration))
	}
	return nil
}

// wrapErr turns a Store error into a gcs error: an ErrUnavailable (a transport failure, a context that
// ended, a throttle, a server error) or a context error is wrapped with ErrUnavailable; anything else
// (permission denied, a missing bucket) is returned as is, wrapped for context.
func wrapErr(what string, err error) error {
	if unavailable(err) {
		if errors.Is(err, ErrUnavailable) {
			return fmt.Errorf("gcs: %s: %w", what, err)
		}
		return fmt.Errorf("gcs: %s: %w (%w)", what, ErrUnavailable, err)
	}
	return fmt.Errorf("gcs: %s: %w", what, err)
}

func unavailable(err error) bool {
	return errors.Is(err, ErrUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Publish writes sth as an object and reads it back; it returns nil only when the object is stored,
// retained as configured, encrypted with the configured key and equal to sth.
//
// Publish is idempotent: the name is derived from the head and the write is create-only, so publishing
// the same head again (a retry after an error that may have followed a successful write) finds the
// object, checks it and returns nil. A head of a smaller size than one already published is accepted:
// a run's heads are ordered by size, not by arrival. Publish refuses a head for another run than
// runID, a head without a signature, and (with Config.RefuseSuspectIDs) an ID that looks like personal data.
//
// It does not retry beyond the Google client's own retries and does not queue: a failure returns an
// error (wrapping ErrUnavailable when Cloud Storage could not be reached) and the caller publishes
// again, as audit.AuditedStore does on its next write or Reanchor.
func (a *Anchor) Publish(ctx context.Context, runID string, sth audit.SignedTreeHead) error {
	if sth.RunID != runID {
		return fmt.Errorf("gcs: tree head is for run %q, not %q", sth.RunID, runID)
	}
	if len(sth.Signature) == 0 {
		return errors.New("gcs: refusing to anchor a tree head without a signature")
	}
	if why := suspectID(runID); why != "" {
		err := fmt.Errorf("gcs: the run or trail ID %q %s; an anchored object cannot be erased before its retention date, and the ID is in it", runID, why)
		if a.cfg.RefuseSuspectIDs {
			return fmt.Errorf("%w: %w", err, agent.ErrConfig)
		}
		a.warn(err)
	}
	body, err := encodeObject(sth)
	if err != nil {
		return err
	}
	name := keyOf(a.prefix, sth.Kind, sth.RunID, sth.Size, body)
	until := a.now().Add(a.cfg.Retention).UTC().Truncate(time.Second).Add(time.Second)

	o := CreateOptions{KMSKey: a.cfg.KMSKey, TemporaryHold: a.cfg.LegalHold}
	if a.cfg.ObjectRetention {
		o.Retention = &ObjectRetention{Mode: Locked, RetainUntil: until}
	}
	info, err := a.s.Create(ctx, name, body, o)
	switch {
	case err == nil:
		return a.readBack(ctx, name, info.Generation, body, until, false)
	case errors.Is(err, ErrExists):
		// The head is already there: this is a retry, or another process published the same head.
		return a.readBack(ctx, name, 0, body, until, true)
	default:
		return wrapErr("write "+name, err)
	}
}

// readBack reads the generation (or the live object when generation is 0) and checks it is the bytes
// written, retained and encrypted as configured.
func (a *Anchor) readBack(ctx context.Context, name string, generation int64, want []byte, until time.Time, existing bool) error {
	got, info, err := a.s.Read(ctx, name, generation, int64(len(want)))
	if err != nil {
		return wrapErr("read back "+name, err)
	}
	if !bytes.Equal(got, want) {
		if existing {
			return fmt.Errorf("gcs: %s holds different bytes than the head being published: %w", name, ErrConflict)
		}
		return fmt.Errorf("gcs: %s read back different bytes than were written: %w", name, ErrConflict)
	}
	return a.checkStored(info, want, until, existing)
}

// checkStored compares what Cloud Storage reports for a stored object with the configuration. The
// retention it requires is counted from the object's own creation time (Cloud Storage reports it), less
// a minute of slack: a head some other writer stored under a weaker retention does not count as anchored.
func (a *Anchor) checkStored(info ObjectInfo, want []byte, until time.Time, existing bool) error {
	weak := func(format string, args ...any) error {
		return fmt.Errorf("gcs: %s: "+format+": %w", append(append([]any{info.Name}, args...), ErrWeak)...)
	}
	if info.Generation == 0 {
		return weak("the object has no generation")
	}
	if info.Size != int64(len(want)) || info.CRC32C != crc32.Checksum(want, crc32.MakeTable(crc32.Castagnoli)) {
		return weak("Cloud Storage reports a size or CRC32C that is not the bytes of the head")
	}
	if info.Created.IsZero() {
		return weak("Cloud Storage reported no creation time, so retention cannot be checked")
	}
	floor := info.Created.Add(a.cfg.Retention).Add(-time.Minute)
	if info.RetentionExpiry.IsZero() {
		return weak("no retention policy covers the object")
	}
	if info.RetentionExpiry.Before(floor) {
		return weak("retained until %s, less than the configured %s after its creation (%s)", info.RetentionExpiry.UTC().Format(time.RFC3339), a.cfg.Retention, info.Created.UTC().Format(time.RFC3339))
	}
	if a.cfg.ObjectRetention {
		r := info.Retention
		switch {
		case r == nil:
			return weak("no per-object retention")
		case r.Mode != Locked:
			return weak("per-object retention mode is %q, want %q", r.Mode, Locked)
		case existing && r.RetainUntil.Before(floor):
			return weak("per-object retention ends %s, less than the configured %s after its creation", r.RetainUntil.UTC().Format(time.RFC3339), a.cfg.Retention)
		case !existing && r.RetainUntil.Before(until.Add(-time.Minute)):
			return weak("per-object retain-until is %s, want %s", r.RetainUntil.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
		}
	}
	if a.cfg.LegalHold && !info.TemporaryHold {
		return weak("no temporary hold, want one")
	}
	if a.cfg.KMSKey != "" && !kmsMatches(a.cfg.KMSKey, info.KMSKey) {
		return weak("encrypted with %q, want a version of %q", info.KMSKey, a.cfg.KMSKey)
	}
	return nil
}

// kmsMatches reports whether the key version Cloud Storage reports belongs to the configured key.
func kmsMatches(configured, reported string) bool {
	return strings.HasPrefix(reported, configured+"/cryptoKeyVersions/")
}
