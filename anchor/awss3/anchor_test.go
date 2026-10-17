package awss3_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/anchor/awss3"
	"github.com/bide-ai/bide/anchor/awss3/awss3test"
	"github.com/bide-ai/bide/audit"
)

func TestNewRefusesWhatIsNotAnAnchorBucket(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    func(*awss3test.Fake)
		cfg  func(*awss3.Config)
		want string
	}{
		{name: "no Object Lock", f: func(f *awss3test.Fake) { f.LockEnabled = false }, want: "Object Lock is not enabled"},
		{name: "no versioning", f: func(f *awss3test.Fake) { f.Versioning = types.BucketVersioningStatusSuspended }, want: "versioning"},
		{name: "no default retention", f: func(f *awss3test.Fake) { f.DefaultMode = "" }, want: "no default retention"},
		{name: "governance default under compliance", f: func(f *awss3test.Fake) { f.DefaultMode = types.ObjectLockRetentionModeGovernance }, want: "weaker than the configured COMPLIANCE"},
		{name: "default retention shorter than configured", f: func(f *awss3test.Fake) { f.DefaultDays = 10 }, want: "shorter than the configured"},
		{name: "default retention below the minimum", f: func(f *awss3test.Fake) { f.DefaultDays = 0 }, want: "below the minimum"},
		{name: "another account owns the bucket", f: func(f *awss3test.Fake) { f.Owner = "999988887777" }, want: "AccessDenied"},
		{name: "no client", cfg: func(c *awss3.Config) { c.Client = nil }, want: "Client is nil"},
		{name: "no bucket", cfg: func(c *awss3.Config) { c.Bucket = "" }, want: "Bucket is empty"},
		{name: "no owner", cfg: func(c *awss3.Config) { c.ExpectedBucketOwner = "" }, want: "ExpectedBucketOwner"},
		{name: "short owner", cfg: func(c *awss3.Config) { c.ExpectedBucketOwner = "1234" }, want: "ExpectedBucketOwner"},
		{name: "retention too short", cfg: func(c *awss3.Config) { c.Retention = time.Hour }, want: "below the minimum"},
		{name: "no KMS key", cfg: func(c *awss3.Config) { c.KMSKeyID = "" }, want: "KMSKeyID is empty"},
		{name: "KMS and none", cfg: func(c *awss3.Config) { c.EncryptionNone = true }, want: "both set"},
		{name: "governance without opt-in", cfg: func(c *awss3.Config) { c.Mode = types.ObjectLockModeGovernance }, want: "AllowGovernance"},
		{name: "unknown mode", cfg: func(c *awss3.Config) { c.Mode = "WORM" }, want: "not COMPLIANCE or GOVERNANCE"},
		{name: "bad prefix", cfg: func(c *awss3.Config) { c.Prefix = "anchor" }, want: "must end in /"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := awss3test.New(bucket)
			if tc.f != nil {
				tc.f(f)
			}
			cfg := baseConfig(f)
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			a, err := awss3.New(ctx, cfg)
			if err == nil || a != nil {
				t.Fatalf("New = %v, %v; want a refusal", a, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if tc.f != nil && tc.name != "another account owns the bucket" && !errors.Is(err, agent.ErrConfig) {
				t.Errorf("error %v does not wrap agent.ErrConfig", err)
			}
			if f.Calls("PutObject") != 0 {
				t.Error("New wrote an object")
			}
		})
	}
}

func TestNewGovernanceWarnsEveryTime(t *testing.T) {
	f := awss3test.New(bucket)
	f.DefaultMode = types.ObjectLockRetentionModeGovernance
	var warns []error
	_, err := awss3.New(ctx, awss3.Config{Client: f, Bucket: bucket, ExpectedBucketOwner: owner, Retention: 48 * time.Hour,
		KMSKeyID: kmsARN, Mode: types.ObjectLockModeGovernance, AllowGovernance: true, OnWarn: func(e error) { warns = append(warns, e) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0].Error(), "BypassGovernanceRetention") {
		t.Fatalf("warnings = %v, want one naming s3:BypassGovernanceRetention", warns)
	}
}

func TestNewWarnsWhenBucketDefaultIsNotKMS(t *testing.T) {
	f := awss3test.New(bucket)
	f.DefaultKMS = false
	var warns []error
	cfg := baseConfig(f)
	cfg.OnWarn = func(e error) { warns = append(warns, e) }
	if _, err := awss3.New(ctx, cfg); err != nil || len(warns) != 1 {
		t.Fatalf("New = %v, warnings %v; want success and one warning", err, warns)
	}
}

func TestPublishWritesALockedEncryptedObjectAndReadsItBack(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	a := newAnchor(t, f, func(c *awss3.Config) { c.Now = func() time.Time { return now }; c.LegalHold = true })
	sth := k.head(t, "run-1", 3, 100)
	if err := a.Publish(ctx, "run-1", sth); err != nil {
		t.Fatal(err)
	}
	keys := f.Keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "bide-anchor/") || strings.Contains(keys[0], "run-1") {
		t.Fatalf("keys = %v; want one under bide-anchor/ that does not name the run", keys)
	}
	v := f.Versions(keys[0])
	if len(v) != 1 || v[0].LockMode != types.ObjectLockModeCompliance || v[0].SSE != types.ServerSideEncryptionAwsKms ||
		v[0].LegalHold != types.ObjectLockLegalHoldStatusOn || !v[0].RetainUntil.After(now.Add(30*24*time.Hour-time.Second)) {
		t.Fatalf("stored version = %+v; want compliance, SSE-KMS, legal hold, retained 30 days", v)
	}
	got, err := newReader(t, f, k.v).Latest(ctx, audit.TreeJournal, "run-1")
	if err != nil || got.Hash() != sth.Hash() {
		t.Fatalf("Latest = %v, %v; want the published head", got, err)
	}
}

func TestPublishIsIdempotentAndKeepsDistinctHeadsOfOneSize(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, nil)
	h := k.head(t, "r", 2, 100)
	for range 3 {
		if err := a.Publish(ctx, "r", h); err != nil {
			t.Fatal(err)
		}
	}
	if keys := f.Keys(); len(keys) != 1 || len(f.Versions(keys[0])) != 1 {
		t.Fatalf("keys %v: a retried head must stay one object with one version", f.Keys())
	}
	// A second process anchors the same size a moment later: a different head, a different object.
	if err := a.Publish(ctx, "r", k.head(t, "r", 2, 200)); err != nil {
		t.Fatal(err)
	}
	heads, err := newReader(t, f, k.v).Head(ctx, audit.TreeJournal, "r", 2)
	if err != nil || len(heads) != 2 {
		t.Fatalf("Head(2) = %d heads, %v; want 2", len(heads), err)
	}
}

func TestPublishAcceptsASmallerHeadAfterALargerOne(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, nil)
	for _, size := range []int{5, 3, 9} {
		if err := a.Publish(ctx, "r", k.head(t, "r", size, int64(size))); err != nil {
			t.Fatal(err)
		}
	}
	r := newReader(t, f, k.v)
	latest, err := r.Latest(ctx, audit.TreeJournal, "r")
	if err != nil || latest.Size != 9 {
		t.Fatalf("Latest = size %d, %v; want 9", latest.Size, err)
	}
	all, err := r.Heads(ctx, audit.TreeJournal, "r") // three heads span two listing pages in the fake
	if err != nil || len(all) != 3 || all[0].Size != 3 || all[1].Size != 5 || all[2].Size != 9 {
		t.Fatalf("Heads = %v, %v; want sizes 3,5,9 in order", sizes(all), err)
	}
	if _, err := r.Head(ctx, audit.TreeJournal, "r", 4); !errors.Is(err, awss3.ErrNotFound) {
		t.Errorf("Head(4) = %v, want ErrNotFound", err)
	}
	if _, err := r.Latest(ctx, audit.TreeEvents, "r"); !errors.Is(err, awss3.ErrNotFound) {
		t.Errorf("Latest of another kind = %v, want ErrNotFound", err)
	}
}

func sizes(hs []audit.SignedTreeHead) []int {
	var out []int
	for _, h := range hs {
		out = append(out, h.Size)
	}
	return out
}

func TestPublishRefusesWhatIsNotAnAnchorableHead(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, nil)
	if err := a.Publish(ctx, "other", k.head(t, "r", 1, 1)); err == nil {
		t.Error("a head for another run was anchored")
	}
	unsigned := k.head(t, "r", 1, 1)
	unsigned.Signature = nil
	if err := a.Publish(ctx, "r", unsigned); err == nil {
		t.Error("an unsigned head was anchored")
	}
	if len(f.Keys()) != 0 {
		t.Error("a refused head left an object")
	}
}

func TestPublishNetworkFailureIsExplicitAndRetryable(t *testing.T) {
	k := newKeys(t, 1)
	for _, tc := range []struct {
		name string
		hook func(op string, n int) error
	}{
		{"transport error", func(op string, n int) error {
			if op == "PutObject" && n == 1 {
				return errors.New("connection reset by peer")
			}
			return nil
		}},
		{"throttle", func(op string, n int) error {
			if op == "PutObject" && n == 1 {
				return awss3test.APIError("SlowDown", 503)
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := awss3test.New(bucket)
			a := newAnchor(t, f, nil)
			f.Hook = tc.hook
			h := k.head(t, "r", 1, 1)
			err := a.Publish(ctx, "r", h)
			if !errors.Is(err, awss3.ErrUnavailable) || len(f.Keys()) != 0 {
				t.Fatalf("Publish = %v, keys %v; want ErrUnavailable and nothing stored", err, f.Keys())
			}
			if err := a.Publish(ctx, "r", h); err != nil {
				t.Fatalf("retry: %v", err)
			}
		})
	}
	t.Run("a context that ended", func(t *testing.T) {
		f := awss3test.New(bucket)
		a := newAnchor(t, f, nil)
		f.Hook = func(string, int) error { return context.DeadlineExceeded }
		if err := a.Publish(ctx, "r", k.head(t, "r", 1, 1)); !errors.Is(err, awss3.ErrUnavailable) {
			t.Fatalf("Publish = %v, want ErrUnavailable", err)
		}
	})
}

func TestPublishAccessDeniedIsNotUnavailable(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, nil)
	f.Hook = func(op string, _ int) error {
		if op == "PutObject" {
			return awss3test.APIError("AccessDenied", 403)
		}
		return nil
	}
	err := a.Publish(ctx, "r", k.head(t, "r", 1, 1))
	if err == nil || errors.Is(err, awss3.ErrUnavailable) {
		t.Fatalf("Publish = %v; an access error must fail and not read as an outage", err)
	}
}

func TestPublishAfterALostResponseFindsItsOwnObject(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, nil)
	f.AfterCommit = func(n int) error {
		if n == 1 {
			return errors.New("response lost")
		}
		return nil
	}
	h := k.head(t, "r", 1, 1)
	if err := a.Publish(ctx, "r", h); !errors.Is(err, awss3.ErrUnavailable) {
		t.Fatalf("first Publish = %v, want ErrUnavailable (the write may have landed)", err)
	}
	if err := a.Publish(ctx, "r", h); err != nil {
		t.Fatalf("retry = %v; the head was already stored and must verify", err)
	}
	if k := f.Keys(); len(k) != 1 || len(f.Versions(k[0])) != 1 {
		t.Fatalf("keys %v: the retry must not write a second version", f.Keys())
	}
}

func TestPublishRefusesAnObjectThatReadsBackWeaker(t *testing.T) {
	k := newKeys(t, 1)
	for _, tc := range []struct {
		name   string
		tamper func(*awss3test.Version)
		want   error
	}{
		{"governance lock", func(v *awss3test.Version) { v.LockMode = types.ObjectLockModeGovernance }, awss3.ErrWeak},
		{"no lock", func(v *awss3test.Version) { v.LockMode = "" }, awss3.ErrWeak},
		{"short retention", func(v *awss3test.Version) { v.RetainUntil = v.RetainUntil.Add(-24 * time.Hour) }, awss3.ErrWeak},
		{"no encryption", func(v *awss3test.Version) { v.SSE = "" }, awss3.ErrWeak},
		{"another KMS key", func(v *awss3test.Version) { v.KMSKey = "arn:aws:kms:eu-west-1:111122223333:key/other" }, awss3.ErrWeak},
		{"no version ID", func(v *awss3test.Version) { v.ID = "null" }, awss3.ErrWeak},
		{"different bytes", func(v *awss3test.Version) { v.Body = append([]byte(nil), v.Body[:len(v.Body)-2]...) }, awss3.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := awss3test.New(bucket)
			a := newAnchor(t, f, nil)
			f.Tamper = func(_ string, v *awss3test.Version) { tc.tamper(v) }
			if err := a.Publish(ctx, "r", k.head(t, "r", 1, 1)); !errors.Is(err, tc.want) {
				t.Fatalf("Publish = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPublishRequiresTheLegalHoldWhenConfigured(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, func(c *awss3.Config) { c.LegalHold = true })
	f.Tamper = func(_ string, v *awss3test.Version) { v.LegalHold = types.ObjectLockLegalHoldStatusOff }
	if err := a.Publish(ctx, "r", k.head(t, "r", 1, 1)); !errors.Is(err, awss3.ErrWeak) {
		t.Fatalf("Publish = %v, want ErrWeak", err)
	}
}

func TestPublishRefusesAKeyHoldingDifferentBytes(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, nil)
	h := k.head(t, "r", 1, 1)
	if err := a.Publish(ctx, "r", h); err != nil {
		t.Fatal(err)
	}
	key := f.Keys()[0]
	f.Tamper = func(_ string, v *awss3test.Version) { v.Body = []byte("{}") }
	if err := a.Publish(ctx, "r", h); !errors.Is(err, awss3.ErrConflict) {
		t.Fatalf("Publish over a key holding other bytes = %v, want ErrConflict (%s)", err, key)
	}
}

func TestEveryRequestCarriesTheExpectedBucketOwner(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, nil)
	if err := a.Publish(ctx, "r", k.head(t, "r", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := a.Publish(ctx, "r", k.head(t, "r", 1, 1)); err != nil { // the already-stored path
		t.Fatal(err)
	}
	r := newReader(t, f, k.v)
	r.Latest(ctx, audit.TreeJournal, "r")
	r.Heads(ctx, audit.TreeJournal, "r")
	r.Audit(ctx, audit.TreeJournal, "r", time.Now())
	for op, owners := range f.Seen {
		for i, o := range owners {
			if o != owner {
				t.Errorf("%s request %d carried owner %q, want %q", op, i+1, o, owner)
			}
		}
	}
	for _, op := range []string{"HeadBucket", "GetBucketVersioning", "GetObjectLockConfiguration", "GetBucketEncryption", "PutObject", "GetObject", "ListObjectVersions"} {
		if len(f.Seen[op]) == 0 {
			t.Errorf("no %s request was made", op)
		}
	}
}

func TestPublishFailsWhenTheBucketMovesToAnotherAccount(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, nil)
	f.Owner = "999988887777" // the name now resolves to a bucket of someone else
	if err := a.Publish(ctx, "r", k.head(t, "r", 1, 1)); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("Publish = %v, want AccessDenied", err)
	}
	if len(f.Keys()) != 0 {
		t.Error("a head was written to the other account's bucket")
	}
}

func TestSuspectIDsWarnOrAreRefused(t *testing.T) {
	k := newKeys(t, 1)
	for _, id := range []string{"refund-jane.doe@example.com", "acct-123456789012"} {
		var warns int
		f := awss3test.New(bucket)
		a := newAnchor(t, f, func(c *awss3.Config) { c.OnWarn = func(error) { warns++ } })
		if err := a.Publish(ctx, id, k.head(t, id, 1, 1)); err != nil || warns != 1 {
			t.Errorf("%q: Publish = %v, %d warnings; want success and one warning", id, err, warns)
		}
		strict := newAnchor(t, awss3test.New(bucket), func(c *awss3.Config) { c.RefuseSuspectIDs = true })
		if err := strict.Publish(ctx, id, k.head(t, id, 1, 1)); err == nil {
			t.Errorf("%q: RefuseSuspectIDs published it", id)
		}
	}
	for _, id := range []string{"run-2026-10-17-0001", "notify:@open:2026-10-17", "9b2e7c1a-5d34-4f10-8a6e-0c1d2e3f4a5b", "wf/refund/7"} {
		var warns int
		a := newAnchor(t, awss3test.New(bucket), func(c *awss3.Config) { c.OnWarn = func(error) { warns++ } })
		if err := a.Publish(ctx, id, k.head(t, id, 1, 1)); err != nil || warns != 0 {
			t.Errorf("%q: Publish = %v, %d warnings; want a quiet success", id, err, warns)
		}
	}
}

// The package's client has no way to delete an object, change a retention or bypass governance, so
// the application's credentials need none of those permissions.
func TestClientCannotDeleteOrBypass(t *testing.T) {
	ty := reflect.TypeFor[awss3.Client]()
	for i := range ty.NumMethod() {
		n := ty.Method(i).Name
		if strings.HasPrefix(n, "Delete") || strings.Contains(n, "Retention") || strings.Contains(n, "Bypass") || strings.HasPrefix(n, "Put") && n != "PutObject" {
			t.Errorf("Client has method %s", n)
		}
	}
}

// A compliance-mode head cannot be deleted before its retention date by anyone, and a delete marker
// leaves it readable.
func TestLockedHeadSurvivesDeleteAttempts(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, nil)
	if err := a.Publish(ctx, "r", k.head(t, "r", 4, 1)); err != nil {
		t.Fatal(err)
	}
	key := f.Keys()[0]
	ver := f.Versions(key)[0].ID
	if err := f.Delete(key, ver, true); err == nil {
		t.Fatal("a compliance-mode version was deleted (even with a bypass)")
	}
	if err := f.Delete(key, "", false); err != nil {
		t.Fatal(err)
	}
	r := newReader(t, f, k.v)
	if _, err := r.Latest(ctx, audit.TreeJournal, "r"); !errors.Is(err, awss3.ErrNotFound) {
		t.Logf("the delete marker hides the key from a plain listing: %v", err)
	}
	rep, err := r.Audit(ctx, audit.TreeJournal, "r", time.Now())
	if err != nil || len(rep.Heads) != 1 {
		t.Fatalf("Audit = %d heads, %v; the locked version must still be readable", len(rep.Heads), err)
	}
	if !hasFinding(rep, "delete marker") {
		t.Errorf("Audit findings %v lack the delete marker", rep.Findings)
	}
}

func hasFinding(r awss3.Report, sub string) bool {
	for _, f := range r.Findings {
		if strings.Contains(f.String(), sub) {
			return true
		}
	}
	return false
}

// A delete marker over the newest head must not make an older head look like the latest.
func TestLatestIsNotFooledByADeleteMarker(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	publish(t, f, k, "r", 1, 2, 3)
	newest := f.Keys()[0] // the largest size sorts first
	if err := f.Delete(newest, "", false); err != nil {
		t.Fatal(err)
	}
	if got := len(f.Keys()); got != 2 {
		t.Fatalf("the marker should hide one key from a plain listing, %d remain", got)
	}
	r := newReader(t, f, k.v)
	latest, err := r.Latest(ctx, audit.TreeJournal, "r")
	if err != nil || latest.Size != 3 {
		t.Fatalf("Latest = size %d, %v; want 3 (the head the marker hides)", latest.Size, err)
	}
	if all, err := r.Heads(ctx, audit.TreeJournal, "r"); err != nil || len(all) != 3 {
		t.Fatalf("Heads = %v, %v; want all 3", sizes(all), err)
	}
}
