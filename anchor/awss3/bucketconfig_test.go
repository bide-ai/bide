package awss3_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/anchor/awss3"
	"github.com/bide-ai/bide/anchor/awss3/awss3test"
	"github.com/bide-ai/bide/audit"
)

const bucketKey = "arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

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
		{name: "default KMS key swapped", weak: func(f *awss3test.Fake) {
			f.DefaultKMSKeyID = "arn:aws:kms:eu-west-1:111122223333:key/aaaabbbb-cc00-4dd1-8ee2-ffff00001111"
		}, want: "default KMS key is"},
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
		"another key": func(v *awss3test.Version) {
			v.KMSKey = "arn:aws:kms:eu-west-1:111122223333:key/aaaabbbb-cc00-4dd1-8ee2-ffff00001111"
		},
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

// An alias cannot be checked against the key ARN S3 reports, so an alias expectation would match any key:
// the Reader refuses it rather than weaken the check.
func TestReaderRefusesAKMSAlias(t *testing.T) {
	f := awss3test.New(bucket)
	k := newKeys(t, 1)
	for _, key := range []string{"alias/bide-anchor", "arn:aws:kms:eu-west-1:111122223333:alias/bide-anchor"} {
		_, err := awss3.NewReader(awss3.ReaderConfig{Client: f, Bucket: bucket, ExpectedBucketOwner: owner, Verifier: k.v, KMSKeyID: key})
		if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "for an alias, pass the ARN of the key it points to") {
			t.Errorf("KMSKeyID %q: NewReader = %v, want agent.ErrConfig naming the fix", key, err)
		}
	}
	// A key ID and a key ARN are accepted.
	for _, key := range []string{"1234abcd-12ab-34cd-56ef-1234567890ab", bucketKey} {
		if _, err := awss3.NewReader(awss3.ReaderConfig{Client: f, Bucket: bucket, ExpectedBucketOwner: owner, Verifier: k.v, KMSKeyID: key}); err != nil {
			t.Errorf("KMSKeyID %q refused: %v", key, err)
		}
	}
}

// A bare key ID names a key, not an alias: it must not match an alias ARN whose name equals it.
func TestBareKeyIDDoesNotMatchAnAliasARN(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	publish(t, f, k, "r", 1)
	r := expecting(t, f, k.v, func(c *awss3.ReaderConfig) { c.KMSKeyID = "1234abcd-12ab-34cd-56ef-1234567890ab" })
	if got := findingsOf(t, r); len(got) != 0 {
		t.Fatalf("a bare ID against its key ARN: %v", got)
	}
	f.DefaultKMSKeyID = "arn:aws:kms:eu-west-1:111122223333:alias/1234abcd-12ab-34cd-56ef-1234567890ab"
	got := findingsOf(t, r)
	if len(got) != 1 || !strings.Contains(got[0], "default KMS key is") {
		t.Fatalf("a bare ID against an alias ARN of the same name: %v; want a finding", got)
	}
}

// A default retention in years counts as 365 days a year: 7 years is 2555 days to the check.
func TestDefaultRetentionInYearsCountsAs365DaysAYear(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	publish(t, f, k, "r", 1)
	f.DefaultYears = 7
	audit := func(days int) []string {
		r := expecting(t, f, k.v, func(c *awss3.ReaderConfig) { c.Retention = time.Duration(days) * 24 * time.Hour })
		return bucketOnly(findingsOf(t, r))
	}
	if got := audit(2555); len(got) != 0 {
		t.Errorf("7 years against 2555 days: %v", got)
	}
	if got := audit(2557); len(got) != 1 || !strings.Contains(got[0], "shorter than the expected") {
		t.Errorf("7 years against 2557 days: %v; want the documented finding", got)
	}
}

// bucketOnly keeps the findings about the bucket and drops those about objects.
func bucketOnly(all []string) []string {
	var out []string
	for _, f := range all {
		if strings.HasPrefix(f, "bucket "+bucket) {
			out = append(out, f)
		}
	}
	return out
}

// Where a key is configured only a key ID, a multi-Region key ID or a key ARN is accepted: an allow-check,
// so an odd string that is no alias to KMS (and no key either) is refused too, by the writer and the Reader.
func TestOnlyAKeyIDOrARNIsAcceptedAsTheKMSKey(t *testing.T) {
	const id = "1234abcd-12ab-34cd-56ef-1234567890ab"
	const mrk = "mrk-1234abcd12ab34cd56ef1234567890ab"
	good := []string{
		id, mrk,
		"arn:aws:kms:eu-west-1:111122223333:key/" + id,
		"arn:aws-cn:kms:cn-north-1:111122223333:key/" + id,
		"arn:aws-us-gov:kms:us-gov-west-1:111122223333:key/" + mrk,
	}
	bad := []string{
		"alias/bide-anchor", "arn:aws:kms:eu-west-1:111122223333:alias/bide-anchor", "alias/bide-anchor\n",
		" alias/x", "\talias/x", "ALIAS/x", "Alias/x", "my-alias/x", "alias", "key", "k", " " + id, id + " ", id + "\n",
		"1234ABCD-12ab-34cd-56ef-1234567890ab", "1234abcd-12AB-34cd-56ef-1234567890ab", "1234abcd-12ab-34CD-56ef-1234567890ab", "1234abcd-12ab-34cd-56EF-1234567890ab", "1234abcd-12ab-34cd-56ef-1234567890AB", "mrk-1234ABCD12ab34cd56ef1234567890ab",
		"1234ABCD-12AB-34CD-56EF-1234567890AB",        // a key ID is lower case
		"1234abcd-12ab-34cd-56ef-1234567890a",         // too short
		"mrk-1234abcd12ab34cd56ef1234567890a",         // too short
		"MRK-1234abcd12ab34cd56ef1234567890ab",        // the prefix is lower case
		"arn:aws:kms:eu-west-1:111122223333:key/k",    // not an ID
		"arn:aws:kms:eu-west-1:1111:key/" + id,        // not an account
		"arn:aws:kms::111122223333:key/" + id,         // no region
		"arn:aws:s3:eu-west-1:111122223333:key/" + id, // not KMS
		"arn:aws:kms:eu-west-1:111122223333:key/" + id + "/extra",
		"arn:aws:kms:eu-west-1:111122223333:key/" + id + "\n",
		"projects/p/locations/l/keyRings/r/cryptoKeys/k", // a Google Cloud name
	}
	k := newKeys(t, 1)
	for _, key := range good {
		f := awss3test.New(bucket)
		if _, err := awss3.NewReader(awss3.ReaderConfig{Client: f, Bucket: bucket, ExpectedBucketOwner: owner, Verifier: k.v, KMSKeyID: key}); err != nil {
			t.Errorf("the Reader refused %q: %v", key, err)
		}
		if _, err := awss3.New(ctx, awss3.Config{Client: f, Bucket: bucket, ExpectedBucketOwner: owner, Retention: 30 * 24 * time.Hour, KMSKeyID: key, OnWarn: func(error) {}}); err != nil {
			t.Errorf("New refused %q: %v", key, err)
		}
	}
	for _, key := range bad {
		f := awss3test.New(bucket)
		_, err := awss3.NewReader(awss3.ReaderConfig{Client: f, Bucket: bucket, ExpectedBucketOwner: owner, Verifier: k.v, KMSKeyID: key})
		if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "for an alias, pass the ARN of the key it points to") {
			t.Errorf("the Reader accepted or mis-reported %q: %v", key, err)
		}
		_, err = awss3.New(ctx, awss3.Config{Client: f, Bucket: bucket, ExpectedBucketOwner: owner, Retention: 30 * 24 * time.Hour, KMSKeyID: key, OnWarn: func(error) {}})
		if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "for an alias, pass the ARN of the key it points to") {
			t.Errorf("New accepted or mis-reported %q: %v", key, err)
		}
		if n := f.Calls("PutObject") + f.Calls("HeadBucket"); n != 0 {
			t.Errorf("%q: %d requests were made before the refusal", key, n)
		}
	}
}

// The Upgrading note quotes the refusal's text so that an operator can search a log for it: the real
// message contains every piece the note quotes.
func TestUpgradingNoteQuotesTheRefusalText(t *testing.T) {
	frag, err := os.ReadFile(filepath.Join("..", "..", "changes", "unreleased", "awss3-refuse-kms-alias.md"))
	if err != nil {
		t.Skip("the changelog fragment is assembled at release: " + err.Error())
	}
	_, got := awss3.New(ctx, awss3.Config{Client: awss3test.New(bucket), Bucket: bucket, ExpectedBucketOwner: owner, Retention: 30 * 24 * time.Hour, KMSKeyID: "alias/x"})
	if got == nil {
		t.Fatal("New accepted an alias")
	}
	for _, piece := range []string{"awss3: Config.KMSKeyID: KMS key must be a key ID or key ARN (got ", "a lower-case UUID, mrk- and 32 lower-case hex digits, or arn:<partition>:kms:<region>:<account>:key/<one of those>", "for an alias, pass the ARN of the key it points to"} {
		if !strings.Contains(got.Error(), piece) {
			t.Errorf("the error does not contain %q", piece)
		}
		if !strings.Contains(string(frag), piece) {
			t.Errorf("the Upgrading note does not quote %q", piece)
		}
	}
	if !strings.Contains(string(frag), "upper-case key ID") {
		t.Error("the Upgrading note does not say upper-case key IDs are refused")
	}
}
