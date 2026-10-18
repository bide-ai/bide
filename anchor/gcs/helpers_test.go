package gcs_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/bide-ai/bide/anchor/gcs"
	"github.com/bide-ai/bide/anchor/gcs/gcstest"
	"github.com/bide-ai/bide/audit"
)

const kmsKey = gcstest.DefaultKMSKey

var ctx = context.Background()

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

// head returns a signed journal head of a run at a size; ts makes two heads of one size differ.
func (k keys) head(t testing.TB, run string, size int, ts int64) audit.SignedTreeHead {
	t.Helper()
	return k.headRoot(t, run, size, ts, run+string(rune(size)))
}

// headRoot is head with a root chosen by a seed, so two heads of one size can disagree.
func (k keys) headRoot(t testing.TB, run string, size int, ts int64, seed string) audit.SignedTreeHead {
	t.Helper()
	root := sha256.Sum256([]byte(seed))
	sth, err := audit.SignTreeHead(audit.TreeHead{Kind: audit.TreeJournal, RunID: run, Size: size, Root: root[:], TimestampNanos: ts}, k.signer)
	if err != nil {
		t.Fatal(err)
	}
	return sth
}

func baseConfig(f *gcstest.Fake) gcs.Config {
	return gcs.Config{Store: f, ExpectedProjectNumber: gcstest.ProjectNumber, Retention: 30 * 24 * time.Hour,
		KMSKey: kmsKey, OnWarn: func(error) {}, Now: f.Now}
}

func newAnchor(t testing.TB, f *gcstest.Fake, mod func(*gcs.Config)) *gcs.Anchor {
	t.Helper()
	cfg := baseConfig(f)
	if mod != nil {
		mod(&cfg)
	}
	a, err := gcs.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func newReader(t testing.TB, f *gcstest.Fake, v audit.Verifier) *gcs.Reader {
	t.Helper()
	r, _ := newReaderSkips(t, f, v)
	return r
}

// newReaderSkips is newReader and the objects it skipped, as it reports them.
func newReaderSkips(t testing.TB, f *gcstest.Fake, v audit.Verifier) (*gcs.Reader, *[]gcs.Finding) {
	t.Helper()
	var skipped []gcs.Finding
	r, err := gcs.NewReader(ctx, gcs.ReaderConfig{Store: f, ExpectedProjectNumber: gcstest.ProjectNumber, Verifier: v,
		OnSkip: func(f gcs.Finding) { skipped = append(skipped, f) }})
	if err != nil {
		t.Fatal(err)
	}
	return r, &skipped
}

// nameAndBody is the object name and the bytes Publish uses for a head: it publishes to a scratch bucket.
func nameAndBody(t testing.TB, k keys, h audit.SignedTreeHead) (string, []byte) {
	t.Helper()
	f := gcstest.New()
	if err := newAnchor(t, f, nil).Publish(ctx, h.RunID, h); err != nil {
		t.Fatal(err)
	}
	name := f.Names()[0]
	body, _, err := f.Read(ctx, name, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return name, body
}

func publish(t testing.TB, a *gcs.Anchor, h audit.SignedTreeHead) {
	t.Helper()
	if err := a.Publish(ctx, h.RunID, h); err != nil {
		t.Fatal(err)
	}
}
