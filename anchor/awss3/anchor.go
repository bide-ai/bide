// Package awss3 is an audit.Anchor that writes every signed tree head as an object in an S3 bucket
// under Object Lock (a WORM store), and a Reader that reads the heads back and verifies them.
//
// It lives in its own Go module so that the AWS SDK stays out of the core module.
//
// # What it gives you, and what it does not
//
// An anchored head is an object that S3 will not overwrite or delete until its retention date, not
// even for the account's root user, when the bucket uses compliance mode. A bank that publishes its
// heads to a bucket in an AWS account the application's operators do not control gets tamper
// evidence: the operator can rewrite the journal and re-sign it with the log key, but cannot make the
// earlier heads disappear, so the rewrite is found by checking the journal against them.
//
// What it does not give you: the account's owner can still delete the bucket's objects after their
// retention date, close the account, or disable the KMS key (the heads become unreadable, not
// altered); governance mode can be bypassed by any principal holding s3:BypassGovernanceRetention;
// nothing stops the application from not publishing, which a monitor finds as a head older than the
// journal's size. Use compliance mode, in an AWS account separate from the one that runs the
// application, with a retention at least as long as the journals it covers. A timestamp here is
// AWS's, not an RFC 3161 token.
//
// # Use
//
//	cfg, _ := config.LoadDefaultConfig(ctx)
//	a, err := awss3.New(ctx, awss3.Config{
//		Client:              s3.NewFromConfig(cfg),
//		Bucket:              "bank-bide-anchor",
//		ExpectedBucketOwner: "111122223333",
//		Retention:           7 * 365 * 24 * time.Hour,
//		KMSKeyID:            "arn:aws:kms:eu-west-1:111122223333:key/...",
//	})
//	if err != nil { ... } // the bucket is not what an anchor needs: see New
//	store, err := audit.NewAuditedStore(inner, signer, a)
//
// New refuses a bucket that lacks Object Lock, versioning or a default retention, so a misconfigured
// bucket fails at start, not at the first head. Credentials come from the AWS SDK's default chain
// and are never part of the configuration.
//
// # One object per head
//
// AuditedStore publishes a head for every journal write, and each head is an object that cannot be
// deleted until its retention date: at one write per second that is 31 million objects a year, kept
// for the whole retention. Plan the cost of storage, requests and KMS before you set a retention, and
// see the retention guide for how to bound it.
//
// # Run and trail IDs are in the bucket
//
// A signed head carries its run or trail ID in clear, because the ID is part of the signed bytes.
// Object keys do not name it (they hold a hash), but the objects do, and an object cannot be erased
// before its retention date. Do not put personal data in a run ID or a trail ID that is anchored here
// (an email address, an account number, a name). Config.OnWarn reports an ID that looks like one, and
// Config.RefuseSuspectIDs makes Publish refuse it.
package awss3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// MinRetention is the shortest retention New accepts. A head that can be deleted a few minutes after
// it was written is not an anchor.
const MinRetention = 24 * time.Hour

// Client is the part of the S3 API this package calls. *s3.Client satisfies it. It has no method that
// deletes an object or changes a retention, so the package cannot call one.
type Client interface {
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	GetBucketVersioning(ctx context.Context, in *s3.GetBucketVersioningInput, opts ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error)
	GetObjectLockConfiguration(ctx context.Context, in *s3.GetObjectLockConfigurationInput, opts ...func(*s3.Options)) (*s3.GetObjectLockConfigurationOutput, error)
	GetBucketEncryption(ctx context.Context, in *s3.GetBucketEncryptionInput, opts ...func(*s3.Options)) (*s3.GetBucketEncryptionOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput, opts ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error)
}

// Config configures an Anchor.
type Config struct {
	// Client is the S3 client. Required.
	Client Client
	// Bucket is the bucket the heads are written to. Required; it must have Object Lock enabled
	// (which requires versioning) and a default retention.
	Bucket string
	// ExpectedBucketOwner is the 12-digit ID of the AWS account that must own the bucket. Required.
	// It is sent with every request, so a bucket of that name in another account is refused, and the
	// read-back proves the object landed in the account you meant.
	ExpectedBucketOwner string
	// Prefix is prepended to every key (default "bide-anchor/"). A prefix that is set must end in "/".
	Prefix string
	// Mode is the object lock mode of each head: types.ObjectLockModeCompliance (the default) or
	// types.ObjectLockModeGovernance, which AllowGovernance must also allow.
	Mode types.ObjectLockMode
	// AllowGovernance permits Mode governance. In governance mode any principal with
	// s3:BypassGovernanceRetention can delete a head before its retention date, so the anchor does not
	// hold against an operator who can grant themselves that permission. New reports it through
	// OnWarn every time.
	AllowGovernance bool
	// Retention is how long each head is locked. Required, at least MinRetention. It is set on each
	// object (not only as the bucket's default) and counted from the time of the write.
	Retention time.Duration
	// LegalHold also places a legal hold on each head. A hold has no end date and is released with
	// s3:PutObjectLegalHold, which the writer then holds too: release it with a separate identity.
	LegalHold bool
	// KMSKeyID is the KMS key (an ID, ARN or alias ARN) each head is encrypted with (SSE-KMS).
	// Required unless EncryptionNone is set.
	KMSKeyID string
	// EncryptionNone allows a store without SSE-KMS. For tests against a server that has no KMS;
	// do not set it in production.
	EncryptionNone bool
	// OnWarn is called with a non-fatal finding: governance mode, a bucket without default KMS
	// encryption, or (with RefuseSuspectIDs false) an ID that looks like personal data. Without it
	// the finding is logged with log/slog at Warn.
	OnWarn func(error)
	// RefuseSuspectIDs makes Publish refuse a run or trail ID that looks like personal data (an
	// email address, a long run of digits) instead of warning.
	RefuseSuspectIDs bool
	// Now is the clock (default time.Now). The retention date sent to S3 is Now() plus Retention.
	Now func() time.Time
}

// Anchor writes signed tree heads to S3 under Object Lock. It is safe for concurrent use.
type Anchor struct {
	c      Client
	cfg    Config
	prefix string
	now    func() time.Time
}

var _ audit.Anchor = (*Anchor)(nil)

// New validates the bucket and returns an Anchor. It refuses (an error wrapping agent.ErrConfig)
// a configuration without a client, bucket, owner or retention, and a bucket that is not owned by the
// expected account, lacks versioning, lacks Object Lock, lacks a default retention, or whose default
// retention is weaker than Mode and Retention. It makes these requests: HeadBucket,
// GetBucketVersioning, GetObjectLockConfiguration and GetBucketEncryption. It writes nothing.
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

func newAnchor(cfg Config) (*Anchor, error) {
	bad := func(format string, args ...any) (*Anchor, error) {
		return nil, fmt.Errorf("awss3: "+format+": %w", append(args, agent.ErrConfig)...)
	}
	if err := layout.Validate(); err != nil {
		return bad("the object layout is invalid: %v", err)
	}
	switch {
	case cfg.Client == nil:
		return bad("Config.Client is nil")
	case cfg.Bucket == "":
		return bad("Config.Bucket is empty")
	case !validOwner(cfg.ExpectedBucketOwner):
		return bad("Config.ExpectedBucketOwner must be the 12-digit AWS account ID that owns the bucket")
	case cfg.Retention < MinRetention:
		return bad("Config.Retention %s is below the minimum %s", cfg.Retention, MinRetention)
	case cfg.KMSKeyID == "" && !cfg.EncryptionNone:
		return bad("Config.KMSKeyID is empty: set the KMS key the heads are encrypted with")
	case cfg.KMSKeyID != "" && cfg.EncryptionNone:
		return bad("Config.KMSKeyID and Config.EncryptionNone are both set")
	}
	switch cfg.Mode {
	case "":
		cfg.Mode = types.ObjectLockModeCompliance
	case types.ObjectLockModeCompliance:
	case types.ObjectLockModeGovernance:
		if !cfg.AllowGovernance {
			return bad("Config.Mode is governance, which any principal with s3:BypassGovernanceRetention can bypass; set AllowGovernance to accept that")
		}
	default:
		return bad("Config.Mode %q is not COMPLIANCE or GOVERNANCE", cfg.Mode)
	}
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = "bide-anchor/"
	}
	if !strings.HasSuffix(prefix, "/") || strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "//") {
		return bad("Config.Prefix %q must end in / and not start with / or hold //", prefix)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	cfg.Prefix = prefix
	a := &Anchor{c: cfg.Client, cfg: cfg, prefix: prefix, now: now}
	if cfg.Mode == types.ObjectLockModeGovernance {
		a.warn(fmt.Errorf("awss3: bucket %s is anchored in governance mode: a principal with s3:BypassGovernanceRetention can delete a head before its retention date; use compliance mode", cfg.Bucket))
	}
	return a, nil
}

func validOwner(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
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
	bucket, owner := aws.String(a.cfg.Bucket), aws.String(a.cfg.ExpectedBucketOwner)
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("awss3: bucket %s: "+format+": %w", append(append([]any{a.cfg.Bucket}, args...), agent.ErrConfig)...)
	}
	if _, err := a.c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: bucket, ExpectedBucketOwner: owner}); err != nil {
		return a.wrap("check bucket", err)
	}
	v, err := a.c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: bucket, ExpectedBucketOwner: owner})
	if err != nil {
		return a.wrap("read versioning", err)
	}
	if v.Status != types.BucketVersioningStatusEnabled {
		return refuse("versioning is %q, not Enabled", v.Status)
	}
	lc, err := a.c.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: bucket, ExpectedBucketOwner: owner})
	if err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) && ae.ErrorCode() == "ObjectLockConfigurationNotFoundError" {
			return refuse("Object Lock is not enabled (it can only be enabled when a bucket is created, or later by AWS support)")
		}
		return a.wrap("read Object Lock configuration", err)
	}
	cfg := lc.ObjectLockConfiguration
	if cfg == nil || cfg.ObjectLockEnabled != types.ObjectLockEnabledEnabled {
		return refuse("Object Lock is not enabled")
	}
	if cfg.Rule == nil || cfg.Rule.DefaultRetention == nil {
		return refuse("it has no default retention: set one, so that a head written without per-object retention is still locked")
	}
	dr := cfg.Rule.DefaultRetention
	period := time.Duration(aws.ToInt32(dr.Days)) * 24 * time.Hour
	if y := aws.ToInt32(dr.Years); y > 0 {
		period = time.Duration(y) * 365 * 24 * time.Hour
	}
	if period < MinRetention {
		return refuse("its default retention is %s, below the minimum %s", period, MinRetention)
	}
	if a.cfg.Mode == types.ObjectLockModeCompliance && dr.Mode != types.ObjectLockRetentionModeCompliance {
		return refuse("its default retention mode is %s, weaker than the configured COMPLIANCE", dr.Mode)
	}
	if period < a.cfg.Retention {
		return refuse("its default retention is %s, shorter than the configured %s", period, a.cfg.Retention)
	}
	if a.cfg.KMSKeyID != "" {
		enc, err := a.c.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: bucket, ExpectedBucketOwner: owner})
		switch {
		case err != nil:
			a.warn(fmt.Errorf("awss3: bucket %s: could not read its default encryption (%w); each head is still written with SSE-KMS", a.cfg.Bucket, err))
		case !defaultKMS(enc):
			a.warn(fmt.Errorf("awss3: bucket %s: its default encryption is not SSE-KMS; each head is still written with SSE-KMS", a.cfg.Bucket))
		}
	}
	return nil
}

func defaultKMS(enc *s3.GetBucketEncryptionOutput) bool {
	if enc == nil || enc.ServerSideEncryptionConfiguration == nil {
		return false
	}
	for _, r := range enc.ServerSideEncryptionConfiguration.Rules {
		if d := r.ApplyServerSideEncryptionByDefault; d != nil && d.SSEAlgorithm == types.ServerSideEncryptionAwsKms {
			return true
		}
	}
	return false
}

// wrap turns an S3 error into an awss3 error: a transport failure, a context that ended, a throttle
// or a server error wraps ErrUnavailable; anything else (access denied, a missing bucket) is
// returned as is, wrapped for context.
func (a *Anchor) wrap(what string, err error) error { return wrapErr(what, err) }

func wrapErr(what string, err error) error {
	if unavailable(err) {
		return fmt.Errorf("awss3: %s: %w (%w)", what, ErrUnavailable, err)
	}
	return fmt.Errorf("awss3: %s: %w", what, err)
}

func unavailable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "InternalError", "ServiceUnavailable", "SlowDown", "RequestTimeout", "RequestTimeTooSkewed", "OperationAborted", "ConditionalRequestConflict":
			return true
		}
		var re interface{ HTTPStatusCode() int }
		if errors.As(err, &re) && re.HTTPStatusCode() >= http.StatusInternalServerError {
			return true
		}
		return false
	}
	return true // no API answer: the request did not complete
}

func isPreconditionFailed(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.ErrorCode() == "PreconditionFailed"
}

// Publish writes sth as an object under Object Lock and reads it back; it returns nil only when the
// object is stored, locked as configured and equal to sth.
//
// Publish is idempotent: the key is derived from the head, so publishing the same head again (a retry
// after an error that may have followed a successful write) finds the object, checks it and returns
// nil. A head of a smaller size than one already published is accepted: a run's heads are ordered by
// size, not by arrival. Publish refuses a head for another run than runID, a head without a
// signature, and (with Config.RefuseSuspectIDs) an ID that looks like personal data.
//
// It does not retry beyond the AWS client's own retries and does not queue: a failure returns an
// error (wrapping ErrUnavailable when S3 could not be reached) and the caller publishes again, as
// audit.AuditedStore does on its next write or Reanchor.
func (a *Anchor) Publish(ctx context.Context, runID string, sth audit.SignedTreeHead) error {
	if sth.RunID != runID {
		return fmt.Errorf("awss3: tree head is for run %q, not %q", sth.RunID, runID)
	}
	if len(sth.Signature) == 0 {
		return errors.New("awss3: refusing to anchor a tree head without a signature")
	}
	if why := suspectID(runID); why != "" {
		err := fmt.Errorf("awss3: the run or trail ID %q %s; an anchored object cannot be erased before its retention date, and the ID is in it", runID, why)
		if a.cfg.RefuseSuspectIDs {
			return fmt.Errorf("%w: %w", err, agent.ErrConfig)
		}
		a.warn(err)
	}
	body, err := encodeObject(sth)
	if err != nil {
		return err
	}
	key := keyOf(a.prefix, sth.Kind, sth.RunID, sth.Size, body)
	until := a.now().Add(a.cfg.Retention).UTC().Truncate(time.Second).Add(time.Second)

	in := &s3.PutObjectInput{
		Bucket:                    aws.String(a.cfg.Bucket),
		Key:                       aws.String(key),
		Body:                      bytes.NewReader(body),
		ContentType:               aws.String("application/json"),
		ChecksumAlgorithm:         types.ChecksumAlgorithmSha256,
		IfNoneMatch:               aws.String("*"),
		ExpectedBucketOwner:       aws.String(a.cfg.ExpectedBucketOwner),
		ObjectLockMode:            types.ObjectLockMode(a.cfg.Mode),
		ObjectLockRetainUntilDate: aws.Time(until),
	}
	if a.cfg.LegalHold {
		in.ObjectLockLegalHoldStatus = types.ObjectLockLegalHoldStatusOn
	}
	if a.cfg.KMSKeyID != "" {
		in.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		in.SSEKMSKeyId = aws.String(a.cfg.KMSKeyID)
		in.BucketKeyEnabled = aws.Bool(true)
	}
	out, err := a.c.PutObject(ctx, in)
	switch {
	case err == nil:
		// Read back the version this write created.
		return a.readBack(ctx, key, aws.ToString(out.VersionId), body, until)
	case isPreconditionFailed(err):
		// The head is already there: this is a retry, or another process published the same head.
		return a.readBack(ctx, key, "", body, until)
	default:
		return a.wrap("write "+key, err)
	}
}

// readBack reads the object (the given version, or the current one) and checks it is the bytes
// written, locked and encrypted as configured, in the bucket's expected owner's account (the owner
// is sent with the read). The retention date it requires is the requested one less a minute of clock
// slack, or, for an object another publish wrote, any date at least the configured retention from
// that object's own write.
func (a *Anchor) readBack(ctx context.Context, key, version string, want []byte, until time.Time) error {
	in := &s3.GetObjectInput{
		Bucket:              aws.String(a.cfg.Bucket),
		Key:                 aws.String(key),
		ExpectedBucketOwner: aws.String(a.cfg.ExpectedBucketOwner),
		ChecksumMode:        types.ChecksumModeEnabled,
	}
	if version != "" {
		in.VersionId = aws.String(version)
	}
	out, err := a.c.GetObject(ctx, in)
	if err != nil {
		return a.wrap("read back "+key, err)
	}
	defer out.Body.Close()
	got, err := io.ReadAll(io.LimitReader(out.Body, int64(len(want))+1))
	if err != nil {
		return a.wrap("read back "+key, err)
	}
	if !bytes.Equal(got, want) {
		if version == "" {
			return fmt.Errorf("awss3: %s holds different bytes than the head being published: %w", key, ErrConflict)
		}
		return fmt.Errorf("awss3: %s read back different bytes than were written: %w", key, ErrConflict)
	}
	if err := a.checkStored(key, out.ObjectLockMode, out.ObjectLockRetainUntilDate, out.ObjectLockLegalHoldStatus, out.ServerSideEncryption, out.SSEKMSKeyId, out.VersionId, until, version == "", out.LastModified); err != nil {
		return err
	}
	return nil
}

// checkStored compares the lock and encryption S3 reports for a stored object with the
// configuration. For an object this call wrote (existing false) the retain-until date must be the
// one requested, less a minute of clock slack. An existing object, written earlier by a retry, another
// process or another tool, must be retained for at least the configured retention from its own write
// (less the same slack): a head some other writer stored under a weaker lock does not count as anchored.
func (a *Anchor) checkStored(key string, mode types.ObjectLockMode, until *time.Time, hold types.ObjectLockLegalHoldStatus, sse types.ServerSideEncryption, kms, version *string, wantUntil time.Time, existing bool, lastModified *time.Time) error {
	weak := func(format string, args ...any) error {
		return fmt.Errorf("awss3: %s: "+format+": %w", append(append([]any{key}, args...), ErrWeak)...)
	}
	if aws.ToString(version) == "" || aws.ToString(version) == "null" {
		return weak("the object has no version ID: versioning is not enabled")
	}
	if mode != a.cfg.Mode {
		return weak("lock mode is %q, want %q", mode, a.cfg.Mode)
	}
	if until == nil {
		return weak("no retain-until date")
	}
	if existing {
		// Another write (a retry, another process, another tool) stored these bytes. Its lock must be
		// as strong as this publish asks for, counted from its own write: S3 reports LastModified.
		if lastModified == nil {
			return weak("S3 reported no LastModified, so the retention of an object written earlier cannot be checked")
		}
		if floor := lastModified.Add(a.cfg.Retention).Add(-time.Minute); until.Before(floor) {
			return weak("an object written earlier is retained until %s, less than the configured %s after its write (%s)", until.UTC().Format(time.RFC3339), a.cfg.Retention, lastModified.UTC().Format(time.RFC3339))
		}
	} else if until.Before(wantUntil.Add(-time.Minute)) {
		return weak("retain-until is %s, want %s", until.UTC().Format(time.RFC3339), wantUntil.UTC().Format(time.RFC3339))
	}
	if a.cfg.LegalHold && hold != types.ObjectLockLegalHoldStatusOn {
		return weak("legal hold is %q, want ON", hold)
	}
	if a.cfg.KMSKeyID != "" {
		if sse != types.ServerSideEncryptionAwsKms {
			return weak("encryption is %q, want aws:kms", sse)
		}
		if aws.ToString(kms) == "" || !kmsMatches(a.cfg.KMSKeyID, aws.ToString(kms)) {
			return weak("encrypted with KMS key %q, want %q", aws.ToString(kms), a.cfg.KMSKeyID)
		}
	}
	return nil
}

// kmsMatches reports whether the key S3 reports (always an ARN) is the configured one: equal when
// the configuration is an ARN, the ARN's key ID when it is a bare ID, or any ARN when it is an alias
// (S3 resolves an alias to the key's ARN, which this package cannot know).
func kmsMatches(configured, reported string) bool {
	switch {
	case configured == reported:
		return true
	case strings.HasPrefix(configured, "alias/") || strings.Contains(configured, ":alias/"):
		return strings.HasPrefix(reported, "arn:")
	default:
		return strings.HasSuffix(reported, "/"+configured)
	}
}
