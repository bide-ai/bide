package runview

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// Proof is the audit verdict on the run's journal, from the audit package's own verifiers (the
// code bide-audit verify and verify-evidence run), never a reimplementation.
type Proof struct {
	Source   string   `json:"source"` // "signed tree head" or "evidence package"
	OK       bool     `json:"ok"`
	Summary  string   `json:"summary"`
	Covers   int      `json:"covers"` // journal records the signed head commits to
	Problems []string `json:"problems,omitempty"`
	Items    []string `json:"items,omitempty"`
}

// Prover checks a run's records against an audit artifact. It returns nil for a run the artifact
// is not about.
type Prover func(ctx context.Context, runID string, recs []agent.Record) *Proof

// HeadProver checks a signed tree head: its signature under v and the timestamp rule
// (audit.CheckTimestamp), and that its root commits to the first sth.Size records of the journal
// shown, by building and verifying an inclusion proof of the last of them (audit.ProveRecord,
// ProofBundle.Verify), as bide-audit prove and verify do.
func HeadProver(sth audit.SignedTreeHead, v audit.Verifier) Prover {
	return func(ctx context.Context, runID string, recs []agent.Record) *Proof {
		if sth.RunID != runID {
			return nil
		}
		p := &Proof{Source: "signed tree head", Covers: sth.Size}
		check(p, verifyHead(ctx, runID, recs, sth, v))
		finish(p, len(recs))
		return p
	}
}

// EvidenceProver checks an evidence package (audit.EvidencePackage.Verify, as bide-audit
// verify-evidence does) and that its signed head commits to the journal shown.
func EvidenceProver(pkg audit.EvidencePackage, v audit.Verifier, opts ...audit.EvidenceVerifyOption) Prover {
	return func(ctx context.Context, runID string, recs []agent.Record) *Proof {
		if pkg.RunID != runID {
			return nil
		}
		p := &Proof{Source: "evidence package", Covers: pkg.STH.Size}
		rep, err := pkg.Verify(v, opts...)
		if err != nil && !errors.Is(err, audit.ErrNotVerified) {
			p.Problems = append(p.Problems, err.Error())
		}
		p.Problems = append(p.Problems, rep.Problems...)
		for _, it := range rep.Items {
			mark := "verified"
			if !it.Verified {
				mark = "NOT verified"
			}
			p.Items = append(p.Items, fmt.Sprintf("%s: %s (%s)", it.Label, mark, it.Note))
		}
		if !rep.OK {
			p.Problems = append(p.Problems, "the package did not verify under this key")
		}
		check(p, verifyHead(ctx, runID, recs, pkg.STH, v))
		finish(p, len(recs))
		return p
	}
}

// verifyHead checks sth against recs: the timestamp rule, then an inclusion proof of the last
// record the head covers, built from recs and verified under v.
func verifyHead(ctx context.Context, runID string, recs []agent.Record, sth audit.SignedTreeHead, v audit.Verifier) error {
	if err := audit.CheckTimestamp(sth.TreeHead, time.Now(), audit.DefaultClockSkew); err != nil {
		return err
	}
	if sth.Size < 1 {
		return fmt.Errorf("the signed head covers no records")
	}
	j, err := agent.NewJournal(staticStore{recs: recs})
	if err != nil {
		return err
	}
	b, err := audit.ProveRecord(ctx, j, runID, sth.Size-1, sth)
	if err != nil {
		return fmt.Errorf("the signed head does not commit to this journal: %w", err)
	}
	return b.Verify(v)
}

func check(p *Proof, err error) {
	if err != nil {
		p.Problems = append(p.Problems, err.Error())
	}
}

func finish(p *Proof, n int) {
	p.OK = len(p.Problems) == 0
	switch {
	case !p.OK:
		p.Summary = "does not verify"
	case p.Covers < n:
		p.Summary = fmt.Sprintf("verifies: records 1 to %d of %d are under the signed head; the %d after it are not yet covered", p.Covers, n, n-p.Covers)
	default:
		p.Summary = fmt.Sprintf("verifies: all %d records are under the signed head", n)
	}
}
