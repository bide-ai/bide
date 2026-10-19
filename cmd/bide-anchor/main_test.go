package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bide-ai/bide/anchor/awss3"
	"github.com/bide-ai/bide/anchor/awss3/awss3test"
	"github.com/bide-ai/bide/anchor/gcs"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/anchorobj"
)

const owner = "111122223333"

type env struct {
	s      audit.Ed25519Signer
	pub    string
	fake   *awss3test.Fake
	anchor *awss3.Anchor
}

func setup(t *testing.T) *env {
	t.Helper()
	h := sha256.Sum256([]byte("k"))
	s := audit.Ed25519Signer{Priv: ed25519.NewKeyFromSeed(h[:])}
	f := awss3test.New("b")
	a, err := awss3.New(context.Background(), awss3.Config{Client: f, Bucket: "b", ExpectedBucketOwner: owner, Retention: 48 * time.Hour, KMSKeyID: "arn:aws:kms:eu-west-1:111122223333:key/k", OnWarn: func(error) {}})
	if err != nil {
		t.Fatal(err)
	}
	return &env{s: s, pub: audit.FormatPublicKey(s.Alg(), s.PublicKey()), fake: f, anchor: a}
}

func (e *env) head(t *testing.T, run string, size int, rootSeed string) audit.SignedTreeHead {
	t.Helper()
	root := sha256.Sum256([]byte(rootSeed))
	sth, err := audit.SignTreeHead(audit.TreeHead{Kind: audit.TreeJournal, RunID: run, Size: size, Root: root[:], TimestampNanos: int64(size)}, e.s)
	if err != nil {
		t.Fatal(err)
	}
	return sth
}

func (e *env) publish(t *testing.T, sths ...audit.SignedTreeHead) {
	t.Helper()
	for _, h := range sths {
		if err := e.anchor.Publish(context.Background(), h.RunID, h); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) dump(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, key := range e.fake.Keys() {
		p := filepath.Join(dir, filepath.FromSlash(key))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, e.fake.Versions(key)[0].Body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func (e *env) do(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb, func(context.Context, bool) (awss3.Client, error) { return e.fake, nil })
	return code, out.String(), errb.String()
}

func TestVerifyOfflineAndExport(t *testing.T) {
	e := setup(t)
	e.publish(t, e.head(t, "r1", 1, "a"), e.head(t, "r1", 3, "c"), e.head(t, "r2", 2, "x"))
	dir := e.dump(t)

	code, out, errs := e.do("verify", "-dir", dir, "-pubkey", e.pub)
	if code != 0 || !strings.Contains(out, "PASS: 3 head(s)") || !strings.Contains(out, `journal "r1": 2 head(s), sizes 1 to 3`) {
		t.Fatalf("verify = %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "cannot show that S3 locked them") {
		t.Errorf("the offline verdict does not say what it cannot show:\n%s", out)
	}
	if code, out, _ := e.do("verify", "-dir", dir, "-pubkey", e.pub, "-run", "nope"); code != 1 || !strings.Contains(out, "FAIL") {
		t.Errorf("verify of an absent run = %d %s", code, out)
	}

	// export: one verified head per line, increasing size, in the form bide audit -heads reads.
	code, out, errs = e.do("export", "-dir", dir, "-pubkey", e.pub, "-run", "r1")
	if code != 0 {
		t.Fatalf("export = %d %s", code, errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("export wrote %d lines: %s", len(lines), out)
	}
	for i, size := range []int{1, 3} {
		var h audit.SignedTreeHead
		if err := audit.UnmarshalStrict([]byte(lines[i]), &h); err != nil || h.Size != size {
			t.Errorf("line %d = size %d, %v; want size %d", i, h.Size, err, size)
		}
	}
	// A key file works as -pubkey; a wrong key fails verification.
	keyfile := filepath.Join(t.TempDir(), "log.pub")
	os.WriteFile(keyfile, []byte(e.pub+"\n"), 0o644)
	if code, _, errs := e.do("export", "-dir", dir, "-pubkey", keyfile, "-run", "r1"); code != 0 {
		t.Errorf("export with a key file = %d %s", code, errs)
	}
	other := audit.Ed25519Signer{Priv: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))}
	if code, out, _ := e.do("verify", "-dir", dir, "-pubkey", audit.FormatPublicKey(other.Alg(), other.PublicKey())); code != 1 || !strings.Contains(out, "FAIL") {
		t.Errorf("verify under another key = %d %s", code, out)
	}
	// -o writes a file.
	of := filepath.Join(t.TempDir(), "heads.jsonl")
	if code, _, _ := e.do("export", "-dir", dir, "-pubkey", e.pub, "-run", "r1", "-o", of); code != 0 {
		t.Fatal("export -o failed")
	}
	if b, _ := os.ReadFile(of); strings.Count(string(b), "\n") != 2 {
		t.Errorf("heads file = %q", b)
	}
}

func TestVerifyReportsASignedHistoryFork(t *testing.T) {
	e := setup(t)
	e.publish(t, e.head(t, "r1", 2, "honest"), e.head(t, "r1", 2, "rewritten"))
	code, out, _ := e.do("verify", "-dir", e.dump(t), "-pubkey", e.pub)
	if code != 1 || !strings.Contains(out, "two different signed heads of size 2") {
		t.Fatalf("verify = %d\n%s", code, out)
	}
	if code, _, errs := e.do("export", "-dir", e.dump(t), "-pubkey", e.pub, "-run", "r1"); code != 1 || !strings.Contains(errs, "not exporting") {
		t.Errorf("export of a fork = %d %s", code, errs)
	}
}

func TestVerifyFailsOnDamagedOrStrayFiles(t *testing.T) {
	e := setup(t)
	e.publish(t, e.head(t, "r1", 1, "a"))
	dir := e.dump(t)
	filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			os.WriteFile(p, append([]byte(" "), mustRead(p)...), 0o644)
		}
		return nil
	})
	if code, out, _ := e.do("verify", "-dir", dir, "-pubkey", e.pub); code != 1 || !strings.Contains(out, "FAIL") {
		t.Fatalf("verify of a damaged object = %d %s", code, out)
	}
}

func mustRead(p string) []byte { b, _ := os.ReadFile(p); return b }

func TestCheckOnline(t *testing.T) {
	e := setup(t)
	e.publish(t, e.head(t, "r1", 1, "a"), e.head(t, "r1", 2, "b"))
	args := []string{"check", "-bucket", "b", "-owner", owner, "-pubkey", e.pub, "-run", "r1"}
	code, out, errs := e.do(args...)
	if code != 0 || !strings.Contains(out, "2 verified head(s)") || !strings.Contains(out, "PASS") {
		t.Fatalf("check of an intact anchor = %d\n%s%s", code, out, errs)
	}
	if code, out, errs := e.do("export", "-bucket", "b", "-owner", owner, "-pubkey", e.pub, "-run", "r1"); code != 0 || strings.Count(out, "\n") != 2 {
		t.Fatalf("online export of an intact anchor = %d %q %q", code, out, errs)
	}
	// An unlocked rewrite of a key and a delete marker are findings.
	key := e.fake.Keys()[0]
	e.fake.DefaultMode = ""
	e.fake.Put(key, e.fake.Versions(key)[0].Body)
	e.fake.Delete(e.fake.Keys()[1], "", false)
	code, out, _ = e.do(args...)
	if code != 1 || !strings.Contains(out, "FINDING") || !strings.Contains(out, "FAIL") {
		t.Fatalf("check with findings = %d\n%s", code, out)
	}
	if code, out, _ := e.do("check", "-bucket", "b", "-owner", owner, "-pubkey", e.pub, "-run", "absent"); code != 1 || !strings.Contains(out, "FAIL: no verified head") {
		t.Errorf("check of an absent run = %d %s", code, out)
	}
	exp := []string{"export", "-bucket", "b", "-owner", owner, "-pubkey", e.pub, "-run", "r1"}
	if code, out, errs := e.do(exp...); code != 1 || out != "" || !strings.Contains(errs, "not exporting") {
		t.Errorf("online export with findings = %d %q %q; want a refusal that exports nothing", code, out, errs)
	}
	if code, out, errs := e.do(append(exp, "-force")...); code != 0 || strings.Count(out, "\n") < 2 || !strings.Contains(errs, "not proof the set is complete") || !strings.Contains(errs, "sizes 1 to 2") {
		t.Errorf("online export -force = %d %q %q; want the heads, the largest size and the completeness note", code, out, errs)
	}
}

func TestUsageErrors(t *testing.T) {
	e := setup(t)
	for _, args := range [][]string{
		nil, {"nope"}, {"verify"}, {"verify", "-dir", "d"}, {"verify", "-dir", "d", "-pubkey", "garbage"},
		{"verify", "-bucket", "b", "-pubkey", e.pub}, {"check", "-dir", "d", "-pubkey", e.pub, "-run", "r"},
		{"export", "-pubkey", e.pub, "-run", "r"}, {"export", "-dir", "d", "-bucket", "b", "-pubkey", e.pub, "-run", "r"},
		{"export", "-dir", "d", "-pubkey", e.pub}, {"verify", "-dir", "d", "-pubkey", e.pub, "-kind", "other"},
		{"verify", "-dir", "d", "-pubkey", e.pub, "extra"}, {"check", "-bucket", "b", "-owner", "12", "-pubkey", e.pub, "-run", "r"},
	} {
		if code, _, _ := e.do(args...); code != exitUsage {
			t.Errorf("%v = exit %d, want %d", args, code, exitUsage)
		}
	}
}

// An empty directory has nothing to verify: neither command passes on it.
func TestEmptyDirectoryFailsBothCommands(t *testing.T) {
	e := setup(t)
	dir := t.TempDir()
	if code, out, _ := e.do("verify", "-dir", dir, "-pubkey", e.pub); code != 1 || !strings.Contains(out, "nothing to verify") {
		t.Errorf("verify of an empty directory = %d %s; want exit 1 naming nothing to verify", code, out)
	}
	if code, _, errs := e.do("export", "-dir", dir, "-pubkey", e.pub, "-run", "r1"); code != 1 || !strings.Contains(errs, "nothing to verify") {
		t.Errorf("export of an empty directory = %d %s; want exit 1 naming nothing to verify", code, errs)
	}
}

// -dir is a directory: a single object file or a missing path is a usage error (exit 2), not a pass.
func TestDirMustBeADirectory(t *testing.T) {
	e := setup(t)
	e.publish(t, e.head(t, "r1", 1, "a"))
	dir := e.dump(t)
	var file string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() && file == "" {
			file = p
		}
		return nil
	})
	for _, cmd := range []string{"verify", "export"} {
		args := func(d string) []string {
			a := []string{cmd, "-dir", d, "-pubkey", e.pub}
			if cmd == "export" {
				a = append(a, "-run", "r1")
			}
			return a
		}
		if code, _, errs := e.do(args(file)...); code != 2 || !strings.Contains(errs, "not a directory") {
			t.Errorf("%s -dir FILE = %d %s; want exit 2 naming a non-directory", cmd, code, errs)
		}
		if code, _, _ := e.do(args(filepath.Join(dir, "missing"))...); code != 2 {
			t.Errorf("%s -dir MISSING = %d; want exit 2", cmd, code)
		}
		if code, _, errs := e.do(args(dir)...); code != 0 {
			t.Errorf("%s -dir DIR = %d %s; want 0", cmd, code, errs)
		}
	}
}

func TestBackendFlag(t *testing.T) {
	e := setup(t)
	e.publish(t, e.head(t, "r1", 1, "a"))
	dir := e.dump(t)
	if code, out, errs := e.do("verify", "-backend", "s3", "-dir", dir, "-pubkey", e.pub); code != 0 {
		t.Fatalf("-backend s3 = %d\n%s%s", code, out, errs)
	}
	code, _, errs := e.do("check", "-backend", "azure", "-bucket", "b", "-owner", owner, "-pubkey", e.pub, "-run", "r1")
	if code != exitUsage || !strings.Contains(errs, `"azure" is not one of: s3, gcs`) {
		t.Errorf("-backend azure = %d %q; want a usage error naming the backends", code, errs)
	}
	// Each backend takes its own owner flag and refuses the other's.
	code, _, errs = e.do("check", "-backend", "s3", "-bucket", "b", "-project", "123", "-pubkey", e.pub, "-run", "r1")
	if code != exitUsage || !strings.Contains(errs, "-project is for -backend gcs") {
		t.Errorf("s3 with -project = %d %q", code, errs)
	}
}

// The S3 layout is valid, and it is the one the backend reads. When another backend is linked (the GCS
// one), this is where its Layout is checked not to collide with this one: only a program that links
// both can import both without dragging both SDKs into each module.
func TestBackendLayoutsDoNotCollide(t *testing.T) {
	l := awss3.Layout()
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	layouts := map[string]anchorobj.Layout{"s3": l, "gcs": gcs.Layout()}
	for n, ly := range layouts {
		if err := ly.Validate(); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	for a, la := range layouts {
		for b, lb := range layouts {
			if a != b && la.Collides(lb) {
				t.Errorf("the %s and %s layouts collide: one could take the other's objects", a, b)
			}
		}
	}
}

// An online read that cannot finish is "unknown" (3), never a pass (0) and not a finding (1).
func TestAbortedAuditIsUnknown(t *testing.T) {
	e := setup(t)
	e.publish(t, e.head(t, "r1", 1, "a"))
	for name, fault := range map[string]error{"unavailable": errors.New("connection reset"), "denied": awss3test.APIError("AccessDenied", 403)} {
		e.fake.Hook = func(op string, _ int) error {
			if op == "ListObjectVersions" {
				return fault
			}
			return nil
		}
		for _, args := range [][]string{
			{"check", "-bucket", "b", "-owner", owner, "-pubkey", e.pub, "-run", "r1"},
			{"export", "-bucket", "b", "-owner", owner, "-pubkey", e.pub, "-run", "r1"},
		} {
			code, out, errs := e.do(args...)
			if code != exitUnknown || !strings.Contains(errs, "state of the bucket is unknown") || strings.Contains(out, "PASS") {
				t.Errorf("%s %v = %d\n%s%s; want %d and no PASS", name, args[0], code, out, errs, exitUnknown)
			}
		}
	}
	e.fake.Hook = nil
	// A verdict about the objects is still a finding: a head signed by another key.
	other := audit.Ed25519Signer{Priv: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))}
	if code, _, _ := e.do("check", "-bucket", "b", "-owner", owner, "-pubkey", audit.FormatPublicKey(other.Alg(), other.PublicKey()), "-run", "r1"); code != exitFindings {
		t.Errorf("a head the key did not sign = exit %d, want %d", code, exitFindings)
	}
}

// A read that never answers is cut off by -timeout and exits 3, not hangs.
func TestTimeoutExitsUnknown(t *testing.T) {
	e := setup(t)
	e.publish(t, e.head(t, "r1", 1, "a"))
	var out, errb bytes.Buffer
	blocked := func(context.Context, bool) (awss3.Client, error) { return blockedClient{e.fake}, nil }
	start := time.Now()
	code := run(context.Background(), []string{"check", "-timeout", "200ms", "-bucket", "b", "-owner", owner, "-pubkey", e.pub, "-run", "r1"}, &out, &errb, blocked)
	if code != exitUnknown || strings.Contains(out.String(), "PASS") || time.Since(start) > 5*time.Second {
		t.Fatalf("exit %d after %v\n%s%s; want %d soon, no PASS", code, time.Since(start), out.String(), errb.String(), exitUnknown)
	}
	if code, _, _ := e.do("check", "-timeout", "-1s", "-bucket", "b", "-owner", owner, "-pubkey", e.pub, "-run", "r1"); code != exitUsage {
		t.Errorf("a negative -timeout = exit %d, want usage", code)
	}
}

// blockedClient answers a listing only when its context ends.
type blockedClient struct{ *awss3test.Fake }

func (blockedClient) ListObjectVersions(ctx context.Context, _ *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// An unreadable directory is an I/O failure (2), not a verdict about objects (1).
func TestUnreadableDirIsNotAFinding(t *testing.T) {
	e := setup(t)
	missing := filepath.Join(t.TempDir(), "nope")
	for _, cmd := range [][]string{{"verify", "-dir", missing}, {"export", "-dir", missing, "-run", "r1"}} {
		code, out, errs := e.do(append(cmd, "-pubkey", e.pub)...)
		if code != exitUsage || strings.Contains(out, "FAIL") || !strings.Contains(errs, "-dir") {
			t.Errorf("%v = %d\n%s%s; want %d and a message naming -dir", cmd, code, out, errs, exitUsage)
		}
	}
	// An object that does not verify is still a finding.
	e.publish(t, e.head(t, "r1", 1, "a"))
	dir := e.dump(t)
	filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			os.WriteFile(p, []byte("junk"), 0o644)
		}
		return nil
	})
	if code, _, _ := e.do("verify", "-dir", dir, "-pubkey", e.pub); code != exitFindings {
		t.Errorf("a damaged object = exit %d, want %d", code, exitFindings)
	}
}

const s3Key = "arn:aws:kms:eu-west-1:111122223333:key/k"

// For -backend s3, -kms-key, -retention and -lock-mode turn the bucket's own configuration into expectations:
// a swapped default key, a shorter default retention or a weaker lock mode is a finding (1), a malformed
// value is usage (2), and a bucket-configuration read that is denied or unavailable is unknown (3).
func TestS3BackendBucketExpectations(t *testing.T) {
	e := setup(t)
	e.fake.DefaultKMSKeyID = s3Key
	e.publish(t, e.head(t, "r1", 1, "a"))
	base := []string{"-bucket", "b", "-owner", owner, "-pubkey", e.pub, "-run", "r1"}
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }

	if code, out, errs := e.do(append([]string{"check"}, with("-kms-key", s3Key, "-retention", "48h", "-lock-mode", "COMPLIANCE")...)...); code != 0 {
		t.Fatalf("every expectation met = %d\n%s%s", code, out, errs)
	}
	for name, extra := range map[string][]string{
		"another key":      {"-kms-key", "arn:aws:kms:eu-west-1:111122223333:key/other"},
		"longer retention": {"-retention", "100000h"},
	} {
		for _, cmd := range []string{"check", "export"} {
			if code, out, errs := e.do(append([]string{cmd}, with(extra...)...)...); code != exitFindings {
				t.Errorf("%s %s = %d\n%s%s; want %d", name, cmd, code, out, errs, exitFindings)
			}
		}
	}
	// An alias cannot be checked against the key ARN S3 reports: refused (2), naming the fix.
	for _, alias := range []string{"alias/bide-anchor", "arn:aws:kms:eu-west-1:111122223333:alias/bide-anchor"} {
		if code, _, errs := e.do(append([]string{"check"}, with("-kms-key", alias)...)...); code != exitUsage || !strings.Contains(errs, "pass the ARN of the key the alias points to") {
			t.Errorf("-kms-key %s = exit %d (%s), want %d naming the fix", alias, code, errs, exitUsage)
		}
	}
	e.fake.DefaultMode = types.ObjectLockRetentionModeGovernance
	if code, out, _ := e.do(append([]string{"check"}, with("-lock-mode", "COMPLIANCE")...)...); code != exitFindings || !strings.Contains(out, "weaker than the expected COMPLIANCE") {
		t.Errorf("a governance default where compliance is expected = %d\n%s", code, out)
	}
	e.fake.DefaultMode = types.ObjectLockRetentionModeCompliance

	for name, extra := range map[string][]string{
		"bad lock mode":  {"-lock-mode", "WORM"},
		"zero retention": {"-retention", "0s"},
		"negative":       {"-retention", "-1h"},
	} {
		if code, _, errs := e.do(append([]string{"check"}, with(extra...)...)...); code != exitUsage {
			t.Errorf("%s = exit %d (%s), want %d", name, code, errs, exitUsage)
		}
	}
	// Reading the bucket's configuration denied or unavailable: unknown, never a pass.
	for _, op := range []string{"GetBucketVersioning", "GetObjectLockConfiguration", "GetBucketEncryption"} {
		for name, fault := range map[string]error{"denied": awss3test.APIError("AccessDenied", 403), "unavailable": errors.New("connection reset")} {
			e.fake.Hook = func(o string, _ int) error {
				if o == op {
					return fault
				}
				return nil
			}
			for _, cmd := range []string{"check", "export"} {
				code, out, errs := e.do(append([]string{cmd}, with("-kms-key", s3Key)...)...)
				if code != exitUnknown || strings.Contains(out, "PASS") {
					t.Errorf("%s with %s %s = %d\n%s%s; want %d", cmd, op, name, code, out, errs, exitUnknown)
				}
			}
		}
	}
	e.fake.Hook = nil
}
