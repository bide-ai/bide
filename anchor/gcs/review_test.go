package gcs_test

import (
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/anchor/gcs"
	"github.com/bide-ai/bide/anchor/gcs/gcstest"
)

// An auditor's Reader.Audit reports the state of each object, but the bucket's own protections (the
// policy being locked, versioning, uniform access, public access prevention) can change after New: an
// unlocked policy can be removed, versioning suspended. Does the Reader or Audit notice?
func TestAuditReportsABucketThatLostItsProtections(t *testing.T) {
	k := newKeys(t, 1)
	for name, mod := range map[string]func(*gcs.BucketInfo){
		"policy not locked": func(b *gcs.BucketInfo) { b.RetentionLocked = false },
		"no versioning":     func(b *gcs.BucketInfo) { b.Versioning = false },
		"no uniform access": func(b *gcs.BucketInfo) { b.UniformAccess = false },
		"public access ok":  func(b *gcs.BucketInfo) { b.PublicAccessPrevented = false },
		"shorter policy":    func(b *gcs.BucketInfo) { b.RetentionPeriod = 25 * time.Hour },
		"another key":       func(b *gcs.BucketInfo) { b.DefaultKMSKey = "projects/p/locations/l/keyRings/r/cryptoKeys/other" },
	} {
		t.Run(name, func(t *testing.T) {
			f := gcstest.New()
			a := newAnchor(t, f, nil)
			publish(t, a, k.head(t, "r1", 3, 1))
			f.Configure(mod)
			r, err := gcs.NewReader(ctx, gcs.ReaderConfig{Store: f, ExpectedProjectNumber: gcstest.ProjectNumber, Verifier: k.v, KMSKey: kmsKey, Retention: 30 * 24 * time.Hour})
			if err != nil {
				t.Fatalf("NewReader refused a bucket Audit should report on: %v", err)
			}
			rep, err := r.Audit(ctx, "journal", "r1", f.Now())
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Findings) == 0 {
				t.Errorf("%s: the bucket's protection is gone and Audit reports nothing", name)
			}
			if !hasFinding(rep, "bucket") {
				t.Errorf("%s: findings %v do not name the bucket", name, rep.Findings)
			}
		})
	}
}

// Heads promises increasing size, "heads of one size in name order" (the order Head returns them in).
func TestHeadsListsOneSizeInNameOrder(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	for ts := int64(1); ts <= 4; ts++ {
		publish(t, a, k.headRoot(t, "r1", 4, ts, "same"))
	}
	publish(t, a, k.head(t, "r1", 2, 1))
	r := newReader(t, f, k.v)
	hs, err := r.Heads(ctx, "journal", "r1")
	if err != nil {
		t.Fatal(err)
	}
	same, err := r.Head(ctx, "journal", "r1", 4)
	if err != nil {
		t.Fatal(err)
	}
	var got, want []int64
	for _, h := range hs {
		if h.Size == 4 {
			got = append(got, h.TimestampNanos)
		}
	}
	for _, h := range same {
		want = append(want, h.TimestampNanos)
	}
	t.Logf("Heads: %v; Head (name order): %v", got, want)
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Heads lists the heads of one size in the reverse of name order: %v, Head gives %v", got, want)
		}
	}
}

// A bucket that still has every protection is no finding, and a reader without a configured key does not
// judge the key.
func TestAuditOfAnIntactBucketHasNoBucketFinding(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	publish(t, newAnchor(t, f, nil), k.head(t, "r1", 3, 1))
	rep, err := newReader(t, f, k.v).Audit(ctx, "journal", "r1", f.Now())
	if err != nil || len(rep.Findings) != 0 {
		t.Fatalf("Audit = %+v, %v", rep.Findings, err)
	}
	f.Configure(func(b *gcs.BucketInfo) { b.DefaultKMSKey = "projects/p/locations/l/keyRings/r/cryptoKeys/other" })
	if rep, _ := newReader(t, f, k.v).Audit(ctx, "journal", "r1", f.Now()); hasFinding(rep, "default key") {
		t.Errorf("a reader without a configured key judged the key: %v", rep.Findings)
	}
	r := newReader(t, f, k.v)
	f.FailNext("Bucket", gcs.ErrUnavailable, 1)
	if _, err := r.Audit(ctx, "journal", "r1", f.Now()); !errors.Is(err, gcs.ErrUnavailable) {
		t.Errorf("Audit with an unreadable bucket = %v; want ErrUnavailable", err)
	}
}

// A generation that is deleted between the listing and the read (the end of its retention) is a finding
// or a report, not an abort of the whole read.
func TestAGenerationThatVanishesDoesNotAbortTheRead(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	publish(t, a, k.head(t, "r1", 3, 1))
	publish(t, a, k.head(t, "r1", 5, 2))
	names := f.Names()
	f.VanishOnRead(names[0]) // the size 5 head's name sorts first
	r, skipped := newReaderSkips(t, f, k.v)
	got, err := r.Latest(ctx, "journal", "r1")
	if err != nil || got.Size != 3 || len(*skipped) != 1 {
		t.Fatalf("Latest = %v, %v, skipped %v; want the size 3 head and one report", got.Size, err, *skipped)
	}
	rep, err := r.Audit(ctx, "journal", "r1", f.Now())
	if err != nil || !hasFinding(rep, "vanished") {
		t.Fatalf("Audit = %+v, %v; want a vanished finding", rep.Findings, err)
	}
}
