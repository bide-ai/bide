package gcs_test

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/bide-ai/bide/anchor/gcs"
	"github.com/bide-ai/bide/audit"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

// TestRealGCS runs the anchor against a real bucket. It is skipped unless BIDE_GCS_BUCKET is set, and
// it is never run in CI (no credentials). Read docs/testing/gcs-anchor-real-run.md before running
// it: the bucket needs a LOCKED retention policy, which cannot be undone, and every object this test
// writes stays until its retention ends.
//
//	BIDE_GCS_BUCKET            the scratch bucket
//	BIDE_GCS_PROJECT_NUMBER    the numeric project number that owns it
//	BIDE_GCS_KMS_KEY           projects/P/locations/L/keyRings/R/cryptoKeys/K (also the bucket's default key)
//	BIDE_GCS_RETENTION_HOURS   the retention to require (default 24; the bucket's policy must be at least this)
//	BIDE_GCS_OBJECT_RETENTION  1 to also use per-object Locked retention (the bucket needs object retention enabled;
//	                           the writer then needs gcs.WriterObjectRetentionRole, not gcs.WriterRole)
//	BIDE_GCS_LEGAL_HOLD        1 to also place temporary holds (a hold stays until a separate identity releases it)
//	BIDE_GCS_WRITER_SA         a service account holding only the writer role: the Anchor runs as it (by impersonation)
//	BIDE_GCS_READER_SA         a service account holding only the reader role: the Reader runs as it
//
// Credentials are Application Default Credentials. The ADC identity is the operator: it is used for the
// bucket probes (and, without BIDE_GCS_WRITER_SA, for the Anchor), and needs storage.objects.delete
// for the final probe, which is expected to be REFUSED: a deletion that succeeds there fails the test.
// With the two service accounts set, the Anchor and the Reader run with exactly the roles shipped in
// iam/, which settles that those roles are enough (and that no Cloud KMS role is needed).
func TestRealGCS(t *testing.T) {
	bucket := os.Getenv("BIDE_GCS_BUCKET")
	if bucket == "" {
		t.Skip("BIDE_GCS_BUCKET is not set")
	}
	project, err := strconv.ParseUint(os.Getenv("BIDE_GCS_PROJECT_NUMBER"), 10, 64)
	if err != nil {
		t.Fatalf("BIDE_GCS_PROJECT_NUMBER: %v", err)
	}
	hours := 24
	if v := os.Getenv("BIDE_GCS_RETENTION_HOURS"); v != "" {
		if hours, err = strconv.Atoi(v); err != nil {
			t.Fatalf("BIDE_GCS_RETENTION_HOURS: %v", err)
		}
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	prefix := "bide-anchor-it/" + hex.EncodeToString(suffix[:]) + "/"
	store := gcs.NewStore(client, bucket)
	asStore := func(sa string) gcs.Store {
		if sa == "" {
			return store
		}
		ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{TargetPrincipal: sa,
			Scopes: []string{"https://www.googleapis.com/auth/devstorage.read_write"}})
		if err != nil {
			t.Fatal(err)
		}
		c, err := storage.NewClient(ctx, option.WithTokenSource(ts))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return gcs.NewStore(c, bucket)
	}
	cfg := gcs.Config{
		Store: asStore(os.Getenv("BIDE_GCS_WRITER_SA")), ExpectedProjectNumber: project, Prefix: prefix, Retention: time.Duration(hours) * time.Hour,
		KMSKey: os.Getenv("BIDE_GCS_KMS_KEY"), ObjectRetention: os.Getenv("BIDE_GCS_OBJECT_RETENTION") == "1",
		LegalHold: os.Getenv("BIDE_GCS_LEGAL_HOLD") == "1", OnWarn: func(err error) { t.Logf("warning: %v", err) },
	}
	a, err := gcs.New(ctx, cfg)
	if err != nil {
		t.Fatalf("New refused the bucket: %v", err)
	}
	info, _ := store.Bucket(ctx)
	t.Logf("bucket: retention %s locked=%v versioning=%v default key=%q soft delete=%s object retention=%v",
		info.RetentionPeriod, info.RetentionLocked, info.Versioning, info.DefaultKMSKey, info.SoftDeleteDuration, info.ObjectRetention)

	k := newKeys(t, 7)
	run := "it-" + hex.EncodeToString(suffix[:])
	h1, h2 := k.head(t, run, 1, 1), k.head(t, run, 2, 2)
	publish(t, a, h1)
	publish(t, a, h1) // the 412 path against the real server
	publish(t, a, h2)

	r, err := gcs.NewReader(ctx, gcs.ReaderConfig{Store: asStore(os.Getenv("BIDE_GCS_READER_SA")), ExpectedProjectNumber: project, Prefix: prefix, KMSKey: cfg.KMSKey, Verifier: k.v})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Latest(ctx, audit.TreeJournal, run)
	if err != nil || got.Hash() != h2.Hash() {
		t.Fatalf("Latest = %v, %v; want the size 2 head", got, err)
	}
	rep, err := r.Audit(ctx, audit.TreeJournal, run, time.Now())
	if err != nil || len(rep.Heads) != 2 || len(rep.Findings) != 0 {
		t.Fatalf("Audit = %+v, %v; want two heads and no findings", rep, err)
	}

	// What the object looks like: the unverified details of the runbook, logged for the record.
	var first gcs.ObjectInfo
	_ = store.List(ctx, prefix, true, func(o gcs.ObjectInfo) bool { first = o; return false })
	t.Logf("object: generation %d, kms key %q, retention expiry %s, per-object retention %+v, temporary hold %v",
		first.Generation, first.KMSKey, first.RetentionExpiry.Format(time.RFC3339), first.Retention, first.TemporaryHold)

	// Deleting a generation inside the retention must be refused by Cloud Storage.
	obj := client.Bucket(bucket).Object(first.Name).Generation(first.Generation)
	err = obj.Delete(ctx)
	var ge *googleapi.Error
	switch {
	case err == nil:
		t.Fatalf("DELETING AN ANCHORED GENERATION SUCCEEDED: the bucket does not hold heads as the anchor requires")
	case errors.As(err, &ge):
		t.Logf("delete of a retained generation refused: HTTP %d", ge.Code)
	default:
		t.Fatalf("delete probe failed in an unexpected way: %v", err)
	}

	// Deleting the live object (no generation) makes it noncurrent; the Reader must still return the head.
	if err := client.Bucket(bucket).Object(first.Name).Delete(ctx); err != nil {
		t.Logf("delete of the live object: %v (it succeeds as a noncurrent transition unless a hold is on the object)", err)
	}
	if got, err := r.Latest(ctx, audit.TreeJournal, run); err != nil || got.Hash() != h2.Hash() {
		t.Fatalf("Latest after the live object was deleted = %v, %v", got, err)
	}
	rep, _ = r.Audit(ctx, audit.TreeJournal, run, time.Now())
	t.Logf("audit after the delete: %d heads, findings: %v", len(rep.Heads), rep.Findings)
	publish(t, a, h1) // repairs a live object over the noncurrent generation
}
