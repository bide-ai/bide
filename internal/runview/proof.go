package runview

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// Proof is the audit verdict on the run's journal, from the audit package's own verifiers (the
// code bide audit verify and verify-evidence run), never a reimplementation.
type Proof struct {
	Source   string   `json:"source"` // "signed tree head" or "evidence package"
	OK       bool     `json:"ok"`
	Summary  string   `json:"summary"`
	Covers   int      `json:"covers"` // journal records the signed head commits to
	Problems []string `json:"problems,omitempty"`
	Items    []string `json:"items,omitempty"`
	// Approvals is the signature verdict on each m-of-n gate that journaled its tally, by
	// tool-use ID: audit.VerifyApprovals over the gate's evidence, with the approver keys given.
	// Nil without approver keys.
	Approvals map[string]ApprovalCheck `json:"approvals,omitempty"`

	// What the page draws from the proof; none of it is in the JSON view.
	Merkle   *MerklePath `json:"-"` // the proven record's path to the signed root
	SignedAt time.Time   `json:"-"` // when the signed head was signed
}

// ApprovalCheck is audit.VerifyApprovals' verdict on one gate, in words.
type ApprovalCheck struct {
	OK   bool   `json:"ok"`
	Text string `json:"text"`

	// What the approver cards show; not in the JSON view. Counted and Denied are the approvers
	// whose decision counted in the recount, Ignored the reason each other decision did not, Keys
	// each approver's key fingerprint (from the approver keys given), and Times the two times of
	// each counted approver's decision, with their sanity flags (audit.ApprovalVerdict.Times).
	Counted, Denied []string                      `json:"-"`
	Ignored         map[string]string             `json:"-"`
	Keys            map[string]string             `json:"-"`
	Times           map[string]audit.DecisionTime `json:"-"`
}

// Prover checks a run's records against an audit artifact. It returns nil for a run the artifact
// is not about.
type Prover func(ctx context.Context, runID string, recs []agent.Record) *Proof

// HeadProver checks a signed tree head: its signature under v and the timestamp rule
// (audit.CheckTimestamp), and that its root commits to the first sth.Size records of the journal
// shown, by building and verifying an inclusion proof of the last of them (audit.ProveRecord,
// ProofBundle.Verify), as bide audit prove and verify do.
//
// With approver keys (ap.Keys non-nil), it also checks each m-of-n gate's signatures: it builds the
// gate's evidence from the journal under sth (audit.ApprovalEvidence) and verifies it with
// audit.VerifyApprovals, as bide audit verify-approvals does.
func HeadProver(sth audit.SignedTreeHead, v audit.Verifier, ap Approvals) Prover {
	return func(ctx context.Context, runID string, recs []agent.Record) *Proof {
		if sth.RunID != runID {
			return nil
		}
		p := &Proof{Source: "signed tree head", Covers: sth.Size, SignedAt: headTime(sth)}
		m, err := verifyHead(ctx, runID, recs, sth, v)
		check(p, err)
		p.Merkle = m
		if ap.Keys != nil {
			j, err := agent.NewJournal(staticStore{recs: recs[:min(sth.Size, len(recs))]})
			p.Approvals = checkApprovals(recs, ap, v, func(id string) ([]audit.EvidenceAction, error) {
				if err != nil {
					return nil, err
				}
				return audit.ApprovalEvidence(ctx, j, runID, id, sth)
			})
		}
		finish(p, len(recs))
		return p
	}
}

// EvidenceProver checks an evidence package (audit.EvidencePackage.Verify, as bide audit
// verify-evidence does) and that its signed head commits to the journal shown.
//
// With approver keys (ap.Keys non-nil), it also checks each m-of-n gate's signatures with
// audit.VerifyApprovals over the package's actions, as bide audit verify-approvals does.
func EvidenceProver(pkg audit.EvidencePackage, v audit.Verifier, ap Approvals, opts ...audit.EvidenceVerifyOption) Prover {
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
		p.SignedAt = headTime(pkg.STH)
		m, herr := verifyHead(ctx, runID, recs, pkg.STH, v)
		check(p, herr)
		p.Merkle = m
		if ap.Keys != nil {
			p.Approvals = checkApprovals(recs, ap, v, func(string) ([]audit.EvidenceAction, error) { return pkg.Actions, nil })
		}
		finish(p, len(recs))
		return p
	}
}

// checkApprovals runs audit.VerifyApprovals on every m-of-n gate of recs (a tool call's, or a plan
// flow node's) that journaled its tally,
// against ap.Expected when set and otherwise the policy that tally records (the policy the gate
// enforced), over the evidence
// actionsFor returns for the call.
func checkApprovals(recs []agent.Record, ap Approvals, logV audit.Verifier, actionsFor func(id string) ([]audit.EvidenceAction, error)) map[string]ApprovalCheck {
	byName := map[string]agent.Record{}
	for _, r := range recs {
		byName[r.Name] = r
	}
	out := map[string]ApprovalCheck{}
	// The gated calls: each model turn's tool calls, and each gated plan flow node's request.
	var calls []agent.ToolUse
	for _, r := range recs {
		if q, ok := flowRequest(r); ok {
			calls = append(calls, q)
			continue
		}
		if r.Kind != agent.StepModel || r.Message == nil {
			continue
		}
		for _, part := range r.Message.Parts {
			if tu, ok := part.(agent.ToolUse); ok {
				calls = append(calls, tu)
			}
		}
	}
	for _, tu := range calls {
		t, ok := agent.RecordedTally(byName, tu.ID)
		if !ok {
			continue
		}
		var tally agent.ApprovalTally
		if err := audit.UnmarshalStrict(t.Result, &tally); err != nil {
			out[tu.ID] = ApprovalCheck{Text: "signatures not verified: the journaled tally does not read: " + err.Error()}
			continue
		}
		recorded := agent.ApprovalPolicy{Need: tally.Need, Approvers: tally.Approvers}
		policy, against := recorded, "against the recorded policy"
		var mismatch string
		if ap.Expected != nil {
			policy, against = *ap.Expected, "against the expected policy"
			if recorded.Need != policy.Need || !slices.Equal(recorded.Approvers, policy.Approvers) {
				mismatch = fmt.Sprintf("recorded policy %s differs from expected %s", policyText(recorded), policyText(policy))
			}
		}
		actions, err := actionsFor(tu.ID)
		if err != nil {
			out[tu.ID] = ApprovalCheck{Text: "signatures not verified: no evidence for this gate under the signed head: " + err.Error()}
			continue
		}
		v, err := audit.VerifyApprovals(actions, tu.ID, policy, ap.Keys, logV)
		c := approvalText(v, err, len(policy.Approvers), mismatch)
		c.Text += " (" + against + ")"
		c.Counted, c.Denied, c.Keys = v.Counted, v.DeniedBy, fingerprints(policy.Approvers, ap.Keys)
		if len(v.Ignored) > 0 {
			c.Ignored = map[string]string{}
			for _, d := range v.Ignored {
				c.Ignored[d.Approver] = d.Reason
			}
		}
		if len(v.Times) > 0 {
			c.Times = map[string]audit.DecisionTime{}
			for _, d := range v.Times {
				c.Times[d.Approver] = d
			}
		}
		out[tu.ID] = c
	}
	return out
}

// Approvals configures the approval-signature check a Prover runs: Keys resolves each approver's
// public key (nil: no check), and Expected, when set, is the policy the auditor expects, which the
// check runs against instead of the policy the gate's tally records.
type Approvals struct {
	Keys     agent.ApproverVerifierFor
	Expected *agent.ApprovalPolicy
}

// policyText renders a policy as "k of n (a, b, c)".
func policyText(p agent.ApprovalPolicy) string {
	return fmt.Sprintf("%d of %d (%s)", p.Need, len(p.Approvers), strings.Join(p.Approvers, ", "))
}

// approvalText phrases a VerifyApprovals verdict: the count and who counted when it holds, and
// otherwise every reason it does not (a problem, a decision that did not count and why, the count
// below k, or the verifier's error).
func approvalText(v audit.ApprovalVerdict, err error, n int, mismatch string) ApprovalCheck {
	if err == nil && v.OK && mismatch == "" {
		return ApprovalCheck{OK: true, Text: fmt.Sprintf("%d of %d: signatures verified (%s)", len(v.Counted), n, strings.Join(v.Counted, ", "))}
	}
	var why []string
	if mismatch != "" {
		why = append(why, mismatch)
	}
	for _, p := range v.Problems {
		if mismatch != "" && strings.HasPrefix(p, "the gate enforced need") {
			continue // the same mismatch, in VerifyApprovals' words
		}
		why = append(why, p)
	}
	for _, d := range v.Ignored {
		why = append(why, fmt.Sprintf("%s's decision did not count: %s", d.Approver, d.Reason))
	}
	if len(v.Counted) < v.Need {
		why = append(why, fmt.Sprintf("only %d of the %d needed approvals verified", len(v.Counted), v.Need))
	}
	if err != nil && len(why) == 0 {
		why = append(why, err.Error())
	}
	return ApprovalCheck{Text: "signatures NOT verified: " + strings.Join(why, "; ")}
}

// verifyHead checks sth against recs: the timestamp rule, then an inclusion proof of the last
// record the head covers, built from recs and verified under v. It returns that record's path to
// the root, for the page to draw, when the proof verified.
func verifyHead(ctx context.Context, runID string, recs []agent.Record, sth audit.SignedTreeHead, v audit.Verifier) (*MerklePath, error) {
	if err := audit.CheckTimestamp(sth.TreeHead, time.Now(), audit.DefaultClockSkew); err != nil {
		return nil, err
	}
	if sth.Size < 1 {
		return nil, fmt.Errorf("the signed head covers no records")
	}
	j, err := agent.NewJournal(staticStore{recs: recs})
	if err != nil {
		return nil, err
	}
	b, err := audit.ProveRecord(ctx, j, runID, sth.Size-1, sth)
	if err != nil {
		return nil, fmt.Errorf("the signed head does not commit to this journal: %w", err)
	}
	if err := b.Verify(v); err != nil {
		return nil, err
	}
	name := ""
	if sth.Size-1 < len(recs) {
		name = recs[sth.Size-1].Name
	}
	return merklePath(b, name), nil
}

// headTime is when sth was signed.
func headTime(sth audit.SignedTreeHead) time.Time {
	if sth.TimestampNanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, sth.TimestampNanos).UTC()
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
