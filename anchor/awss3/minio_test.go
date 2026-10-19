package awss3_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bide-ai/bide/anchor/awss3"
	"github.com/bide-ai/bide/audit"
)

// These tests run the anchor against a real S3 API server that implements Object Lock: MinIO in CI
// (see .github/workflows/ci.yml). They need BIDE_S3_ENDPOINT (and AWS_ACCESS_KEY_ID and
// AWS_SECRET_ACCESS_KEY). They skip without it, except under BIDE_REQUIRE_OBJECTLOCK=1, where a
// missing server fails the test: the Integration job must not pass by skipping them.
//
// MinIO cannot show what only AWS does: SSE-KMS (it has no KMS here, so the tests set
// EncryptionNone), the check of x-amz-expected-bucket-owner (MinIO ignores the header; the fake tests
// that every request carries it), and AWS's own retention bookkeeping.

func realClient(t *testing.T) *s3.Client {
	t.Helper()
	endpoint := os.Getenv("BIDE_S3_ENDPOINT")
	if endpoint == "" {
		if os.Getenv("BIDE_REQUIRE_OBJECTLOCK") == "1" {
			t.Fatal("BIDE_REQUIRE_OBJECTLOCK=1 but BIDE_S3_ENDPOINT is not set: the Object Lock tests must not skip")
		}
		t.Skip("BIDE_S3_ENDPOINT is not set (an S3 API server with Object Lock, such as MinIO)")
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = aws.String(endpoint); o.UsePathStyle = true })
}

var bucketSeq int

// newBucket creates a bucket; lock enables Object Lock (and so versioning), retention sets a default.
func newBucket(t *testing.T, c *s3.Client, lock bool, mode types.ObjectLockRetentionMode, days int32) string {
	t.Helper()
	bucketSeq++
	name := fmt.Sprintf("bide-anchor-%d-%d", time.Now().UnixNano()%1e9, bucketSeq)
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name), ObjectLockEnabledForBucket: aws.Bool(lock)}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if lock && mode != "" {
		if _, err := c.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{Bucket: aws.String(name),
			ObjectLockConfiguration: &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled,
				Rule: &types.ObjectLockRule{DefaultRetention: &types.DefaultRetention{Mode: mode, Days: aws.Int32(days)}}}}); err != nil {
			t.Fatalf("set default retention: %v", err)
		}
	}
	return name
}

func realConfig(c *s3.Client, bucket string) awss3.Config {
	return awss3.Config{Client: c, Bucket: bucket, ExpectedBucketOwner: owner, Retention: 48 * time.Hour,
		EncryptionNone: true, OnWarn: func(error) {}}
}

func TestObjectLockServerRefusesBucketsThatAreNotAnchors(t *testing.T) {
	c := realClient(t)
	for name, b := range map[string]string{
		"no Object Lock":       newBucket(t, c, false, "", 0),
		"no default retention": newBucket(t, c, true, "", 0),
		"governance default":   newBucket(t, c, true, types.ObjectLockRetentionModeGovernance, 2),
	} {
		if _, err := awss3.New(ctx, realConfig(c, b)); err == nil {
			t.Errorf("%s: New accepted the bucket", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
}

func TestObjectLockServerAnchorsLocksAndReadsBack(t *testing.T) {
	c := realClient(t)
	k := newKeys(t, 1)
	b := newBucket(t, c, true, types.ObjectLockRetentionModeCompliance, 2)
	a, err := awss3.New(ctx, realConfig(c, b))
	if err != nil {
		t.Fatal(err)
	}
	h := k.head(t, "run-1", 4, 100)
	// Publish twice: the conditional write must make the second a no-op that still verifies.
	for range 2 {
		if err := a.Publish(ctx, "run-1", h); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Publish(ctx, "run-1", k.head(t, "run-1", 2, 50)); err != nil { // a smaller head after a larger one
		t.Fatal(err)
	}
	r, err := awss3.NewReader(awss3.ReaderConfig{Client: c, Bucket: b, ExpectedBucketOwner: owner, Verifier: k.v})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := r.Latest(ctx, audit.TreeJournal, "run-1")
	if err != nil || latest.Hash() != h.Hash() {
		t.Fatalf("Latest = %v, %v; want the size-4 head", latest.Size, err)
	}
	rep, err := r.Audit(ctx, audit.TreeJournal, "run-1", time.Now())
	if err != nil || len(rep.Heads) != 2 || len(rep.Findings) != 0 {
		t.Fatalf("Audit = %d heads, findings %v, %v; want 2 heads and no findings (one version per key: the retry wrote nothing)", len(rep.Heads), rep.Findings, err)
	}

	// What the server refuses: deleting a locked version, and shortening its retention.
	list, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(b)})
	if err != nil || len(list.Versions) != 2 {
		t.Fatalf("versions = %d, %v", len(list.Versions), err)
	}
	v := list.Versions[0]
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b), Key: v.Key, VersionId: v.VersionId}); err == nil {
		t.Error("the server deleted a compliance-locked version")
	}
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b), Key: v.Key, VersionId: v.VersionId, BypassGovernanceRetention: aws.Bool(true)}); err == nil {
		t.Error("the server let BypassGovernanceRetention delete a compliance-locked version")
	}
	if _, err := c.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{Bucket: aws.String(b), Key: v.Key, VersionId: v.VersionId,
		Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeCompliance, RetainUntilDate: aws.Time(time.Now().Add(time.Hour))}}); err == nil {
		t.Error("the server shortened a compliance retention")
	}
	// A delete marker is allowed, and leaves every version readable and audited.
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b), Key: v.Key}); err != nil {
		t.Fatal(err)
	}
	rep, err = r.Audit(ctx, audit.TreeJournal, "run-1", time.Now())
	if err != nil || len(rep.Heads) != 2 || !hasFinding(rep, "delete marker") {
		t.Fatalf("after a delete marker: %d heads, findings %v, %v", len(rep.Heads), rep.Findings, err)
	}
}

func TestObjectLockServerGovernanceAndLegalHold(t *testing.T) {
	c := realClient(t)
	k := newKeys(t, 1)
	b := newBucket(t, c, true, types.ObjectLockRetentionModeGovernance, 2)
	cfg := realConfig(c, b)
	cfg.Mode, cfg.AllowGovernance, cfg.LegalHold = types.ObjectLockModeGovernance, true, true
	var warned bool
	cfg.OnWarn = func(error) { warned = true }
	a, err := awss3.New(ctx, cfg)
	if err != nil || !warned {
		t.Fatalf("New = %v, warned %v; want governance accepted with a warning", err, warned)
	}
	if err := a.Publish(ctx, "run-1", k.head(t, "run-1", 1, 1)); err != nil {
		t.Fatal(err)
	}
	list, _ := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(b)})
	v := list.Versions[0]
	// The legal hold keeps the version even from the bypass.
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b), Key: v.Key, VersionId: v.VersionId, BypassGovernanceRetention: aws.Bool(true)}); err == nil {
		t.Error("the server deleted a version under legal hold")
	}
}

// The Reader's bucket-configuration findings against a real server: versioning, Object Lock, the default
// retention and the default encryption are read with the same calls the reader's policy allows.
func TestObjectLockServerBucketConfigurationFindings(t *testing.T) {
	c := realClient(t)
	k := newKeys(t, 1)
	reader := func(b string, mod func(*awss3.ReaderConfig)) *awss3.Reader {
		cfg := awss3.ReaderConfig{Client: c, Bucket: b, ExpectedBucketOwner: owner, Verifier: k.v, OnSkip: func(awss3.Finding) {}}
		if mod != nil {
			mod(&cfg)
		}
		r, err := awss3.NewReader(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	audit1 := func(r *awss3.Reader) []string {
		rep, err := r.Audit(ctx, audit.TreeJournal, "run-1", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range rep.Findings {
			out = append(out, f.String())
		}
		return out
	}
	has := func(got []string, sub string) bool {
		for _, g := range got {
			if strings.Contains(g, sub) {
				return true
			}
		}
		return false
	}

	good := newBucket(t, c, true, types.ObjectLockRetentionModeCompliance, 2)
	if got := audit1(reader(good, func(c *awss3.ReaderConfig) { c.Retention = 48 * time.Hour; c.Mode = types.ObjectLockModeCompliance })); len(got) != 0 {
		t.Errorf("an anchor bucket with its expectations met: %v", got)
	}
	if got := audit1(reader(good, func(c *awss3.ReaderConfig) { c.Retention = 30 * 24 * time.Hour })); !has(got, "shorter than the expected") {
		t.Errorf("a longer expected retention: %v", got)
	}
	if got := audit1(reader(good, func(c *awss3.ReaderConfig) { c.KMSKeyID = "arn:aws:kms:us-east-1:111122223333:key/abcd" })); !has(got, "encryption") {
		t.Errorf("an expected KMS key on a bucket without one: %v", got)
	}
	gov := newBucket(t, c, true, types.ObjectLockRetentionModeGovernance, 2)
	if got := audit1(reader(gov, func(c *awss3.ReaderConfig) { c.Mode = types.ObjectLockModeCompliance })); !has(got, "weaker than the expected COMPLIANCE") {
		t.Errorf("a governance default where compliance is expected: %v", got)
	}
	noDefault := newBucket(t, c, true, "", 0)
	if got := audit1(reader(noDefault, nil)); !has(got, "no default retention") {
		t.Errorf("a bucket with no default retention: %v", got)
	}
}
