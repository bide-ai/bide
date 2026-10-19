package gcs_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/anchor/gcs"
	"github.com/bide-ai/bide/anchor/gcs/gcstest"
	"github.com/bide-ai/bide/audit"
)

func TestNewReaderRefusesWhatItCannotTrust(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	for name, cfg := range map[string]gcs.ReaderConfig{
		"no store":      {ExpectedProjectNumber: 1, Verifier: k.v},
		"no project":    {Store: f, Verifier: k.v},
		"no verifier":   {Store: f, ExpectedProjectNumber: gcstest.ProjectNumber},
		"other project": {Store: f, ExpectedProjectNumber: 42, Verifier: k.v},
		"bad prefix":    {Store: f, ExpectedProjectNumber: gcstest.ProjectNumber, Verifier: k.v, Prefix: "x"},
		"bad key":       {Store: f, ExpectedProjectNumber: gcstest.ProjectNumber, Verifier: k.v, KMSKey: "key"},
	} {
		if r, err := gcs.NewReader(ctx, cfg); err == nil || r != nil || !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: NewReader = %v, %v; want a config error", name, r, err)
		}
	}
}

func TestLatestReturnsTheLargestHeadAndReportsNothingForAnEmptyPartition(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	r := newReader(t, f, k.v)
	if _, err := r.Latest(ctx, audit.TreeJournal, "r"); !errors.Is(err, gcs.ErrNotFound) {
		t.Fatalf("Latest of nothing = %v; want ErrNotFound", err)
	}
	for _, n := range []int{2, 9, 5} {
		publish(t, a, k.head(t, "r", n, int64(n)))
	}
	publish(t, a, k.head(t, "other", 50, 1))
	got, err := r.Latest(ctx, audit.TreeJournal, "r")
	if err != nil || got.Size != 9 || got.RunID != "r" {
		t.Fatalf("Latest = %+v, %v; want size 9 of r", got, err)
	}
	hs, err := r.Head(ctx, audit.TreeJournal, "r", 5)
	if err != nil || len(hs) != 1 || hs[0].Size != 5 {
		t.Fatalf("Head(5) = %v, %v", hs, err)
	}
	if _, err := r.Head(ctx, audit.TreeJournal, "r", 6); !errors.Is(err, gcs.ErrNotFound) {
		t.Fatalf("Head(6) = %v; want ErrNotFound", err)
	}
	if _, err := r.Head(ctx, audit.TreeJournal, "r", -1); err == nil {
		t.Fatal("Head(-1) succeeded")
	}
}

// A deleted or overwritten newest head must not make an older one look like the latest.
func TestLatestIsNotFooledByDeletionOrOverwrite(t *testing.T) {
	k := newKeys(t, 1)
	for name, attack := range map[string]func(*gcstest.Fake, string){
		"delete the live object": func(f *gcstest.Fake, n string) { _ = f.DeleteLive(n) },
		"overwrite with junk":    func(f *gcstest.Fake, n string) { f.Overwrite(n, []byte("junk")) },
		"overwrite with an older head's bytes": func(f *gcstest.Fake, n string) {
			_, body := nameAndBody(t, k, k.head(t, "r", 1, 1))
			f.Overwrite(n, body)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := gcstest.New()
			a := newAnchor(t, f, nil)
			publish(t, a, k.head(t, "r", 1, 1))
			newest := k.head(t, "r", 8, 2)
			publish(t, a, newest)
			attack(f, f.Names()[0]) // the names sort the largest size first
			r, skipped := newReaderSkips(t, f, k.v)
			got, err := r.Latest(ctx, audit.TreeJournal, "r")
			if err != nil || got.Hash() != newest.Hash() {
				t.Fatalf("Latest = %+v, %v; want the size 8 head", got, err)
			}
			if name != "delete the live object" && len(*skipped) == 0 {
				t.Error("the overwriting generation was skipped without a report")
			}
			rep, err := r.Audit(ctx, audit.TreeJournal, "r", f.Now())
			if err != nil || !hasFinding(rep, "noncurrent generation") || !hasFinding(rep, "2 generations of one name") && name != "delete the live object" {
				t.Fatalf("Audit = %+v, %v; want a noncurrent finding", rep.Findings, err)
			}
		})
	}
}

func hasFinding(rep gcs.Report, sub string) bool {
	for _, f := range rep.Findings {
		if strings.Contains(f.String(), sub) {
			return true
		}
	}
	return false
}

func TestReaderSkipsWhatIsNoHeadAndErrorsOnAForgery(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	good := k.head(t, "r", 3, 1)
	publish(t, a, good)
	partition := strings.Split(f.Names()[0], "/")[1]
	f.Inject(gcs.ObjectInfo{Name: "bide-anchor/" + partition + "/stray.txt", RetentionExpiry: f.Now().Add(time.Hour)}, []byte("hello"))
	f.Inject(gcs.ObjectInfo{Name: "elsewhere/x.json"}, []byte("{}"))
	r, skipped := newReaderSkips(t, f, k.v)
	got, err := r.Latest(ctx, audit.TreeJournal, "r")
	if err != nil || got.Hash() != good.Hash() {
		t.Fatalf("Latest = %v, %v; want the good head despite a stray object", got, err)
	}
	if len(*skipped) != 1 || !strings.Contains((*skipped)[0].Key, "stray.txt") {
		t.Fatalf("skipped = %v; want the stray object reported", *skipped)
	}
	// A well-formed head signed by another key is an error, never skipped.
	forged := newKeys(t, 2).head(t, "r", 7, 1)
	publish(t, a, forged)
	if _, err := r.Latest(ctx, audit.TreeJournal, "r"); !errors.Is(err, gcs.ErrBadSignature) || !errors.Is(err, gcs.ErrObject) {
		t.Fatalf("Latest with a forged head = %v; want ErrBadSignature", err)
	}
	if _, err := r.Heads(ctx, audit.TreeJournal, "r"); !errors.Is(err, gcs.ErrBadSignature) {
		t.Fatalf("Heads with a forged head = %v; want ErrBadSignature", err)
	}
}

func TestReaderRefusesTamperedBytes(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	publish(t, a, k.head(t, "r", 3, 1))
	name := f.Names()[0]
	_, body := nameAndBody(t, k, k.head(t, "r", 3, 2)) // a valid head, but not the one the name digests
	f.Tamper(name, f.Generations(name)[0].Generation, body)
	r, skipped := newReaderSkips(t, f, k.v)
	if _, err := r.Latest(ctx, audit.TreeJournal, "r"); !errors.Is(err, gcs.ErrNotFound) || len(*skipped) != 1 {
		t.Fatalf("Latest = %v, skipped %v; want the tampered object skipped and reported", err, *skipped)
	}
	rep, err := r.Audit(ctx, audit.TreeJournal, "r", f.Now())
	if err != nil || !hasFinding(rep, "claims size 3") {
		t.Fatalf("Audit = %+v, %v; want a claimed-size finding", rep.Findings, err)
	}
}

func TestReaderReportsEquivocation(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	publish(t, a, k.headRoot(t, "r", 4, 1, "history A"))
	publish(t, a, k.headRoot(t, "r", 4, 2, "history B"))
	r := newReader(t, f, k.v)
	if _, err := r.Latest(ctx, audit.TreeJournal, "r"); !errors.Is(err, gcs.ErrEquivocation) {
		t.Fatalf("Latest = %v; want ErrEquivocation", err)
	}
	if _, err := r.Heads(ctx, audit.TreeJournal, "r"); !errors.Is(err, gcs.ErrEquivocation) {
		t.Fatalf("Heads = %v; want ErrEquivocation", err)
	}
	if _, err := r.Head(ctx, audit.TreeJournal, "r", 4); !errors.Is(err, gcs.ErrEquivocation) {
		t.Fatalf("Head = %v; want ErrEquivocation", err)
	}
	rep, err := r.Audit(ctx, audit.TreeJournal, "r", f.Now())
	if err != nil || !hasFinding(rep, "two histories") {
		t.Fatalf("Audit = %+v, %v; want an equivocation finding", rep.Findings, err)
	}
	// Two heads of one size with the same root are two processes around a lease handoff, not equivocation.
	g := gcstest.New()
	b := newAnchor(t, g, nil)
	publish(t, b, k.headRoot(t, "r", 4, 1, "same"))
	publish(t, b, k.headRoot(t, "r", 4, 2, "same"))
	if got, err := newReader(t, g, k.v).Latest(ctx, audit.TreeJournal, "r"); err != nil || got.Size != 4 {
		t.Fatalf("Latest of same-root heads = %v, %v", got, err)
	}
}

func TestAuditReportsHowHeadsAreStored(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, nil)
	r := newReader(t, f, k.v)
	publish(t, a, k.head(t, "r", 1, 1))
	rep, err := r.Audit(ctx, audit.TreeJournal, "r", f.Now())
	if err != nil || len(rep.Heads) != 1 || len(rep.Findings) != 0 {
		t.Fatalf("Audit of an intact anchor = %+v, %v; want one head and no findings", rep, err)
	}
	f.Advance(31 * 24 * time.Hour)
	rep, _ = r.Audit(ctx, audit.TreeJournal, "r", f.Now())
	if !hasFinding(rep, "retention has ended") {
		t.Errorf("findings after the retention = %v; want retention ended", rep.Findings)
	}
	for name, mod := range map[string]func(*gcs.ObjectInfo){
		"no retention covers it": func(i *gcs.ObjectInfo) { i.RetentionExpiry = time.Time{} },
		"Unlocked": func(i *gcs.ObjectInfo) {
			i.Retention = &gcs.ObjectRetention{Mode: "Unlocked", RetainUntil: i.Created.Add(24 * time.Hour)}
		},
	} {
		g := gcstest.New()
		h := k.head(t, "r", 2, 1)
		injectHead(t, k, g, h, mod)
		rep, err := newReader(t, g, k.v).Audit(ctx, audit.TreeJournal, "r", g.Now())
		if err != nil || len(rep.Heads) != 1 || !hasFinding(rep, name) {
			t.Errorf("%s: Audit = %+v, %v", name, rep.Findings, err)
		}
	}
	// With the configured key known, an object under another key is a finding.
	g := gcstest.New()
	injectHead(t, k, g, k.head(t, "r", 2, 1), func(i *gcs.ObjectInfo) { i.KMSKey = "" })
	kr, err := gcs.NewReader(ctx, gcs.ReaderConfig{Store: g, ExpectedProjectNumber: gcstest.ProjectNumber, Verifier: k.v, KMSKey: kmsKey})
	if err != nil {
		t.Fatal(err)
	}
	if rep, _ := kr.Audit(ctx, audit.TreeJournal, "r", g.Now()); !hasFinding(rep, "not a version of the configured key") {
		t.Errorf("findings = %v; want a key finding", rep.Findings)
	}
}

func TestReaderFailuresAreExplicit(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	publish(t, newAnchor(t, f, nil), k.head(t, "r", 1, 1))
	r := newReader(t, f, k.v)
	f.FailNext("List", gcs.ErrUnavailable, 1)
	if _, err := r.Latest(ctx, audit.TreeJournal, "r"); !errors.Is(err, gcs.ErrUnavailable) {
		t.Fatalf("Latest with a failed listing = %v; want ErrUnavailable", err)
	}
	f.FailNext("Read", gcs.ErrDenied, 1)
	if _, err := r.Latest(ctx, audit.TreeJournal, "r"); !errors.Is(err, gcs.ErrDenied) || errors.Is(err, gcs.ErrNotFound) {
		t.Fatalf("Latest with a denied read = %v; want a denied error, not a skip", err)
	}
}

func TestLoadDir(t *testing.T) {
	k := newKeys(t, 1)
	dir := t.TempDir()
	for _, h := range []audit.SignedTreeHead{k.head(t, "r", 2, 1), k.head(t, "r", 1, 1)} {
		name, body := nameAndBody(t, k, h)
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	heads, err := gcs.LoadDir(dir, k.v)
	if err != nil || len(heads) != 2 || heads[0].Size != 1 || heads[1].Size != 2 {
		t.Fatalf("LoadDir = %v, %v; want sizes 1, 2", heads, err)
	}
	if _, err := gcs.LoadDir(dir, nil); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("LoadDir without a verifier = %v", err)
	}
	if _, err := gcs.LoadDir(dir, newKeys(t, 2).v); !errors.Is(err, gcs.ErrBadSignature) {
		t.Fatalf("LoadDir under another key = %v; want ErrBadSignature", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gcs.LoadDir(dir, k.v); !errors.Is(err, gcs.ErrObject) {
		t.Fatalf("LoadDir with a stray file = %v; want ErrObject", err)
	}
}

// A bucket another project owns is ErrWrongProject (and still a configuration error), so a caller can tell
// the fact from a failure to read the bucket.
func TestNewReaderWrongProjectIsDistinct(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	_, err := gcs.NewReader(ctx, gcs.ReaderConfig{Store: f, ExpectedProjectNumber: gcstest.ProjectNumber + 1, Verifier: k.v})
	if !errors.Is(err, gcs.ErrWrongProject) || !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("NewReader = %v, want ErrWrongProject and agent.ErrConfig", err)
	}
	f.FailNext("Bucket", gcs.ErrUnavailable, 1)
	if _, err := gcs.NewReader(ctx, gcs.ReaderConfig{Store: f, ExpectedProjectNumber: gcstest.ProjectNumber, Verifier: k.v}); errors.Is(err, gcs.ErrWrongProject) || !errors.Is(err, gcs.ErrUnavailable) {
		t.Fatalf("an unreadable bucket = %v, want ErrUnavailable and not ErrWrongProject", err)
	}
}
