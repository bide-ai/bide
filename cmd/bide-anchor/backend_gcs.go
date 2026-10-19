package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"cloud.google.com/go/storage"
	"github.com/bide-ai/bide/anchor/gcs"
	"github.com/bide-ai/bide/audit"
)

// gcsStore opens the bucket for reading. It uses Application Default Credentials, and tests replace it.
var gcsStore = func(ctx context.Context, bucket string) (gcs.Store, error) {
	c, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: Google Cloud configuration: %w", errUsage, err)
	}
	return gcs.NewStore(c, bucket), nil
}

func gcsBackend() backend {
	return backend{
		name:        "gcs",
		errNotFound: gcs.ErrNotFound,
		ownerFlag:   "-project",
		ownerWhat:   "the number of the Google Cloud project that owns the bucket",
		loadDir:     gcs.LoadDir,
		// A bucket of another project is a fact the Reader saw (the name may have been recreated by someone
		// else). A vanished generation (listed, then unreadable) is under a locked retention policy a sign of
		// tampering or of a misconfiguration, so it is a finding; an unavailable or denied read is not.
		isFinding: func(err error) bool {
			return errors.Is(err, gcs.ErrNotFound) || errors.Is(err, gcs.ErrObject) || errors.Is(err, gcs.ErrBadSignature) ||
				errors.Is(err, gcs.ErrEquivocation) || errors.Is(err, gcs.ErrVanished) || errors.Is(err, gcs.ErrWrongProject)
		},
		open: func(ctx context.Context, o openOptions) (headReader, error) {
			if o.owner != "" {
				return nil, fmt.Errorf("%w: -owner is for -backend s3; the gcs backend takes -project", errUsage)
			}
			if o.lockMode != "" {
				return nil, fmt.Errorf("%w: -lock-mode is for -backend s3 (Object Lock modes); a Cloud Storage retention policy is locked or not", errUsage)
			}
			n, err := strconv.ParseUint(o.project, 10, 64)
			if err != nil || n == 0 {
				return nil, fmt.Errorf("%w: -project %q is not a project number (digits, not the project ID)", errUsage, o.project)
			}
			st, err := gcsStore(ctx, o.bucket)
			if err != nil {
				return nil, err
			}
			r, err := gcs.NewReader(ctx, gcs.ReaderConfig{Store: st, ExpectedProjectNumber: n, Prefix: o.prefix, KMSKey: o.kmsKey, Retention: o.retention, Verifier: o.verifier,
				OnSkip: func(f gcs.Finding) { o.onSkip(f.String()) }})
			if err != nil {
				return nil, err
			}
			return gcsReader{r}, nil
		},
	}
}

type gcsReader struct{ r *gcs.Reader }

func (g gcsReader) Heads(ctx context.Context, kind, runID string) ([]audit.SignedTreeHead, error) {
	return g.r.Heads(ctx, kind, runID)
}

func (g gcsReader) Audit(ctx context.Context, kind, runID string, now time.Time) (report, error) {
	rep, err := g.r.Audit(ctx, kind, runID, now)
	if err != nil {
		return report{}, err
	}
	out := report{Heads: rep.Heads}
	for _, f := range rep.Findings {
		out.Findings = append(out.Findings, f.String())
	}
	return out, nil
}
