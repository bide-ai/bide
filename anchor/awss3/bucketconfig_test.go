package awss3_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/anchor/awss3"
	"github.com/bide-ai/bide/anchor/awss3/awss3test"
	"github.com/bide-ai/bide/audit"
)

const bucketKey = "arn:aws:kms:eu-west-1:111122223333:key/abcd-1234"

// expecting is a Reader that expects the key, retention and mode the fake bucket is configured with.
func expecting(t testing.TB, f *awss3test.Fake, v audit.Verifier, mod func(*awss3.ReaderConfig)) *awss3.Reader {
	t.Helper()
	cfg := awss3.ReaderConfig{Client: f, Bucket: bucket, ExpectedBucketOwner: owner, Verifier: v,
		KMSKeyID: bucketKey, Retention: 30 * 24 * time.Hour, Mode: types.ObjectLockModeCompliance, OnSkip: func(awss3.Finding) {}}
	if mod != nil {
		mod(&cfg)
	}
	r, err := awss3.NewReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func findingsOf(t testing.TB, r *awss3.Reader) []string {
	t.Helper()
	rep, err := r.Audit(ctx, audit.TreeJournal, "r", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range rep.Findings {
		out = append(out, f.String())
	}
	return out
}

func TestBucketConfigurationFindings(t *testing.T) {
	k := newKeys(t, 1)
	for _, tc := range []struct {
		name string
		weak func(*awss3test.Fake)
		cfg  func(*awss3.ReaderConfig)
		want string // "" means no finding about the bucket
	}{
		{name: "intact, every expectation met"},
		{name: "versioning suspended", weak: func(f *awss3test.Fake) { f.Versioning = types.BucketVersioningStatusSuspended }, want: "versioning is"},
		{name: "versioning never enabled", weak: func(f *awss3test.Fake) { f.Versioning = "" }, want: "versioning is"},
		{name: "Object Lock off", weak: func(f *awss3test.Fake) { f.LockEnabled = false }, want: "Object Lock is not enabled"},
		{name: "no default retention", weak: func(f *awss3test.Fake) { f.DefaultMode = "" }, want: "no default retention"},
		{name: "default retention below the minimum", weak: func(f *awss3test.Fake) { f.DefaultDays = 0 }, cfg: func(c *awss3.ReaderConfig) { c.Retention = 0 }, want: "below the minimum"},
		{name: "default retention shorter than expected", weak: func(f *awss3test.Fake) { f.DefaultDays = 10 }, want: "shorter than the expected"},
		{name: "governance default where compliance is expected", weak: func(f *awss3test.Fake) { f.DefaultMode = types.ObjectLockRetentionModeGovernance }, want: "weaker than the expected COMPLIANCE"},
		{name: "default encryption is not KMS", weak: func(f *awss3test.Fake) { f.DefaultKMS = false }, want: "default encryption is not SSE-KMS"},
		{name: "default KMS key swapped", weak: func(f *awss3test.Fake) { f.DefaultKMSKeyID = "arn:aws:kms:eu-west-1:111122223333:key/other" }, want: "default KMS key is"},
		{name: "default key is the AWS-managed one", weak: func(f *awss3test.Fake) { f.DefaultKMSKeyID = "" }, want: "default KMS key is"},
		// A governance default is not weaker than a governance expectation, and nothing is judged
		// about an expectation the reader was not given.
		{name: "governance expected and found", weak: func(f *awss3test.Fake) { f.DefaultMode = types.ObjectLockRetentionModeGovernance },
			cfg: func(c *awss3.ReaderConfig) { c.Mode = types.ObjectLockModeGovernance }},
		{name: "no expectations: a swapped key and a short retention pass", weak: func(f *awss3test.Fake) { f.DefaultKMSKeyID = "x"; f.DefaultDays = 2 },
			cfg: func(c *awss3.ReaderConfig) { c.KMSKeyID, c.Retention, c.Mode = "", 0, "" }},
		{name: "no expectations: the unconditional checks still run", weak: func(f *awss3test.Fake) { f.Versioning = types.BucketVersioningStatusSuspended },
			cfg: func(c *awss3.ReaderConfig) { c.KMSKeyID, c.Retention, c.Mode = "", 0, "" }, want: "versioning is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := awss3test.New(bucket)
			publish(t, f, k, "r", 1)
			if tc.weak != nil {
				tc.weak(f)
			}
			got := findingsOf(t, expecting(t, f, k.v, tc.cfg))
			var bucketFindings []string
			for _, g := range got {
				if strings.HasPrefix(g, "bucket "+bucket) {
					bucketFindings = append(bucketFindings, g)
				}
			}
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("findings %v; want none", got)
				}
				return
			}
			if len(bucketFindings) != 1 || !strings.Contains(bucketFindings[0], tc.want) {
				t.Fatalf("bucket findings %v; want one containing %q", bucketFindings, tc.want)
			}
		})
	}
}

func TestObjectFindingsAgainstExpectations(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	publish(t, f, k, "r", 1)
	key := f.Keys()[0]
	if got := findingsOf(t, expecting(t, f, k.v, nil)); len(got) != 0 {
		t.Fatalf("intact: %v", got)
	}
	// An object encrypted with another key, and one that is not KMS-encrypted at all.
	for name, tamper := range map[string]func(*awss3test.Version){
		"another key":   func(v *awss3test.Version) { v.KMSKey = "arn:aws:kms:eu-west-1:111122223333:key/other" },
		"no encryption": func(v *awss3test.Version) { v.SSE = "" },
		"retention cut": func(v *awss3test.Version) { v.RetainUntil = v.Created.Add(24 * time.Hour) },
	} {
		f.Tamper = func(_ string, v *awss3test.Version) { tamper(v) }
		got := findingsOf(t, expecting(t, f, k.v, nil))
		if len(got) != 1 || !strings.Contains(got[0], key[strings.LastIndex(key, "/")+1:]) {
			t.Errorf("%s: findings %v; want one naming the object", name, got)
		}
		// Without the expectation the same object is not judged on it.
		off := expecting(t, f, k.v, func(c *awss3.ReaderConfig) { c.KMSKeyID, c.Retention = "", 0 })
		if got := findingsOf(t, off); len(got) != 0 {
			t.Errorf("%s without expectations: %v", name, got)
		}
	}
	f.Tamper = nil
}

func TestGovernanceObjectsAreNotAFindingWhenGovernanceIsExpected(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	f.DefaultMode = types.ObjectLockRetentionModeGovernance
	a := newAnchor(t, f, func(c *awss3.Config) { c.Mode = types.ObjectLockModeGovernance; c.AllowGovernance = true })
	if err := a.Publish(ctx, "r", k.head(t, "r", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if got := findingsOf(t, expecting(t, f, k.v, func(c *awss3.ReaderConfig) { c.Mode = types.ObjectLockModeGovernance })); len(got) != 0 {
		t.Errorf("governance expected: %v", got)
	}
	if got := findingsOf(t, expecting(t, f, k.v, func(c *awss3.ReaderConfig) { c.Mode = "" })); len(got) == 0 || !strings.Contains(got[0], "governance mode") {
		t.Errorf("no mode expectation: %v, want the governance finding", got)
	}
}

// A bucket-configuration read that is denied or unavailable aborts the audit: the state is unknown.
func TestBucketConfigurationReadFailureAbortsTheAudit(t *testing.T) {
	k := newKeys(t, 1)
	for _, op := range []string{"GetBucketVersioning", "GetObjectLockConfiguration", "GetBucketEncryption"} {
		for name, fault := range map[string]error{"denied": awss3test.APIError("AccessDenied", 403), "unavailable": errors.New("connection reset")} {
			f := awss3test.New(bucket)
			publish(t, f, k, "r", 1)
			f.Hook = func(o string, _ int) error {
				if o == op {
					return fault
				}
				return nil
			}
			rep, err := expecting(t, f, k.v, nil).Audit(ctx, audit.TreeJournal, "r", time.Now())
			if err == nil || len(rep.Heads) != 0 || len(rep.Findings) != 0 {
				t.Errorf("%s %s: Audit = %d heads, %d findings, %v; want an error and an empty report", op, name, len(rep.Heads), len(rep.Findings), err)
			}
			if name == "unavailable" && !errors.Is(err, awss3.ErrUnavailable) {
				t.Errorf("%s: %v does not wrap ErrUnavailable", op, err)
			}
		}
	}
}

// The bucket's encryption is read only when a key is expected, so a reader without that expectation needs
// no s3:GetEncryptionConfiguration.
func TestEncryptionIsReadOnlyWhenAKeyIsExpected(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	publish(t, f, k, "r", 1)
	findingsOf(t, expecting(t, f, k.v, func(c *awss3.ReaderConfig) { c.KMSKeyID = "" }))
	if n := f.Calls("GetBucketEncryption"); n != 1 { // the Anchor's own startup check made the one call
		t.Errorf("GetBucketEncryption called %d times, want only the writer's start-up check", n)
	}
	findingsOf(t, expecting(t, f, k.v, nil))
	if f.Calls("GetBucketEncryption") != 2 {
		t.Errorf("a reader expecting a key did not read the default encryption")
	}
}

func TestReaderConfigRefusesWhatCannotBeExpected(t *testing.T) {
	f := awss3test.New(bucket)
	k := newKeys(t, 1)
	for name, mod := range map[string]func(*awss3.ReaderConfig){
		"unknown mode":       func(c *awss3.ReaderConfig) { c.Mode = "WORM" },
		"negative retention": func(c *awss3.ReaderConfig) { c.Retention = -time.Hour },
	} {
		cfg := awss3.ReaderConfig{Client: f, Bucket: bucket, ExpectedBucketOwner: owner, Verifier: k.v}
		mod(&cfg)
		if _, err := awss3.NewReader(cfg); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: NewReader = %v, want agent.ErrConfig", name, err)
		}
	}
}
