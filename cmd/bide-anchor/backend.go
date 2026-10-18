package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bide-ai/bide/anchor/awss3"
	"github.com/bide-ai/bide/audit"
)

// A backend is one object store the anchors write to. The command's checks (verify, export, check) are
// the same for every backend; a backend says how to reach its bucket and how to read an object set.
// Backends are chosen with -backend. Only s3 is built in today; the Google Cloud Storage backend is
// added here when its package ships.
type backend struct {
	name string
	// open returns a reader of the bucket named by o, with read-only credentials.
	open func(ctx context.Context, o openOptions) (headReader, error)
	// loadDir reads and verifies a downloaded object set.
	loadDir func(dir string, v audit.Verifier) ([]audit.SignedTreeHead, error)
	// errNotFound is the backend's "no head anchored" error, wrapped when an offline set holds none.
	errNotFound error
	// ownerFlag is the name of the flag that names the account or project that owns the bucket,
	// and what it holds, for messages.
	ownerFlag, ownerWhat string
}

// openOptions are the flags an online command gives its backend.
type openOptions struct {
	bucket, owner, prefix string
	pathStyle             bool
	verifier              audit.Verifier
	onSkip                func(string) // an object the reader skipped because it claims to be no head
}

// headReader reads the heads of one tree from a bucket and reports how they are stored.
type headReader interface {
	Heads(ctx context.Context, kind, runID string) ([]audit.SignedTreeHead, error)
	Audit(ctx context.Context, kind, runID string, now time.Time) (report, error)
}

// report is a backend-neutral awss3.Report: the verified heads and one line per finding.
type report struct {
	Heads    []audit.SignedTreeHead
	Findings []string
}

// backendNames is the names -backend accepts, for messages.
const backendNames = "s3"

// lookupBackend returns the named backend; mk makes the S3 client.
func lookupBackend(name string, mk clientFunc) (backend, error) {
	switch name {
	case "s3":
		return s3Backend(mk), nil
	case "gcs":
		return backend{}, errors.New("-backend gcs is not in this build yet; this build has: " + backendNames)
	default:
		return backend{}, fmt.Errorf("-backend %q is not one of: %s", name, backendNames)
	}
}

func s3Backend(mk clientFunc) backend {
	return backend{
		name:        "s3",
		errNotFound: awss3.ErrNotFound,
		ownerFlag:   "-owner",
		ownerWhat:   "the 12-digit AWS account ID that owns the bucket",
		loadDir:     awss3.LoadDir,
		open: func(ctx context.Context, o openOptions) (headReader, error) {
			c, err := mk(ctx, o.pathStyle)
			if err != nil {
				return nil, fmt.Errorf("AWS configuration: %w", err)
			}
			r, err := awss3.NewReader(awss3.ReaderConfig{Client: c, Bucket: o.bucket, ExpectedBucketOwner: o.owner, Prefix: o.prefix,
				Verifier: o.verifier, OnSkip: func(f awss3.Finding) { o.onSkip(f.String()) }})
			if err != nil {
				return nil, err
			}
			return s3Reader{r}, nil
		},
	}
}

type s3Reader struct{ r *awss3.Reader }

func (s s3Reader) Heads(ctx context.Context, kind, runID string) ([]audit.SignedTreeHead, error) {
	return s.r.Heads(ctx, kind, runID)
}

func (s s3Reader) Audit(ctx context.Context, kind, runID string, now time.Time) (report, error) {
	rep, err := s.r.Audit(ctx, kind, runID, now)
	if err != nil {
		return report{}, err
	}
	out := report{Heads: rep.Heads}
	for _, f := range rep.Findings {
		out.Findings = append(out.Findings, f.String())
	}
	return out, nil
}
