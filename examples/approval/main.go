// Command approval shows an m-of-n human approval gate end to end, across separate
// processes, against an on-disk SQLite journal. An agent wants to refund an order; the
// refund tool requires 2 of 3 named approvers (ops, finance, risk), each signing their
// decision with their own key. Then an auditor proves offline, from a portable evidence
// file, that two eligible approvers signed off BEFORE the refund ran.
//
// With no arguments it runs the whole story in one process (no API key, no network):
//
//	go run .
//
// Each actor can also run as its own process against one journal, which is how approval
// works in practice (the run pauses; approvers decide later, elsewhere):
//
//	go run . run      -db approval.db               # drive the agent: pauses, or refunds
//	go run . approve  -db approval.db -as ops       # one approver signs and records a decision
//	go run . approve  -db approval.db -as risk -forge   # a decision with a forged signature
//	go run . evidence -db approval.db -out evidence.json
//	go run . verify   -in evidence.json             # the auditor: public keys only
//
// The evidence file also verifies with the standalone CLI, which checks every inclusion
// proof against the log key:
//
//	bide-audit verify-evidence -evidence evidence.json -pubkey <hex printed by "evidence">
//
// The keys are derived from fixed seeds so separate processes agree on them. That is for the
// demo only: real approvers hold their own private keys, and the verifier's public keys come
// from your PKI.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
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
	"github.com/bide-ai/bide/store/sqlite"
)

const (
	runID  = "refund-42"
	callID = "refund-1" // the tool-use id the scripted model gives its refund call
)

// policy is the gate: 2 of these 3 approvers must sign off.
var policy = agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}

// registered is everyone with a key, including mallory, who has a key but is not eligible.
var registered = []string{"ops", "finance", "risk", "mallory"}

// demoKey derives a fixed ed25519 key from a name. Demo only (see the package comment).
func demoKey(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("bide examples/approval " + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

func logKey() ed25519.PrivateKey { return demoKey("log operator") }

// approverVerifiers resolves an approver id to the public key that verifies their
// signature. The gate uses it at run time and the auditor uses the same shape offline.
func approverVerifiers() agent.ApproverVerifierFor {
	keys := map[string]ed25519.PublicKey{}
	for _, id := range registered {
		keys[id] = demoKey("approver " + id).Public().(ed25519.PublicKey)
	}
	return func(id string) (agent.ApproverVerifier, bool) {
		pub, ok := keys[id]
		if !ok {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: pub}, true
	}
}

// refundModel is a scripted, stateless model: it asks for the refund until a tool result is
// in the conversation, then answers. Stateless, so a resumed run needs no per-run script.
type refundModel struct{}

func (refundModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	if result, ok := toolResult(req.Messages); ok {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "Refund complete: " + result}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	} else {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: callID, Name: "refund", ArgsFragment: json.RawMessage(`{"order":42,"amount":120}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

func toolResult(msgs []agent.Message) (string, bool) {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if tr, ok := p.(agent.ToolResult); ok {
				var s string
				if json.Unmarshal(tr.Result, &s) != nil {
					s = string(tr.Result)
				}
				return s, true
			}
		}
	}
	return "", false
}

type refundArgs struct {
	Order  int `json:"order"`
	Amount int `json:"amount"`
}

// newAgent builds the agent with the approval-gated refund tool. witness, if set, gets one
// line appended per real refund, so a test can count side effects across processes.
func newAgent(store agent.Durable, witness string) *agent.Agent {
	refund := agent.Func("refund", "refund an order",
		agent.Safety{Approval: &policy},
		func(_ context.Context, in refundArgs) (string, error) {
			if witness != "" {
				f, err := os.OpenFile(witness, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err != nil {
					return "", err
				}
				fmt.Fprintf(f, "refunded order %d\n", in.Order)
				if err := f.Close(); err != nil {
					return "", err
				}
			}
			return fmt.Sprintf("$%d to order %d", in.Amount, in.Order), nil
		})
	return agent.New(refundModel{}, store, refund).WithApproverVerifiers(approverVerifiers())
}

// cmdRun drives the agent once. It either pauses at the gate (printing the tally) or completes.
func cmdRun(ctx context.Context, store agent.Durable, witness string) error {
	out, err := newAgent(store, witness).Run(ctx, runID, "Refund order 42.")
	var pend *agent.PendingApproval
	switch {
	case errors.As(err, &pend) && pend.Quorum != nil:
		q := pend.Quorum
		fmt.Println("status: paused")
		fmt.Printf("approved: %d of %d\n", q.Approved, q.Need)
		fmt.Printf("waiting on: %s\n", strings.Join(q.Pending, ", "))
		fmt.Printf("tool call: %s (%s)\n", pend.ToolUseID, pend.ToolName)
		return nil
	case err != nil:
		return err
	}
	fmt.Println("status: done")
	for _, p := range out.Parts {
		if t, ok := p.(agent.Text); ok {
			fmt.Printf("answer: %s\n", t.Text)
		}
	}
	return nil
}

// cmdApprove records one approver's signed decision. forge signs with a key that is not the
// approver's, to show the gate ignores it.
func cmdApprove(ctx context.Context, store agent.Durable, as string, approved, forge bool) error {
	key := demoKey("approver " + as)
	if forge {
		key = demoKey("forger")
	}
	sig := ed25519.Sign(key, agent.ApprovalDecisionBytes(runID, callID, as, approved))
	if err := agent.ApproveAs(ctx, store, runID, callID, as, approved, sig); err != nil {
		return err
	}
	verb := "approved"
	if !approved {
		verb = "denied"
	}
	how := "signed"
	if forge {
		how = "FORGED signature"
	}
	fmt.Printf("recorded: %s %s (%s)\n", as, verb, how)
	return nil
}

// cmdEvidence writes a portable evidence package: the refund's inclusion proof plus one per
// approver decision, all under one signed tree head.
func cmdEvidence(ctx context.Context, store agent.Durable, outPath string) error {
	pkg, err := audit.Evidence(ctx, store, runID, logKey(), time.Now().Unix(), audit.WithToolCall(callID))
	if err != nil {
		return err
	}
	decisions, err := audit.ApprovalEvidence(ctx, store, runID, callID, policy.Approvers, pkg.STH)
	if err != nil {
		return err
	}
	pkg.Actions = append(pkg.Actions, decisions[:len(decisions)-1]...) // the action is already packaged
	b, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(outPath, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote: %s (%d proofs)\n", outPath, len(pkg.Actions))
	fmt.Printf("log public key: %s\n", hex.EncodeToString(logKey().Public().(ed25519.PublicKey)))
	return nil
}

// cmdVerify is the auditor. It reads only the evidence file and public keys: no journal, no
// private keys. It exits non-zero unless the proofs verify AND k eligible approvers signed
// off before the refund.
func cmdVerify(inPath string) error {
	b, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	var pkg audit.EvidencePackage
	if err := json.Unmarshal(b, &pkg); err != nil {
		return err
	}
	logPub := logKey().Public().(ed25519.PublicKey)
	rep, err := pkg.Verify(logPub)
	if err != nil {
		return err
	}
	fmt.Printf("proofs: %s\n", passFail(rep.OK))
	v, err := audit.VerifyApprovals(pkg.Actions, callID, policy, approverVerifiers(), logPub)
	if err != nil {
		return err
	}
	fmt.Printf("counted: %s\n", strings.Join(v.Counted, ", "))
	for _, d := range v.Ignored {
		fmt.Printf("ignored: %s (%s)\n", d.Approver, d.Reason)
	}
	fmt.Printf("k-of-n: %s (%d of %d)\n", passFail(v.OK), len(v.Counted), v.Need)
	if !rep.OK || !v.OK {
		return errors.New("verification failed")
	}
	return nil
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

// demo runs the whole story in one process against a temp journal.
func demo(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "bide-approval-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	store, err := sqlite.Open(filepath.Join(dir, "approval.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	witness := filepath.Join(dir, "refunds.txt")

	step := func(title string, f func() error) error {
		fmt.Printf("\n== %s\n", title)
		return f()
	}
	steps := []struct {
		title string
		f     func() error
	}{
		{"The agent asks to refund; the gate pauses it", func() error { return cmdRun(ctx, store, witness) }},
		{"mallory approves (has a key, but is not an eligible approver)", func() error { return cmdApprove(ctx, store, "mallory", true, false) }},
		{"risk's approval arrives with a forged signature", func() error { return cmdApprove(ctx, store, "risk", true, true) }},
		{"Resume: neither counts", func() error { return cmdRun(ctx, store, witness) }},
		{"ops approves", func() error { return cmdApprove(ctx, store, "ops", true, false) }},
		{"Resume: 1 of 2", func() error { return cmdRun(ctx, store, witness) }},
		{"finance approves", func() error { return cmdApprove(ctx, store, "finance", true, false) }},
		{"Resume: 2 of 2, the refund runs", func() error { return cmdRun(ctx, store, witness) }},
		{"Resume again: nothing runs twice", func() error { return cmdRun(ctx, store, witness) }},
		{"Export evidence", func() error { return cmdEvidence(ctx, store, filepath.Join(dir, "evidence.json")) }},
		{"An auditor verifies offline", func() error { return cmdVerify(filepath.Join(dir, "evidence.json")) }},
	}
	for _, s := range steps {
		if err := step(s.title, s.f); err != nil {
			return err
		}
	}
	n, err := countLines(witness)
	if err != nil {
		return err
	}
	fmt.Printf("\nrefunds actually issued: %d\n", n)
	return nil
}

func countLines(path string) (int, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strings.Count(string(b), "\n"), nil
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
	db := fs.String("db", "approval.db", "sqlite journal path")
	witness := fs.String("witness", "", "file to append one line to per real refund (run)")
	as := fs.String("as", "", "approver id (approve)")
	deny := fs.Bool("deny", false, "record a denial instead of an approval (approve)")
	forge := fs.Bool("forge", false, "sign with a key that is not the approver's (approve)")
	out := fs.String("out", "evidence.json", "evidence file to write (evidence)")
	in := fs.String("in", "evidence.json", "evidence file to verify (verify)")
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
	switch cmd {
	case "run":
		err = cmdRun(ctx, store, *witness)
	case "approve":
		if *as == "" {
			fatal(errors.New("approve: -as is required"))
		}
		err = cmdApprove(ctx, store, *as, !*deny, *forge)
	case "evidence":
		err = cmdEvidence(ctx, store, *out)
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
