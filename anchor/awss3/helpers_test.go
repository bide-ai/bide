package awss3_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/bide-ai/bide/anchor/awss3"
	"github.com/bide-ai/bide/anchor/awss3/awss3test"
	"github.com/bide-ai/bide/audit"
)

const (
	bucket = "bank-anchor"
	owner  = "111122223333"
	kmsARN = "arn:aws:kms:eu-west-1:111122223333:key/abcd-1234"
)

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
	root := sha256.Sum256([]byte(run + string(rune(size))))
	th := audit.TreeHead{Kind: audit.TreeJournal, RunID: run, Size: size, Root: root[:], TimestampNanos: ts}
	sth, err := audit.SignTreeHead(th, k.signer)
	if err != nil {
		t.Fatal(err)
	}
	return sth
}

func baseConfig(f *awss3test.Fake) awss3.Config {
	return awss3.Config{Client: f, Bucket: bucket, ExpectedBucketOwner: owner, Retention: 30 * 24 * time.Hour,
		KMSKeyID: kmsARN, OnWarn: func(error) {}}
}

func newAnchor(t testing.TB, f *awss3test.Fake, mod func(*awss3.Config)) *awss3.Anchor {
	t.Helper()
	cfg := baseConfig(f)
	if mod != nil {
		mod(&cfg)
	}
	a, err := awss3.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func newReader(t testing.TB, f *awss3test.Fake, v audit.Verifier) *awss3.Reader {
	t.Helper()
	r, err := awss3.NewReader(awss3.ReaderConfig{Client: f, Bucket: bucket, ExpectedBucketOwner: owner, Verifier: v})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
