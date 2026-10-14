// Command makerchecker shows separation of duties ("maker-checker") on an m-of-n approval gate: the
// person who requests a refund may not approve it. The flow (refund.json) gates its refund node on 2
// of ops, finance and risk with exclude_principal, and ops starts the run, so ops is the requester
// (the maker): ops's own signed approval is recorded but never counts, and the refund runs only
// once finance and risk (the checkers) approve. An auditor then verifies offline, from evidence that
// proves who requested the run (its run:principal record) without disclosing the run's input.
//
//	go run ./makerchecker
//
// Demo only: the keys are derived from fixed names (PREDICTABLE KEYS, NOT SECRETS), as in the
// examples beside it.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/plan"
)

//go:embed refund.json
var config []byte

const runID = "refund-mc-42"

// policy is the gate refund.json declares, as the auditor expects it.
var policy = agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}, ExcludePrincipal: true}

// Order is the flow's input, Refund the gated node's input (what approvers sign), Receipt its output.
type Order struct {
	ID string `json:"id"`
}

type Refund struct {
	Order  string `json:"order"`
	Amount int    `json:"amount"`
}

type Receipt struct {
	Order  string `json:"order"`
	Amount int    `json:"amount"`
}

func demoKey(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("bide examples/approval/makerchecker " + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

func approverVerifiers() agent.ApproverVerifierFor {
	return func(id string) (agent.ApproverVerifier, bool) {
		if !strings.Contains(" ops finance risk ", " "+id+" ") {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: demoKey("approver " + id).Public().(ed25519.PublicKey)}, true
	}
}

// story runs the maker-checker refund against j and returns the refunds issued and the offline
// verdict.
func story(ctx context.Context, j *agent.Journal, out func(format string, args ...any)) (int, audit.ApprovalVerdict, error) {
	refunds := 0
	reg := plan.NewRegistry()
	if err := errors.Join(
		reg.RegisterStep("lookup", func(_ context.Context, o Order) (Refund, error) { return Refund{Order: o.ID, Amount: 120}, nil }, plan.ReadOnly()),
		reg.RegisterStep("refund", func(_ context.Context, r Refund) (Refund, error) { refunds++; return r, nil }),
		reg.RegisterStep("notify", func(_ context.Context, r Refund) (Receipt, error) { return Receipt(r), nil }, plan.ReadOnly()),
	); err != nil {
		return 0, audit.ApprovalVerdict{}, err
	}
	flow, err := plan.Load[Order, Receipt](config, reg, plan.WithLoadedApproverVerifiers(approverVerifiers()))
	if err != nil {
		return 0, audit.ApprovalVerdict{}, err
	}
	// The maker: the deployment's authentication says ops asked for this refund.
	asOps := agent.ContextWithIdentity(ctx, agent.Identity{Actor: "refund-desk@1", OnBehalfOf: "ops"})
	run := func() (*agent.ApprovalPending, error) {
		_, err := flow.Run(asOps, j, runID, Order{ID: "order-42"})
		var pend *agent.ApprovalPending
		if errors.As(err, &pend) {
			q := pend.Quorum
			out("  paused: %d approved of %d needed; excluded as the requester: %s; waiting on %s\n",
				q.Approved, q.Need, strings.Join(q.Excluded, ", "), strings.Join(q.Pending, ", "))
			return pend, nil
		}
		return nil, err
	}
	sign := func(pend *agent.ApprovalPending, who string) error {
		at := time.Now()
		sig := ed25519.Sign(demoKey("approver "+who), agent.ApprovalDecisionBytes(pend.Subject(), who, true, at))
		out("  %s approves\n", who)
		return agent.SubmitDecision(ctx, j, agent.Decision{RunID: pend.RunID, ToolUseID: pend.ToolUseID, ApproverID: who,
			Approved: true, DecidedAt: at, Alg: audit.AlgEd25519, Signature: sig})
	}

	out("== ops (the maker) requests the refund\n")
	pend, err := run()
	if err != nil || pend == nil {
		return refunds, audit.ApprovalVerdict{}, fmt.Errorf("first drive: %v", err)
	}
	out("== ops approves their own request: recorded, never counted\n")
	if err := sign(pend, "ops"); err != nil {
		return refunds, audit.ApprovalVerdict{}, err
	}
	if _, err := run(); err != nil {
		return refunds, audit.ApprovalVerdict{}, err
	}
	out("== finance and risk (the checkers) approve\n")
	for _, who := range []string{"finance", "risk"} {
		if err := sign(pend, who); err != nil {
			return refunds, audit.ApprovalVerdict{}, err
		}
	}
	if p, err := run(); err != nil || p != nil {
		return refunds, audit.ApprovalVerdict{}, fmt.Errorf("drive at quorum: %v", err)
	}
	out("  refunds issued: %d\n", refunds)

	out("== an auditor verifies offline\n")
	logKey := audit.Ed25519Signer{Priv: demoKey("log operator")}
	th, err := audit.NewTreeHead(ctx, j, runID, time.Now().UnixNano())
	if err != nil {
		return refunds, audit.ApprovalVerdict{}, err
	}
	sth, err := audit.SignTreeHead(th, logKey)
	if err != nil {
		return refunds, audit.ApprovalVerdict{}, err
	}
	acts, err := audit.ApprovalEvidence(ctx, j, runID, pend.ToolUseID, sth)
	if err != nil {
		return refunds, audit.ApprovalVerdict{}, err
	}
	logV := audit.Ed25519Verifier{Pub: demoKey("log operator").Public().(ed25519.PublicKey)}
	v, err := audit.VerifyApprovals(acts, pend.ToolUseID, policy, approverVerifiers(), logV)
	if err != nil {
		return refunds, v, err
	}
	out("  requesters (from the proven run:principal, without the run's input): %s\n", strings.Join(v.Requesters, ", "))
	out("  counted: %s\n", strings.Join(v.Counted, ", "))
	for _, d := range v.Ignored {
		out("  ignored %s: %s\n", d.Approver, d.Reason)
	}
	out("  maker-checker: PASS (%d of %d, the requester excluded)\n", len(v.Counted), v.Need)
	return refunds, v, nil
}

func main() {
	j, err := agent.NewJournal(agent.NewMemStore())
	if err == nil {
		_, _, err = story(context.Background(), j, func(f string, a ...any) { fmt.Printf(f, a...) })
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
