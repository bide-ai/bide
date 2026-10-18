package anchorobj_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/anchorobj"
)

var (
	errObject = errors.New("test: object")
	errBadSig = errors.New("test: bad signature")
	errEquiv  = errors.New("test: equivocation")
)

var testLayout = anchorobj.Layout{Format: "bide.audit.test-anchor.v1", PartitionTag: "bide.audit.test-anchor.partition.v1\x00", ErrObject: errObject, ErrBadSignature: errBadSig}

// otherLayout is another backend: it differs in format and partition tag only.
var otherLayout = anchorobj.Layout{Format: "bide.audit.other-anchor.v1", PartitionTag: "bide.audit.other-anchor.partition.v1\x00", ErrObject: errObject, ErrBadSignature: errBadSig}

type keys struct {
	signer audit.Signer
	v      audit.Verifier
}

func newKeys(t testing.TB, seed byte) keys {
	t.Helper()
	h := sha256.Sum256([]byte{seed})
	s := audit.Ed25519Signer{Priv: ed25519.NewKeyFromSeed(h[:])}
	v, err := audit.VerifierOf(s)
	if err != nil {
		t.Fatal(err)
	}
	return keys{signer: s, v: v}
}

func (k keys) head(t testing.TB, run string, size int, ts int64, seed string) audit.SignedTreeHead {
	t.Helper()
	root := sha256.Sum256([]byte(seed))
	sth, err := audit.SignTreeHead(audit.TreeHead{Kind: audit.TreeJournal, RunID: run, Size: size, Root: root[:], TimestampNanos: ts}, k.signer)
	if err != nil {
		t.Fatal(err)
	}
	return sth
}

// encoded returns a head's partition, name and body under l.
func encoded(t testing.TB, l anchorobj.Layout, sth audit.SignedTreeHead) (partition, name string, body []byte) {
	t.Helper()
	body, err := l.Encode(sth)
	if err != nil {
		t.Fatal(err)
	}
	key := l.Key("p/", sth.Kind, sth.RunID, sth.Size, body)
	partition, name, ok := anchorobj.SplitKey("p/", key)
	if !ok {
		t.Fatalf("SplitKey(%q) failed", key)
	}
	return partition, name, body
}

func TestEncodeIsDeterministicAndDecodeRoundTrips(t *testing.T) {
	k := newKeys(t, 1)
	sth := k.head(t, "r", 4, 7, "a")
	p, n, body := encoded(t, testLayout, sth)
	again, err := testLayout.Encode(sth)
	if err != nil || string(again) != string(body) {
		t.Fatalf("Encode twice = %q, %v; want the same bytes", again, err)
	}
	got, err := testLayout.Decode(p, n, body, k.v)
	if err != nil || got.Hash() != sth.Hash() {
		t.Fatalf("Decode = %v, %v; want the head", got, err)
	}
	if _, err := testLayout.Decode(p, n, body, nil); err != nil {
		t.Fatalf("Decode without a verifier = %v; the signature check is skipped, nothing else", err)
	}
}

// What must differ between backends does: an object of one layout never decodes under another.
func TestLayoutsDoNotCrossValidate(t *testing.T) {
	k := newKeys(t, 1)
	sth := k.head(t, "r", 4, 7, "a")
	if testLayout.Partition("journal", "r") == otherLayout.Partition("journal", "r") {
		t.Fatal("two backends share a partition name")
	}
	p, n, body := encoded(t, testLayout, sth)
	if _, err := otherLayout.Decode(p, n, body, k.v); !errors.Is(err, errObject) {
		t.Fatalf("an object of one layout decoded under another: %v", err)
	}
	// Same name and partition, the other backend's format in the body: refused for its format.
	_, _, ob := encoded(t, otherLayout, sth)
	if _, err := testLayout.Decode(p, n, ob, k.v); !errors.Is(err, errObject) || !strings.Contains(err.Error(), "has format") {
		t.Fatalf("a body of another format: %v; want an error naming its format", err)
	}
}

func TestDecodeRefusesWhatTheNameDoesNotSay(t *testing.T) {
	k := newKeys(t, 1)
	sth := k.head(t, "r", 4, 7, "a")
	p, n, body := encoded(t, testLayout, sth)
	other := k.head(t, "r", 5, 7, "a")
	_, otherName, otherBody := encoded(t, testLayout, other)
	_, _, sameSizeBody := encoded(t, testLayout, k.head(t, "r", 4, 8, "a")) // the same run and size, another head
	otherRun := k.head(t, "q", 4, 7, "a")
	otherPart, _, otherRunBody := encoded(t, testLayout, otherRun)

	for name, tc := range map[string]struct {
		partition, name string
		body            []byte
		want            string
	}{
		"not an object name":       {p, "x.json", body, "is not an object name"},
		"a name of another size":   {p, otherName, body, "named for size"},
		"a body of another digest": {p, n, sameSizeBody, "digest"},
		"another head's body":      {p, n, otherBody, "named for size"},
		"another run's partition":  {otherPart, n, body, "belongs in partition"},
		"another run's body":       {p, n, otherRunBody, "belongs in partition"},
		"not JSON":                 {p, n, []byte("junk"), ""},
		"an unknown member":        {p, n, []byte(strings.Replace(string(body), `{"format"`, `{"extra":1,"format"`, 1)), ""},
		"a trailing newline":       {p, n, append(append([]byte{}, body...), '\n'), ""},
		"different spacing":        {p, n, []byte(strings.Replace(string(body), `,"sth"`, `, "sth"`, 1)), ""},
	} {
		_, err := testLayout.Decode(tc.partition, tc.name, tc.body, k.v)
		if !errors.Is(err, errObject) {
			t.Errorf("%s: Decode = %v; want an error wrapping the layout's ErrObject", name, err)
			continue
		}
		if errors.Is(err, errBadSig) {
			t.Errorf("%s: reported as a bad signature: %v", name, err)
		}
		if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, tc.want)
		}
	}
}

func TestDecodeReportsABadSignatureAsBoth(t *testing.T) {
	k := newKeys(t, 1)
	sth := newKeys(t, 2).head(t, "r", 4, 7, "a") // well formed, signed by another key
	p, n, body := encoded(t, testLayout, sth)
	_, err := testLayout.Decode(p, n, body, k.v)
	if !errors.Is(err, errObject) || !errors.Is(err, errBadSig) {
		t.Fatalf("Decode = %v; want both ErrObject and ErrBadSignature", err)
	}
}

func TestCheckEquivocation(t *testing.T) {
	k := newKeys(t, 1)
	a := k.head(t, "r", 4, 1, "A")
	sameRoot := k.head(t, "r", 4, 2, "A")
	b := k.head(t, "r", 4, 3, "B")
	other := k.head(t, "r", 5, 1, "C")
	for name, tc := range map[string]struct {
		heads []audit.SignedTreeHead
		bad   bool
	}{
		"none":                       {nil, false},
		"one":                        {[]audit.SignedTreeHead{a}, false},
		"two sizes":                  {[]audit.SignedTreeHead{a, other}, false},
		"one size, one root":         {[]audit.SignedTreeHead{a, sameRoot}, false},
		"one size, two roots":        {[]audit.SignedTreeHead{a, b}, true},
		"two roots, any order":       {[]audit.SignedTreeHead{other, b, a}, true},
		"two roots, a third between": {[]audit.SignedTreeHead{a, other, b}, true},
	} {
		err := anchorobj.CheckEquivocation(tc.heads, errEquiv)
		if (err != nil) != tc.bad || (err != nil && !errors.Is(err, errEquiv)) {
			t.Errorf("%s: CheckEquivocation = %v, want bad=%v wrapping the given error", name, err, tc.bad)
		}
	}
}

// A body that is a valid head in another spelling (spacing, member order) and whose name carries its own
// digest passes every check but the canonical-encoding one, which is what stops two spellings of one head
// from being two objects the Reader counts twice.
func TestDecodeRefusesANonCanonicalBodyWhoseNameMatchesIt(t *testing.T) {
	k := newKeys(t, 1)
	sth := k.head(t, "r", 4, 7, "a")
	p, _, body := encoded(t, testLayout, sth)
	respaced := []byte(strings.Replace(string(body), `,"sth"`, `, "sth"`, 1))
	name := anchorobj.ObjectName(sth.Size, respaced) // the digest of the respaced bytes
	_, err := testLayout.Decode(p, name, respaced, k.v)
	if !errors.Is(err, errObject) || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("Decode = %v; want a canonical-encoding refusal", err)
	}
}

func TestLayoutValidate(t *testing.T) {
	if err := testLayout.Validate(); err != nil {
		t.Fatalf("a good layout is invalid: %v", err)
	}
	for name, mod := range map[string]func(*anchorobj.Layout){
		"no format":          func(l *anchorobj.Layout) { l.Format = "" },
		"no tag":             func(l *anchorobj.Layout) { l.PartitionTag = "" },
		"tag without a NUL":  func(l *anchorobj.Layout) { l.PartitionTag = "bide.audit.test-anchor.partition.v1" },
		"tag of only a NUL":  func(l *anchorobj.Layout) { l.PartitionTag = "\x00" },
		"no ErrObject":       func(l *anchorobj.Layout) { l.ErrObject = nil },
		"no ErrBadSignature": func(l *anchorobj.Layout) { l.ErrBadSignature = nil },
	} {
		l := testLayout
		mod(&l)
		if l.Validate() == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}

func TestCollides(t *testing.T) {
	if testLayout.Collides(otherLayout) || otherLayout.Collides(testLayout) {
		t.Error("distinct layouts collide")
	}
	sameTag, sameFormat := otherLayout, otherLayout
	sameTag.PartitionTag = testLayout.PartitionTag
	sameFormat.Format = testLayout.Format
	if !testLayout.Collides(sameTag) || !testLayout.Collides(sameFormat) {
		t.Error("layouts sharing a tag or a format do not collide")
	}
}

func TestDecodeRefusesAnInvalidLayout(t *testing.T) {
	k := newKeys(t, 1)
	p, n, body := encoded(t, testLayout, k.head(t, "r", 4, 7, "a"))
	bad := testLayout
	bad.ErrObject = nil
	if _, err := bad.Decode(p, n, body, k.v); err == nil || strings.Contains(err.Error(), "%!w") {
		t.Fatalf("Decode with a nil ErrObject = %v; want a clear validation error", err)
	}
}

func TestCheckEquivocationGuards(t *testing.T) {
	k := newKeys(t, 1)
	if err := anchorobj.CheckEquivocation([]audit.SignedTreeHead{k.head(t, "r", 4, 1, "A")}, nil); err == nil {
		t.Error("a nil equivocation error was accepted")
	}
	// Two runs with a head of one size and different roots are not equivocation: they are not one tree.
	err := anchorobj.CheckEquivocation([]audit.SignedTreeHead{k.head(t, "r", 4, 1, "A"), k.head(t, "q", 4, 1, "B")}, errEquiv)
	if err == nil || errors.Is(err, errEquiv) || !strings.Contains(err.Error(), "more than one tree") {
		t.Errorf("CheckEquivocation across runs = %v; want a usage error that is not equivocation", err)
	}
}
