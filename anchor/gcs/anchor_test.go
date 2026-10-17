package gcs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/anchor/gcs"
	"github.com/bide-ai/bide/anchor/gcs/gcstest"
	"github.com/bide-ai/bide/audit"
)

func TestNewRefusesWhatIsNotAnAnchorBucket(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    func(*gcs.BucketInfo)
		cfg  func(*gcs.Config)
		want string
	}{
		{name: "another project owns the bucket", f: func(b *gcs.BucketInfo) { b.ProjectNumber = 999 }, want: "owned by project 999"},
		{name: "no retention policy", f: func(b *gcs.BucketInfo) { b.RetentionPeriod = 0 }, want: "no retention policy"},
		{name: "policy not locked", f: func(b *gcs.BucketInfo) { b.RetentionLocked = false }, want: "not locked"},
		{name: "policy below the minimum", f: func(b *gcs.BucketInfo) { b.RetentionPeriod = time.Hour }, want: "below the minimum"},
		{name: "policy shorter than configured", f: func(b *gcs.BucketInfo) { b.RetentionPeriod = 10 * 24 * time.Hour }, want: "shorter than the configured"},
		{name: "no versioning", f: func(b *gcs.BucketInfo) { b.Versioning = false }, want: "versioning"},
		{name: "no uniform access", f: func(b *gcs.BucketInfo) { b.UniformAccess = false }, want: "uniform bucket-level access"},
		{name: "public access not prevented", f: func(b *gcs.BucketInfo) { b.PublicAccessPrevented = false }, want: "public access prevention"},
		{name: "another default key", f: func(b *gcs.BucketInfo) { b.DefaultKMSKey = "projects/p/locations/l/keyRings/r/cryptoKeys/other" }, want: "default key"},
		{name: "no default key", f: func(b *gcs.BucketInfo) { b.DefaultKMSKey = "" }, want: "default key"},
		{name: "object retention without bucket support", cfg: func(c *gcs.Config) { c.ObjectRetention = true }, want: "object retention enabled"},
		{name: "no store", cfg: func(c *gcs.Config) { c.Store = nil }, want: "Store is nil"},
		{name: "no project number", cfg: func(c *gcs.Config) { c.ExpectedProjectNumber = 0 }, want: "ExpectedProjectNumber"},
		{name: "retention too short", cfg: func(c *gcs.Config) { c.Retention = time.Hour }, want: "below the minimum"},
		{name: "no KMS key", cfg: func(c *gcs.Config) { c.KMSKey = "" }, want: "KMSKey is empty"},
		{name: "KMS key with a version", cfg: func(c *gcs.Config) { c.KMSKey = kmsKey + "/cryptoKeyVersions/1" }, want: "no key version"},
		{name: "KMS and none", cfg: func(c *gcs.Config) { c.EncryptionNone = true }, want: "both set"},
		{name: "bad prefix", cfg: func(c *gcs.Config) { c.Prefix = "anchor" }, want: "must end in /"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gcstest.New()
			if tc.f != nil {
				f.Configure(tc.f)
			}
			cfg := baseConfig(f)
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			a, err := gcs.New(ctx, cfg)
			if err == nil || a != nil {
				t.Fatalf("New = %v, %v; want a refusal", a, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if !errors.Is(err, agent.ErrConfig) {
				t.Errorf("error %v does not wrap agent.ErrConfig", err)
			}
			for _, c := range f.Calls() {
				if c == "Create" {
					t.Error("New wrote an object")
				}
			}
		})
	}
}

func TestNewUnreadableBucketIsNotAConfigError(t *testing.T) {
	f := gcstest.New()
	f.FailNext("Bucket", gcs.ErrDenied, 1)
	if _, err := gcs.New(ctx, baseConfig(f)); err == nil || errors.Is(err, gcs.ErrUnavailable) || !errors.Is(err, gcs.ErrDenied) {
		t.Fatalf("New = %v; want a denied error that is not unavailable", err)
	}
	f.FailNext("Bucket", gcs.ErrUnavailable, 1)
	if _, err := gcs.New(ctx, baseConfig(f)); !errors.Is(err, gcs.ErrUnavailable) {
		t.Fatalf("New = %v; want unavailable", err)
	}
}

func TestNewWarnsAboutSoftDelete(t *testing.T) {
	f := gcstest.New()
	f.Configure(func(b *gcs.BucketInfo) { b.SoftDeleteDuration = 7 * 24 * time.Hour })
	var warns []error
	cfg := baseConfig(f)
	cfg.OnWarn = func(e error) { warns = append(warns, e) }
	if _, err := gcs.New(ctx, cfg); err != nil || len(warns) != 1 || !strings.Contains(warns[0].Error(), "soft delete") {
		t.Fatalf("New = %v, warnings %v; want success and one soft delete warning", err, warns)
	}
}

func TestPublishWritesARetainedEncryptedObjectAndReadsItBack(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, func(c *gcs.Config) { c.LegalHold = true })
	sth := k.head(t, "run-1", 3, 100)
	publish(t, a, sth)
	names := f.Names()
	if len(names) != 1 || !strings.HasPrefix(names[0], "bide-anchor/") || strings.Contains(names[0], "run-1") {
		t.Fatalf("names = %v; want one under bide-anchor/ that does not name the run", names)
	}
	g := f.Generations(names[0])
	if len(g) != 1 || !g[0].TemporaryHold || !strings.HasPrefix(g[0].KMSKey, kmsKey+"/cryptoKeyVersions/") ||
		g[0].RetentionExpiry.Sub(g[0].Created) != 30*24*time.Hour {
		t.Fatalf("stored generations = %+v; want a held, CMEK-encrypted, 30-day retained object", g)
	}
	got, err := newReader(t, f, k.v).Latest(ctx, audit.TreeJournal, "run-1")
	if err != nil || got.Hash() != sth.Hash() {
		t.Fatalf("Latest = %v, %v; want the published head", got, err)
	}
}

func TestPublishWithObjectRetentionSetsLockedRetention(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	f.Configure(func(b *gcs.BucketInfo) { b.ObjectRetention = true })
	publish(t, newAnchor(t, f, func(c *gcs.Config) { c.ObjectRetention = true }), k.head(t, "r", 1, 1))
	g := f.Generations(f.Names()[0])[0]
	if g.Retention == nil || g.Retention.Mode != gcs.Locked || g.Retention.RetainUntil.Before(g.Created.Add(30*24*time.Hour)) {
		t.Fatalf("retention = %+v; want Locked for at least 30 days", g.Retention)
	}
}

func TestPublishIsIdempotentAndKeepsDistinctHeadsOfOneSize(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	h := k.head(t, "r", 2, 10)
	publish(t, a, h)
	publish(t, a, h)
	if n := len(f.Generations(f.Names()[0])); n != 1 {
		t.Fatalf("%d generations after publishing one head twice; want 1", n)
	}
	other := k.head(t, "r", 2, 11) // same size and root, another timestamp: another signed head
	publish(t, a, other)
	if len(f.Names()) != 2 {
		t.Fatalf("names = %v; want two objects for two heads of one size", f.Names())
	}
}

func TestPublishAcceptsASmallerHeadAfterALargerOne(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	publish(t, a, k.head(t, "r", 5, 1))
	publish(t, a, k.head(t, "r", 3, 2))
	heads, err := newReader(t, f, k.v).Heads(ctx, audit.TreeJournal, "r")
	if err != nil || len(heads) != 2 || heads[0].Size != 3 || heads[1].Size != 5 {
		t.Fatalf("Heads = %v, %v; want sizes 3 then 5", heads, err)
	}
}

func TestPublishRefusesWhatIsNotAnAnchorableHead(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	if err := a.Publish(ctx, "other", k.head(t, "r", 1, 1)); err == nil {
		t.Error("Publish accepted a head of another run")
	}
	unsigned := k.head(t, "r", 1, 1)
	unsigned.Signature = nil
	if err := a.Publish(ctx, "r", unsigned); err == nil || !strings.Contains(err.Error(), "without a signature") {
		t.Errorf("Publish of an unsigned head = %v", err)
	}
	if len(f.Names()) != 0 {
		t.Errorf("refused heads left objects: %v", f.Names())
	}
}

func TestPublishFailureIsExplicitAndRetryable(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	h := k.head(t, "r", 1, 1)
	f.FailNext("Create", gcs.ErrUnavailable, 1)
	if err := a.Publish(ctx, "r", h); !errors.Is(err, gcs.ErrUnavailable) {
		t.Fatalf("Publish = %v; want ErrUnavailable", err)
	}
	f.FailNext("Create", context.Canceled, 1)
	if err := a.Publish(ctx, "r", h); !errors.Is(err, gcs.ErrUnavailable) {
		t.Fatalf("Publish with a cancelled context = %v; want ErrUnavailable", err)
	}
	f.FailNext("Read", gcs.ErrUnavailable, 1)
	if err := a.Publish(ctx, "r", h); !errors.Is(err, gcs.ErrUnavailable) {
		t.Fatalf("Publish with a failed read-back = %v; want ErrUnavailable", err)
	}
	publish(t, a, h) // the retry finds the object the failed read-back left and checks it
	if n := len(f.Generations(f.Names()[0])); n != 1 {
		t.Fatalf("%d generations after retries; want 1", n)
	}
}

func TestPublishAccessDeniedIsNotUnavailable(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	f.FailNext("Create", gcs.ErrDenied, 1)
	err := a.Publish(ctx, "r", k.head(t, "r", 1, 1))
	if !errors.Is(err, gcs.ErrDenied) || errors.Is(err, gcs.ErrUnavailable) {
		t.Fatalf("Publish = %v; want denied and not unavailable", err)
	}
}

func TestPublishAfterALostResponseFindsItsOwnObject(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	h := k.head(t, "r", 1, 1)
	f.LoseResponseNext("Create", gcs.ErrUnavailable)
	if err := a.Publish(ctx, "r", h); !errors.Is(err, gcs.ErrUnavailable) {
		t.Fatalf("Publish = %v; want ErrUnavailable", err)
	}
	if len(f.Names()) != 1 {
		t.Fatal("the lost response did not leave the object")
	}
	publish(t, a, h)
	if n := len(f.Generations(f.Names()[0])); n != 1 {
		t.Fatalf("%d generations; the retry must not write again", n)
	}
}

// injectHead stores the bytes of h at its name as a foreign writer, with the given attributes.
func injectHead(t *testing.T, k keys, f *gcstest.Fake, h audit.SignedTreeHead, mod func(*gcs.ObjectInfo)) {
	t.Helper()
	name, body := nameAndBody(t, k, h)
	info := gcs.ObjectInfo{Name: name, Created: f.Now(), KMSKey: kmsKey + "/cryptoKeyVersions/1", RetentionExpiry: f.Now().Add(30 * 24 * time.Hour)}
	if mod != nil {
		mod(&info)
	}
	f.Inject(info, body)
}

func TestPublishRefusesAnExistingObjectThatIsWeaker(t *testing.T) {
	k := newKeys(t, 1)
	h := k.head(t, "r", 1, 1)
	for _, tc := range []struct {
		name string
		mod  func(*gcs.ObjectInfo)
		cfg  func(*gcs.Config)
	}{
		{name: "retention shorter than configured", mod: func(i *gcs.ObjectInfo) { i.RetentionExpiry = i.Created.Add(24 * time.Hour) }},
		{name: "no retention covers it", mod: func(i *gcs.ObjectInfo) { i.RetentionExpiry = time.Time{} }},
		{name: "another key", mod: func(i *gcs.ObjectInfo) {
			i.KMSKey = "projects/p/locations/l/keyRings/r/cryptoKeys/other/cryptoKeyVersions/1"
		}},
		{name: "google-managed key", mod: func(i *gcs.ObjectInfo) { i.KMSKey = "" }},
		{name: "a key whose name merely starts like the configured one", mod: func(i *gcs.ObjectInfo) { i.KMSKey = kmsKey + "2/cryptoKeyVersions/1" }},
		{name: "no hold when one is configured", cfg: func(c *gcs.Config) { c.LegalHold = true }},
		{name: "no per-object retention when configured", cfg: func(c *gcs.Config) { c.ObjectRetention = true }},
		{name: "unlocked per-object retention", cfg: func(c *gcs.Config) { c.ObjectRetention = true },
			mod: func(i *gcs.ObjectInfo) {
				i.Retention = &gcs.ObjectRetention{Mode: "Unlocked", RetainUntil: i.Created.Add(60 * 24 * time.Hour)}
			}},
		{name: "per-object retention too short", cfg: func(c *gcs.Config) { c.ObjectRetention = true },
			mod: func(i *gcs.ObjectInfo) {
				i.Retention = &gcs.ObjectRetention{Mode: gcs.Locked, RetainUntil: i.Created.Add(time.Hour)}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gcstest.New()
			f.Configure(func(b *gcs.BucketInfo) { b.ObjectRetention = true })
			injectHead(t, k, f, h, tc.mod)
			err := newAnchor(t, f, tc.cfg).Publish(ctx, "r", h)
			if !errors.Is(err, gcs.ErrWeak) {
				t.Fatalf("Publish = %v; want ErrWeak", err)
			}
		})
	}
	// The same object as a foreign writer would store it correctly is accepted: the checks above are
	// what refuses, not the existence of the object.
	f := gcstest.New()
	injectHead(t, k, f, h, nil)
	publish(t, newAnchor(t, f, nil), h)
}

func TestPublishRefusesAnObjectCloudStorageDescribesInconsistently(t *testing.T) {
	k := newKeys(t, 1)
	h := k.head(t, "r", 1, 1)
	for name, mod := range map[string]func(*gcs.ObjectInfo){
		"checksum":         func(i *gcs.ObjectInfo) { i.CRC32C++ },
		"size":             func(i *gcs.ObjectInfo) { i.Size++ },
		"no generation":    func(i *gcs.ObjectInfo) { i.Generation = 0 },
		"no creation time": func(i *gcs.ObjectInfo) { i.Created = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			f := gcstest.New()
			injectHead(t, k, f, h, nil)
			f.Report(f.Names()[0], 0, mod)
			if err := newAnchor(t, f, nil).Publish(ctx, "r", h); !errors.Is(err, gcs.ErrWeak) {
				t.Fatalf("Publish = %v; want ErrWeak", err)
			}
		})
	}
}

// A fresh write that the bucket stored under other terms than were asked for does not count as anchored.
func TestPublishRefusesAFreshWriteStoredUnderOtherTerms(t *testing.T) {
	k := newKeys(t, 1)
	for name, tc := range map[string]struct {
		cfg func(*gcs.Config)
		mod func(*gcs.ObjectInfo)
	}{
		"per-object retention too short": {func(c *gcs.Config) { c.ObjectRetention = true },
			func(i *gcs.ObjectInfo) { i.Retention.RetainUntil = i.Created.Add(time.Hour) }},
		"per-object retention unlocked": {func(c *gcs.Config) { c.ObjectRetention = true },
			func(i *gcs.ObjectInfo) { i.Retention.Mode = "Unlocked" }},
		"per-object retention dropped": {func(c *gcs.Config) { c.ObjectRetention = true }, func(i *gcs.ObjectInfo) { i.Retention = nil }},
		"hold dropped":                 {func(c *gcs.Config) { c.LegalHold = true }, func(i *gcs.ObjectInfo) { i.TemporaryHold = false }},
		"another key": {nil, func(i *gcs.ObjectInfo) {
			i.KMSKey = "projects/p/locations/l/keyRings/r/cryptoKeys/other/cryptoKeyVersions/1"
		}},
		"retention dropped": {nil, func(i *gcs.ObjectInfo) { i.RetentionExpiry = time.Time{} }},
	} {
		t.Run(name, func(t *testing.T) {
			f := gcstest.New()
			f.Configure(func(b *gcs.BucketInfo) { b.ObjectRetention = true })
			f.AlterNextCreate(tc.mod)
			if err := newAnchor(t, f, tc.cfg).Publish(ctx, "r", k.head(t, "r", 1, 1)); !errors.Is(err, gcs.ErrWeak) {
				t.Fatalf("Publish = %v; want ErrWeak", err)
			}
		})
	}
}

func TestPublishRefusesAFreshWriteThatReadsBackWeaker(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	// The bucket's policy is changed after start (a locked policy cannot be, but the client must not
	// assume what it read at start still holds): the object's own retention is what is checked.
	f.Configure(func(b *gcs.BucketInfo) { b.RetentionPeriod = time.Hour; b.DefaultKMSKey = "" })
	if err := a.Publish(ctx, "r", k.head(t, "r", 1, 1)); !errors.Is(err, gcs.ErrWeak) {
		t.Fatalf("Publish = %v; want ErrWeak", err)
	}
}

func TestPublishRefusesANameHoldingDifferentBytes(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	h := k.head(t, "r", 1, 1)
	publish(t, a, h)
	f.Overwrite(f.Names()[0], []byte(`{"format":"x"}`))
	if err := a.Publish(ctx, "r", h); !errors.Is(err, gcs.ErrConflict) {
		t.Fatalf("Publish over an overwritten object = %v; want ErrConflict", err)
	}
}

func TestSuspectIDsWarnOrAreRefused(t *testing.T) {
	k := newKeys(t, 1)
	for _, id := range []string{"alice@example.com", "acct-123456789012"} {
		f := gcstest.New()
		var warns []error
		a := newAnchor(t, f, func(c *gcs.Config) { c.OnWarn = func(e error) { warns = append(warns, e) } })
		publish(t, a, k.head(t, id, 1, 1))
		if len(warns) != 1 {
			t.Errorf("%q: warnings = %v; want one", id, warns)
		}
		r := newAnchor(t, gcstest.New(), func(c *gcs.Config) { c.RefuseSuspectIDs = true })
		if err := r.Publish(ctx, id, k.head(t, id, 1, 1)); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%q: Publish with RefuseSuspectIDs = %v; want a config error", id, err)
		}
	}
	f := gcstest.New()
	var warns []error
	publish(t, newAnchor(t, f, func(c *gcs.Config) { c.OnWarn = func(e error) { warns = append(warns, e) } }), k.head(t, "run-2026-10-17-0001", 1, 1))
	if len(warns) != 0 {
		t.Errorf("an ordinary run ID warned: %v", warns)
	}
}

// What a retention policy does and does not stop, as the fake models it and the Reader must withstand.
func TestAnchoredHeadSurvivesDeleteAttempts(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	h := k.head(t, "r", 4, 1)
	publish(t, a, h)
	name := f.Names()[0]
	g := f.Generations(name)[0].Generation
	if err := f.DeleteGeneration(name, g); !errors.Is(err, gcstest.ErrRetained) {
		t.Fatalf("DeleteGeneration inside the retention = %v; want ErrRetained", err)
	}
	if err := f.DeleteLive(name); err != nil { // allowed: the generation becomes noncurrent and stays
		t.Fatal(err)
	}
	got, err := newReader(t, f, k.v).Latest(ctx, audit.TreeJournal, "r")
	if err != nil || got.Hash() != h.Hash() {
		t.Fatalf("Latest after the live object was deleted = %v, %v; want the head", got, err)
	}
	publish(t, a, h) // repairs the live name; the retained generation is still there
	if n := len(f.Generations(name)); n != 2 {
		t.Fatalf("%d generations after a republish over a deleted live object; want 2", n)
	}
	f.Advance(31 * 24 * time.Hour)
	if err := f.DeleteGeneration(name, g); err != nil {
		t.Fatalf("DeleteGeneration after the retention = %v; the model must let it go then", err)
	}
}

func TestHeldHeadCannotBeDeletedAfterRetention(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	publish(t, newAnchor(t, f, func(c *gcs.Config) { c.LegalHold = true }), k.head(t, "r", 1, 1))
	name := f.Names()[0]
	g := f.Generations(name)[0].Generation
	f.Advance(365 * 24 * time.Hour)
	if err := f.DeleteGeneration(name, g); !errors.Is(err, gcstest.ErrRetained) {
		t.Fatalf("DeleteGeneration of a held object = %v; want ErrRetained", err)
	}
	f.ReleaseHold(name, g)
	if err := f.DeleteGeneration(name, g); err != nil {
		t.Fatal(err)
	}
}
