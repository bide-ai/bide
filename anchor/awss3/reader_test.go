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
	if _, err := newReader(t, f, k.v).Latest(ctx, audit.TreeJournal, "r"); !errors.Is(err, awss3.ErrObject) {
		t.Fatalf("Latest = %v, want ErrObject for a head the log key did not sign", err)
	}
	if _, err := newReader(t, f, k.v).Heads(ctx, audit.TreeJournal, "r"); !errors.Is(err, awss3.ErrObject) {
		t.Fatalf("Heads = %v, want ErrObject", err)
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
			rd := newReader(t, f, k.v)
			if _, err := rd.Heads(ctx, audit.TreeJournal, "r"); !errors.Is(err, awss3.ErrObject) {
				t.Fatalf("Heads = %v, want ErrObject", err)
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
			if _, err := newReader(t, f, k.v).Latest(ctx, audit.TreeJournal, "r"); !errors.Is(err, awss3.ErrObject) {
				t.Fatalf("Latest = %v, want ErrObject", err)
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
