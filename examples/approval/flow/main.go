// Command flow shows an m-of-n approval gate on a plan flow node, loaded from a declarative config
// (refund.json): lookup -> refund -> notify, where refund needs 2 of 3 named approvers (ops,
// finance, risk) within 48 hours (`timeout`), after which the gate expires and the refund
// never runs. The flow pauses before refund runs, exactly as a gated agent tool call does;
// approvers sign the node and its input with their own keys; the refund runs once at 2 of 3; and an
// auditor verifies the decisions offline. It uses the same approver keys as the agent example
// beside it (../main.go), and the same predictable demo keys warning applies.
//
// With no arguments it runs the whole story in one process:
//
//	go run ./flow
//
// Each actor can also run as its own process against one journal:
//
//	go run ./flow run      -db flow.db             # drive the flow: pauses, or refunds
//	go run ./flow approve  -db flow.db -as ops     # one approver signs and records a decision
//	go run ./flow approve  -db flow.db -as risk -deny
//	go run ./flow evidence -db flow.db -out flow-evidence.json
//	go run ./flow verify   -in flow-evidence.json
//
// The paused run and its gate, in the run viewer (go install github.com/bide-ai/bide/cmd/bide@latest):
//
//	bide view -db flow.db
//
// The standalone CLI verifies the evidence file; -call is the node's key:
//
//	bide audit verify-approvals -evidence flow-evidence.json -pubkey <log key hex> -call node:refund \
//	    -need 2 -approvers ops,finance,risk -approver-keys approver-keys.json
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/plan"
	"github.com/bide-ai/bide/store/sqlite"
)

//go:embed refund.json
var config []byte

const (
	runID   = "refund-flow-42"
	gateKey = "node:refund" // the gated node's key: the pause's ToolUseID
)

// policy is the gate refund.json declares, as the auditor expects it.
var policy = agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}, Timeout: 48 * time.Hour}

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
	Note   string `json:"note"`
}

// demoKey derives a fixed ed25519 key from a name, as ../main.go does. Demo only: PREDICTABLE KEYS,
// NOT SECRETS. Real approvers hold their own randomly generated keys.
func demoKey(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("bide examples/approval " + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

func logKey() audit.Signer { return audit.Ed25519Signer{Priv: demoKey("log operator")} }

func logVerifier() audit.Verifier {
	return audit.Ed25519Verifier{Pub: demoKey("log operator").Public().(ed25519.PublicKey)}
}

func approverVerifiers() agent.ApproverVerifierFor {
	return func(id string) (agent.ApproverVerifier, bool) {
		if !strings.Contains(" ops finance risk ", " "+id+" ") {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: demoKey("approver " + id).Public().(ed25519.PublicKey)}, true
	}
}

// loadFlow loads refund.json against the registered blocks. refunds counts the refunds issued.
func loadFlow(refunds *int) (*plan.Flow[Order, Receipt], error) {
	reg := plan.NewRegistry()
	errs := []error{
		reg.RegisterStep("lookup", func(_ context.Context, o Order) (Refund, error) {
			return Refund{Order: o.ID, Amount: 120}, nil
		}, plan.ReadOnly()),
		// The refund is a side effect: it halts rather than repeat after an ambiguous crash.
		reg.RegisterStep("refund", func(_ context.Context, r Refund) (Refund, error) {
			*refunds++
			fmt.Printf("  refund issued: %d on %s\n", r.Amount, r.Order)
			return r, nil
		}),
		reg.RegisterStep("notify", func(_ context.Context, r Refund) (Receipt, error) {
			return Receipt{Order: r.Order, Amount: r.Amount, Note: "refunded"}, nil
		}, plan.ReadOnly()),
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return plan.Load[Order, Receipt](config, reg, plan.WithLoadedApproverVerifiers(approverVerifiers()))
}

func cmdRun(ctx context.Context, j *agent.Journal, refunds *int) error {
	flow, err := loadFlow(refunds)
	if err != nil {
		return err
	}
	out, err := flow.Run(ctx, j, runID, Order{ID: "order-42"})
	var pend *agent.ApprovalPending
	switch {
	case errors.As(err, &pend):
		q := pend.Quorum
		fmt.Printf("  paused before %s (%s) with input %s: %d of %d approved, %d needed; waiting on %s\n",
			pend.ToolName, pend.ToolUseID, pend.Args, q.Approved, len(q.Approvers), q.Need, strings.Join(q.Pending, ", "))
		return nil
	case errors.Is(err, plan.ErrApprovalExpired):
		// checked first: an expiry also wraps ErrApprovalDenied
		fmt.Printf("  expired: %v\n", err)
		return nil
	case errors.Is(err, plan.ErrApprovalDenied):
		fmt.Printf("  denied: %v\n", err)
		return nil
	case err != nil:
		return err
	}
	fmt.Printf("  done: %+v\n", out)
	return nil
}

// cmdApprove signs the recorded request (the node and its input) as approver as.
func cmdApprove(ctx context.Context, j *agent.Journal, as string, approved bool) error {
	recs, err := j.History(ctx, runID)
	if err != nil {
		return err
	}
	_, call, ok := agent.FindToolCall(recs, gateKey)
	if !ok {
		return errors.New("the run has not reached the gate yet: run it first")
	}
	subject := agent.ApprovalSubject{RunID: runID, ToolUseID: gateKey, ToolName: call.Name, Args: call.Args}
	decidedAt := time.Now() // when the approver decided, by their clock: signed with the decision
	sig := ed25519.Sign(demoKey("approver "+as), agent.ApprovalDecisionBytes(subject, as, approved, decidedAt))
	d := agent.Decision{RunID: runID, ToolUseID: gateKey, ApproverID: as, Approved: approved, DecidedAt: decidedAt, Alg: audit.AlgEd25519, Signature: sig}
	if err := agent.SubmitDecision(ctx, j, d, agent.WithDecisionCheck(approverVerifiers())); err != nil {
		return err
	}
	verb := "approved"
	if !approved {
		verb = "denied"
	}
	fmt.Printf("  %s %s %s(%s)\n", as, verb, call.Name, call.Args)
	return nil
}

func cmdEvidence(ctx context.Context, j *agent.Journal, outPath, keysPath string) error {
	pkg, err := audit.Evidence(ctx, j, runID, logKey(), time.Now().UnixNano())
	if err != nil {
		return err
	}
	acts, err := audit.ApprovalEvidence(ctx, j, runID, gateKey, pkg.STH)
	if err != nil {
		return err
	}
	pkg.Actions = append(pkg.Actions, acts...)
	if err := pkg.Seal(logKey()); err != nil {
		return err
	}
	b, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(outPath, b, 0o644); err != nil {
		return err
	}
	keys := map[string]string{}
	for _, id := range policy.Approvers {
		keys[id] = "ed25519:" + hex.EncodeToString(demoKey("approver "+id).Public().(ed25519.PublicKey))
	}
	kb, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(keysPath, kb, 0o644); err != nil {
		return err
	}
	fmt.Printf("  wrote %s and %s (log key ed25519:%s)\n", outPath, keysPath, hex.EncodeToString(demoKey("log operator").Public().(ed25519.PublicKey)))
	return nil
}

func cmdVerify(inPath string) error {
	b, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	var pkg audit.EvidencePackage
	if err := json.Unmarshal(b, &pkg); err != nil {
		return err
	}
	v, err := audit.VerifyApprovals(pkg.Actions, gateKey, policy, approverVerifiers(), logVerifier())
	if err != nil && !errors.Is(err, audit.ErrNotVerified) {
		return err
	}
	fmt.Printf("  node: %s %s\n  counted: %s\n", v.ToolName, v.Args, strings.Join(v.Counted, ", "))
	for _, p := range v.Problems {
		fmt.Printf("  problem: %s\n", p)
	}
	if !v.OK {
		return errors.New("verification failed")
	}
	fmt.Printf("  k-of-n: PASS (%d of %d)\n", len(v.Counted), v.Need)
	return nil
}

func demo(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "bide-approval-flow-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	store, err := sqlite.Open(filepath.Join(dir, "flow.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	j, err := agent.NewJournal(store)
	if err != nil {
		return err
	}
	refunds := 0
	steps := []struct {
		title string
		f     func() error
	}{
		{"The flow reaches the gated refund node and pauses", func() error { return cmdRun(ctx, j, &refunds) }},
		{"ops approves", func() error { return cmdApprove(ctx, j, "ops", true) }},
		{"Resume: 1 of 2", func() error { return cmdRun(ctx, j, &refunds) }},
		{"risk approves", func() error { return cmdApprove(ctx, j, "risk", true) }},
		{"Resume: 2 of 2, the refund runs and the flow finishes", func() error { return cmdRun(ctx, j, &refunds) }},
		{"Resume again: nothing runs twice", func() error { return cmdRun(ctx, j, &refunds) }},
		{"Export evidence", func() error {
			return cmdEvidence(ctx, j, filepath.Join(dir, "flow-evidence.json"), filepath.Join(dir, "approver-keys.json"))
		}},
		{"An auditor verifies offline", func() error { return cmdVerify(filepath.Join(dir, "flow-evidence.json")) }},
	}
	for _, s := range steps {
		fmt.Printf("\n== %s\n", s.title)
		if err := s.f(); err != nil {
			return err
		}
	}
	fmt.Printf("\nrefunds actually issued: %d\n", refunds)
	return nil
}

func main() {
	ctx := context.Background()
	if len(os.Args) < 2 {
		if err := demo(ctx); err != nil {
			fatal(err)
		}
		return
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	db := fs.String("db", "flow.db", "sqlite journal path")
	as := fs.String("as", "", "approver id (approve)")
	deny := fs.Bool("deny", false, "record a denial instead of an approval (approve)")
	out := fs.String("out", "flow-evidence.json", "evidence file to write (evidence)")
	keysOut := fs.String("keys-out", "approver-keys.json", "approver public keys file to write (evidence)")
	in := fs.String("in", "flow-evidence.json", "evidence file to verify (verify)")
	_ = fs.Parse(args)
	if cmd == "verify" {
		if err := cmdVerify(*in); err != nil {
			fatal(err)
		}
		return
	}
	store, err := sqlite.Open(*db)
	if err != nil {
		fatal(err)
	}
	defer store.Close()
	j, err := agent.NewJournal(store)
	if err != nil {
		fatal(err)
	}
	refunds := 0
	switch cmd {
	case "run":
		err = cmdRun(ctx, j, &refunds)
	case "approve":
		if *as == "" {
			fatal(errors.New("approve: -as is required"))
		}
		err = cmdApprove(ctx, j, *as, !*deny)
	case "evidence":
		err = cmdEvidence(ctx, j, *out, *keysOut)
	default:
		err = fmt.Errorf("unknown command %q (want run, approve, evidence, or verify)", cmd)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
