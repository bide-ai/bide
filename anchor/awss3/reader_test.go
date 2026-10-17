package awss3_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/anchor/awss3"
	"github.com/bide-ai/bide/anchor/awss3/awss3test"
	"github.com/bide-ai/bide/audit"
)

// publish anchors heads of one run and returns the fake's single key and body per head.
func publish(t *testing.T, f *awss3test.Fake, k keys, run string, sizes ...int) {
	t.Helper()
	a := newAnchor(t, f, nil)
	for _, s := range sizes {
		if err := a.Publish(ctx, run, k.head(t, run, s, int64(s))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReaderRefusesHeadsNotSignedByTheLogKey(t *testing.T) {
	k, other := newKeys(t, 1), newKeys(t, 2)
	f := awss3test.New(bucket)
	publish(t, f, other, "r", 3) // a writer with the bucket but not the log key
	// A well-formed head the log key did not sign is never skipped: it may be a real head under a
	// rotated key, and skipping it would show an older head as the latest.
	publish(t, f, k, "r", 1)
	for name, call := range map[string]func(*awss3.Reader) error{
		"Latest": func(r *awss3.Reader) error { _, err := r.Latest(ctx, audit.TreeJournal, "r"); return err },
		"Heads":  func(r *awss3.Reader) error { _, err := r.Heads(ctx, audit.TreeJournal, "r"); return err },
		"Head":   func(r *awss3.Reader) error { _, err := r.Head(ctx, audit.TreeJournal, "r", 3); return err },
	} {
		rd, skipped := newReaderSkips(t, f, k.v)
		if err := call(rd); !errors.Is(err, awss3.ErrBadSignature) || !errors.Is(err, awss3.ErrObject) || len(*skipped) != 0 {
			t.Errorf("%s = %v with %d skipped, want ErrBadSignature and no skip", name, err, len(*skipped))
		}
	}
}

func TestReaderRefusesAnObjectThatDoesNotMatchItsKey(t *testing.T) {
	k := newKeys(t, 1)
	// Build a valid object, then place it under keys that lie about it.
	src := awss3test.New(bucket)
	publish(t, src, k, "r", 3)
	srcKey := src.Keys()[0]
	body := src.Versions(srcKey)[0].Body
	parts := strings.Split(srcKey, "/")
	partition, name := parts[1], parts[2]

	other := awss3test.New(bucket)
	publish(t, other, k, "r", 7)
	otherName := strings.Split(other.Keys()[0], "/")[2]

	for _, tc := range []struct{ name, key string }{
		{"another size in the name", "bide-anchor/" + partition + "/" + otherName},
		{"another digest", "bide-anchor/" + partition + "/" + name[:len(name)-len(".json")-1] + "0.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := awss3test.New(bucket)
			f.Put(tc.key, body)
			rd, skipped := newReaderSkips(t, f, k.v)
			if _, err := rd.Heads(ctx, audit.TreeJournal, "r"); !errors.Is(err, awss3.ErrNotFound) || len(*skipped) != 1 {
				t.Fatalf("Heads = %v with %d skipped, want ErrNotFound and the object reported as skipped", err, len(*skipped))
			}
			rep, err := rd.Audit(ctx, audit.TreeJournal, "r", time.Now())
			if err != nil || len(rep.Heads) != 0 || len(rep.Findings) == 0 {
				t.Fatalf("Audit = %d heads, findings %v, %v; want the object reported", len(rep.Heads), rep.Findings, err)
			}
		})
	}
}

func TestReaderRefusesNonCanonicalAndMalformedBodies(t *testing.T) {
	k := newKeys(t, 1)
	src := awss3test.New(bucket)
	publish(t, src, k, "r", 3)
	key := src.Keys()[0]
	body := src.Versions(key)[0].Body
	for name, b := range map[string][]byte{
		"extra member":   []byte(strings.Replace(string(body), `{"format"`, `{"extra":1,"format"`, 1)),
		"spaced":         []byte(strings.Replace(string(body), `"sth":`, `"sth": `, 1)),
		"not json":       []byte("hello"),
		"empty":          nil,
		"another format": []byte(strings.Replace(string(body), "bide.audit.s3-anchor.v1", "bide.audit.s3-anchor.v9", 1)),
	} {
		t.Run(name, func(t *testing.T) {
			f := awss3test.New(bucket)
			f.Put(key, b)
			rd, skipped := newReaderSkips(t, f, k.v)
			if _, err := rd.Latest(ctx, audit.TreeJournal, "r"); !errors.Is(err, awss3.ErrNotFound) || len(*skipped) != 1 {
				t.Fatalf("Latest = %v with %d skipped, want ErrNotFound and the object reported as skipped", err, len(*skipped))
			}
		})
	}
}

func TestReaderNeedsAVerifier(t *testing.T) {
	if _, err := awss3.NewReader(awss3.ReaderConfig{Client: awss3test.New(bucket), Bucket: bucket, ExpectedBucketOwner: owner}); err == nil {
		t.Fatal("NewReader accepted no Verifier: it would trust anyone who can write the bucket")
	}
}

func TestAuditReportsEveryWeaknessInHowHeadsAreStored(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	publish(t, f, k, "r", 1, 2, 3)
	rd := newReader(t, f, k.v)
	rep, err := rd.Audit(ctx, audit.TreeJournal, "r", time.Now())
	if err != nil || len(rep.Heads) != 3 || len(rep.Findings) != 0 {
		t.Fatalf("Audit of an intact anchor = %d heads, findings %v, %v", len(rep.Heads), rep.Findings, err)
	}
	if sizes(rep.Heads)[0] != 1 || sizes(rep.Heads)[2] != 3 {
		t.Errorf("heads %v are not in increasing size", sizes(rep.Heads))
	}

	keys := f.Keys()
	// An unlocked object, a second version of a key, and a stray object another tool wrote.
	f.Put(keys[0], f.Versions(keys[0])[0].Body)
	f.DefaultMode = ""
	f.Put(keys[1][:len(keys[1])-len(".json")]+"0.json", []byte("junk"))
	f.Put(keys[2][:strings.LastIndex(keys[2], "/")+1]+"stray.txt", []byte("x"))
	rep, err = rd.Audit(ctx, audit.TreeJournal, "r", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2 versions of one key", "not locked", "not an object name"} {
		if !hasFinding(rep, want) {
			t.Errorf("findings lack %q: %v", want, rep.Findings)
		}
	}
	// Retention that has ended: the object can be deleted now.
	rep, _ = rd.Audit(ctx, audit.TreeJournal, "r", time.Now().Add(400*24*time.Hour))
	if !hasFinding(rep, "retention has ended") {
		t.Errorf("findings lack an ended retention: %v", rep.Findings)
	}
}

func TestAuditFlagsGovernanceLocks(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	f.DefaultMode = "GOVERNANCE"
	a := newAnchor(t, f, func(c *awss3.Config) { c.Mode = "GOVERNANCE"; c.AllowGovernance = true })
	if err := a.Publish(ctx, "r", k.head(t, "r", 1, 1)); err != nil {
		t.Fatal(err)
	}
	rep, err := newReader(t, f, k.v).Audit(ctx, audit.TreeJournal, "r", time.Now())
	if err != nil || !hasFinding(rep, "governance mode") {
		t.Fatalf("Audit = %v, %v; want a governance finding", rep.Findings, err)
	}
	key := f.Keys()[0]
	if err := f.Delete(key, f.Versions(key)[0].ID, false); err == nil {
		t.Error("a governance-locked version was deleted without the bypass")
	}
	if err := f.Delete(key, f.Versions(key)[0].ID, true); err != nil {
		t.Errorf("the bypass should delete a governance version (that is the weakness): %v", err)
	}
}

func TestLoadDirVerifiesADownloadedSet(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	publish(t, f, k, "a", 1, 2)
	publish(t, f, k, "b", 5)
	dir := t.TempDir()
	write := func(key string, b []byte) string {
		p := filepath.Join(dir, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	var paths []string
	for _, key := range f.Keys() {
		paths = append(paths, write(key, f.Versions(key)[0].Body))
	}
	heads, err := awss3.LoadDir(dir, k.v)
	if err != nil || len(heads) != 3 {
		t.Fatalf("LoadDir = %d heads, %v; want 3", len(heads), err)
	}
	if _, err := awss3.LoadDir(dir, newKeys(t, 2).v); !errors.Is(err, awss3.ErrObject) {
		t.Errorf("LoadDir under another key = %v, want ErrObject", err)
	}
	// A renamed file (its name no longer states its head), a stray file, a damaged one.
	if err := os.Rename(paths[0], filepath.Join(filepath.Dir(paths[0]), "renamed.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := awss3.LoadDir(dir, k.v); !errors.Is(err, awss3.ErrObject) {
		t.Errorf("LoadDir with a renamed object = %v, want ErrObject", err)
	}
	// An object in the directory of another partition than its head belongs to.
	wrong := filepath.Join(dir, "bide-anchor", strings.Repeat("0", 64))
	if err := os.MkdirAll(wrong, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(filepath.Dir(paths[0]), "renamed.json"), paths[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(paths[0], filepath.Join(wrong, filepath.Base(paths[0]))); err != nil {
		t.Fatal(err)
	}
	if _, err := awss3.LoadDir(dir, k.v); !errors.Is(err, awss3.ErrObject) {
		t.Errorf("LoadDir with an object in another partition's directory = %v, want ErrObject", err)
	}
	if _, err := awss3.LoadDir(dir, nil); err == nil {
		t.Error("LoadDir without a verifier succeeded")
	}
}

// A key written twice with the same bytes is one head, and Audit still reports the second version.
func TestARewrittenKeyIsOneHead(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	publish(t, f, k, "r", 1, 2)
	key := f.Keys()[0]
	f.Put(key, f.Versions(key)[0].Body)
	r := newReader(t, f, k.v)
	if all, err := r.Heads(ctx, audit.TreeJournal, "r"); err != nil || len(all) != 2 {
		t.Fatalf("Heads = %v, %v; want 2 heads", sizes(all), err)
	}
	if rep, _ := r.Audit(ctx, audit.TreeJournal, "r", time.Now()); !hasFinding(rep, "2 versions of one key") {
		t.Errorf("Audit findings %v lack the rewritten key", rep.Findings)
	}
}

// Two validly signed heads of one size with different roots are the log key signing two histories:
// the library returns an error and never picks one.
func TestEquivocationIsAnErrorEverywhere(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, nil)
	if err := a.Publish(ctx, "r", k.head(t, "r", 1, 1)); err != nil {
		t.Fatal(err)
	}
	fork := k.headRoot(t, "r", 5, 100, "honest")
	if err := a.Publish(ctx, "r", fork); err != nil {
		t.Fatal(err)
	}
	// The same size and the same root with another timestamp (a lease handoff) is fine.
	if err := a.Publish(ctx, "r", k.headRoot(t, "r", 5, 150, "honest")); err != nil {
		t.Fatal(err)
	}
	rd := newReader(t, f, k.v)
	if got, err := rd.Latest(ctx, audit.TreeJournal, "r"); err != nil || got.Size != 5 {
		t.Fatalf("Latest over a handoff pair = %d, %v; want size 5", got.Size, err)
	}
	if err := a.Publish(ctx, "r", k.headRoot(t, "r", 5, 200, "rewritten")); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Latest(ctx, audit.TreeJournal, "r"); !errors.Is(err, awss3.ErrEquivocation) {
		t.Errorf("Latest = %v, want ErrEquivocation", err)
	}
	if _, err := rd.Heads(ctx, audit.TreeJournal, "r"); !errors.Is(err, awss3.ErrEquivocation) {
		t.Errorf("Heads = %v, want ErrEquivocation", err)
	}
	if _, err := rd.Head(ctx, audit.TreeJournal, "r", 5); !errors.Is(err, awss3.ErrEquivocation) {
		t.Errorf("Head = %v, want ErrEquivocation", err)
	}
	if hs, err := rd.Head(ctx, audit.TreeJournal, "r", 1); err != nil || len(hs) != 1 {
		t.Errorf("Head of an unaffected size = %d, %v", len(hs), err)
	}
	rep, err := rd.Audit(ctx, audit.TreeJournal, "r", time.Now())
	if err != nil || !hasFinding(rep, "two different signed heads of size 5") {
		t.Errorf("Audit = %v, %v; want an equivocation finding", rep.Findings, err)
	}
}

// One garbage object that sorts first does not make the partition unreadable: it is skipped and
// reported, Latest returns the newest valid head, and Audit fails on it when it claims a larger size.
func TestAStrayObjectIsSkippedAndReported(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	publish(t, f, k, "r", 2, 3)
	keys := f.Keys()
	dir := keys[0][:strings.LastIndex(keys[0], "/")+1]
	f.Put(dir+"0000000000000000000-"+strings.Repeat("a", 32)+".json", []byte("garbage"))
	rd, skipped := newReaderSkips(t, f, k.v)
	got, err := rd.Latest(ctx, audit.TreeJournal, "r")
	if err != nil || got.Size != 3 {
		t.Fatalf("Latest = size %d, %v; want the newest valid head (3)", got.Size, err)
	}
	if len(*skipped) != 1 || !strings.Contains((*skipped)[0].Key, "0000000000000000000-") {
		t.Errorf("skipped = %v, want the garbage object named", *skipped)
	}
	all, err := rd.Heads(ctx, audit.TreeJournal, "r")
	if err != nil || len(all) != 2 {
		t.Errorf("Heads = %v, %v; want the 2 valid heads", sizes(all), err)
	}
	rep, err := rd.Audit(ctx, audit.TreeJournal, "r", time.Now())
	if err != nil || !hasFinding(rep, "claims size") {
		t.Errorf("Audit = %v, %v; want a finding that a skipped object claims a larger size", rep.Findings, err)
	}
}

// An object another writer stored, with the same bytes (so the same key), under a weaker lock does
// not count as anchored.
func TestPublishRefusesAnExistingObjectUnderAWeakerLock(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	f.DefaultDays = 365 // the bucket default is what the other writer's object got
	a := newAnchor(t, f, func(c *awss3.Config) { c.Retention = 300 * 24 * time.Hour; c.KMSKeyID = ""; c.EncryptionNone = true })
	h := k.head(t, "r", 1, 1)
	key, body := keyAndBody(t, k, h)
	f.DefaultDays = 2
	f.Put(key, body) // a two-day lock under the key Publish will use
	f.DefaultDays = 365
	if err := a.Publish(ctx, "r", h); !errors.Is(err, awss3.ErrWeak) {
		t.Fatalf("Publish over a 2-day object = %v, want ErrWeak", err)
	}
	// The same object with a long enough lock is accepted.
	f2 := awss3test.New(bucket)
	f2.DefaultDays = 365
	a2 := newAnchor(t, f2, func(c *awss3.Config) { c.Retention = 300 * 24 * time.Hour; c.KMSKeyID = ""; c.EncryptionNone = true })
	f2.Put(key, body)
	if err := a2.Publish(ctx, "r", h); err != nil {
		t.Fatalf("Publish over an object retained 365 days = %v", err)
	}
	// An existing object whose write time S3 does not report cannot be checked, and is refused.
	f4 := awss3test.New(bucket)
	f4.DefaultDays = 365
	a4 := newAnchor(t, f4, func(c *awss3.Config) { c.Retention = 300 * 24 * time.Hour; c.KMSKeyID = ""; c.EncryptionNone = true })
	f4.Put(key, body)
	f4.Tamper = func(_ string, v *awss3test.Version) { v.Created = time.Time{} }
	if err := a4.Publish(ctx, "r", h); !errors.Is(err, awss3.ErrWeak) {
		t.Errorf("Publish over an object with no LastModified = %v, want ErrWeak", err)
	}
}
