package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/anchor/gcs"
	"github.com/bide-ai/bide/anchor/gcs/gcstest"
	"github.com/bide-ai/bide/audit"
)

type gcsEnv struct {
	s      audit.Ed25519Signer
	pub    string
	fake   *gcstest.Fake
	anchor *gcs.Anchor
	proj   string
}

func gcsSetup(t *testing.T) *gcsEnv {
	t.Helper()
	h := sha256.Sum256([]byte("k"))
	s := audit.Ed25519Signer{Priv: ed25519.NewKeyFromSeed(h[:])}
	f := gcstest.New()
	a, err := gcs.New(context.Background(), gcs.Config{Store: f, ExpectedProjectNumber: gcstest.ProjectNumber, Retention: 48 * time.Hour,
		KMSKey: gcstest.DefaultKMSKey, OnWarn: func(error) {}, Now: f.Now})
	if err != nil {
		t.Fatal(err)
	}
	old := gcsStore
	gcsStore = func(context.Context, string) (gcs.Store, error) { return f, nil }
	t.Cleanup(func() { gcsStore = old })
	return &gcsEnv{s: s, pub: audit.FormatPublicKey(s.Alg(), s.PublicKey()), fake: f, anchor: a, proj: projectText()}
}

func projectText() string { return strconv.FormatUint(gcstest.ProjectNumber, 10) }

func (e *gcsEnv) head(t *testing.T, run string, size int, seed string) audit.SignedTreeHead {
	t.Helper()
	root := sha256.Sum256([]byte(seed))
	sth, err := audit.SignTreeHead(audit.TreeHead{Kind: audit.TreeJournal, RunID: run, Size: size, Root: root[:], TimestampNanos: int64(size)}, e.s)
	if err != nil {
		t.Fatal(err)
	}
	return sth
}

func (e *gcsEnv) publish(t *testing.T, sths ...audit.SignedTreeHead) {
	t.Helper()
	for _, h := range sths {
		if err := e.anchor.Publish(context.Background(), h.RunID, h); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *gcsEnv) dump(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range e.fake.Names() {
		gens := e.fake.Generations(name)
		_, _, err := e.fake.Read(context.Background(), name, gens[len(gens)-1].Generation, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		body, _, _ := e.fake.Read(context.Background(), name, gens[len(gens)-1].Generation, 1<<20)
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func (e *gcsEnv) do(cmd string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(context.Background(), append([]string{cmd, "-backend", "gcs"}, args...), &out, &errb, nil)
	return code, out.String(), errb.String()
}

func TestGCSBackendOfflineAndOnline(t *testing.T) {
	e := gcsSetup(t)
	e.publish(t, e.head(t, "r1", 1, "a"), e.head(t, "r1", 3, "c"))
	dir := e.dump(t)
	if code, out, errs := e.do("verify", "-dir", dir, "-pubkey", e.pub); code != 0 || !strings.Contains(out, "PASS: 2 head(s)") {
		t.Fatalf("verify = %d\n%s%s", code, out, errs)
	}
	if code, out, errs := e.do("export", "-dir", dir, "-pubkey", e.pub, "-run", "r1"); code != 0 || strings.Count(out, "\n") != 2 {
		t.Fatalf("offline export = %d\n%q%s", code, out, errs)
	}
	online := []string{"-bucket", "b", "-project", e.proj, "-pubkey", e.pub, "-run", "r1"}
	if code, out, errs := e.do("check", online...); code != 0 || !strings.Contains(out, "2 verified head(s)") || !strings.Contains(out, "PASS") {
		t.Fatalf("check = %d\n%s%s", code, out, errs)
	}
	if code, out, errs := e.do("export", online...); code != 0 || strings.Count(out, "\n") != 2 {
		t.Fatalf("online export = %d\n%q%s", code, out, errs)
	}
	// The wrong project, the wrong flag and a project ID in place of the number are refused before any read.
	// A bucket that another project owns is a fact the reader saw: a finding (1), not usage.
	if code, _, errs := e.do("check", "-bucket", "b", "-project", "999", "-pubkey", e.pub, "-run", "r1"); code != exitFindings || !strings.Contains(errs, "owned by project") {
		t.Errorf("a bucket of another project = exit %d %q, want %d", code, errs, exitFindings)
	}
	if code, _, errs := e.do("check", "-bucket", "b", "-owner", "111122223333", "-pubkey", e.pub, "-run", "r1"); code != exitUsage || !strings.Contains(errs, "-owner is for -backend s3") {
		t.Errorf("gcs with -owner = %d %q", code, errs)
	}
	if code, _, errs := e.do("check", "-bucket", "b", "-project", "my-project", "-pubkey", e.pub, "-run", "r1"); code != exitUsage || !strings.Contains(errs, "not a project number") {
		t.Errorf("a project ID = %d %q", code, errs)
	}
}

// An aborted audit is "unknown" (3) for gcs too, a verdict about objects is a finding (1).
func TestGCSBackendAbortedAuditIsUnknown(t *testing.T) {
	e := gcsSetup(t)
	e.publish(t, e.head(t, "r1", 1, "a"))
	online := []string{"-bucket", "b", "-project", e.proj, "-pubkey", e.pub, "-run", "r1"}
	// Reading the bucket's own configuration (at open) that fails is unknown, not usage.
	for _, fault := range []error{errors.New("connection reset"), gcs.ErrUnavailable, gcs.ErrDenied} {
		for _, cmd := range []string{"check", "export"} {
			e.fake.FailNext("Bucket", fault, 1)
			if code, out, errs := e.do(cmd, online...); code != exitUnknown || strings.Contains(out, "PASS") {
				t.Errorf("%s with the bucket unreadable (%v) = %d\n%s%s; want %d", cmd, fault, code, out, errs, exitUnknown)
			}
		}
	}
	for _, m := range []string{"List", "Read"} {
		for _, cmd := range []string{"check", "export"} {
			e.fake.FailNext(m, errors.New("connection reset"), 1) // the first call fails, which aborts the audit
			code, out, errs := e.do(cmd, online...)
			if code != exitUnknown || strings.Contains(out, "PASS") || !strings.Contains(errs, "state of the bucket is unknown") {
				t.Errorf("%s with %s failing = %d\n%s%s; want %d", cmd, m, code, out, errs, exitUnknown)
			}
		}
	}
	// A head the log key did not sign is a finding.
	other := audit.Ed25519Signer{Priv: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))}
	args := append([]string{}, online...)
	args[5] = audit.FormatPublicKey(other.Alg(), other.PublicKey())
	if code, _, _ := e.do("check", args...); code != exitFindings {
		t.Errorf("a head the key did not sign = exit %d, want %d", code, exitFindings)
	}
}

// Two histories signed by the log key is a finding for gcs too, offline and online.
func TestGCSBackendEquivocation(t *testing.T) {
	e := gcsSetup(t)
	e.publish(t, e.head(t, "r1", 2, "honest"), e.head(t, "r1", 2, "rewritten"))
	if code, out, _ := e.do("verify", "-dir", e.dump(t), "-pubkey", e.pub); code != exitFindings || !strings.Contains(out, "two different signed heads") {
		t.Errorf("offline = %d %s", code, out)
	}
	if code, _, _ := e.do("export", "-bucket", "b", "-project", e.proj, "-pubkey", e.pub, "-run", "r1"); code != exitFindings {
		t.Errorf("online export of a fork = exit %d, want %d", code, exitFindings)
	}
}
