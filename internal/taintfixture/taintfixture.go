// Package taintfixture builds the journal of a run that read untrusted content and then called a tool,
// so the packages that read gates (the run viewer, bide approve, the notifier, the audit verifiers) can
// probe the escalation instance of a call (docs/design/untrusted-content-and-amount-caps.md, section 5.3)
// through the exported taint option. It is for tests.
package taintfixture

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// Run is the ID of the fixture's run.
const Run = "run-taint"

// F is a run with an untrusted read (c1) and a write (c2) whose tool has its own gate (1 of own1), under
// a taint policy whose escalation gate is 2 of {esc1, esc2}.
type F struct {
	T       *testing.T
	Journal *agent.Journal
	Own     *agent.ApprovalPolicy
	Esc     *agent.ApprovalPolicy
	Priv    map[string]ed25519.PrivateKey
	Pub     map[string]ed25519.PublicKey
	LogPriv ed25519.PrivateKey
	LogPub  ed25519.PublicKey
	Wrote   int
	// At is when the fixture's decisions are signed.
	At time.Time
}

// New returns a fixture over a signed journal. own is the write tool's own gate, nil for none.
func New(t *testing.T, own *agent.ApprovalPolicy) *F {
	t.Helper()
	f := &F{T: t, Own: own, Esc: &agent.ApprovalPolicy{Need: 2, Approvers: []string{"esc1", "esc2"}},
		Priv: map[string]ed25519.PrivateKey{}, Pub: map[string]ed25519.PublicKey{}, At: time.Date(2026, 10, 24, 12, 0, 1, 0, time.UTC)}
	for _, id := range []string{"own1", "esc1", "esc2", "other"} {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		f.Priv[id], f.Pub[id] = priv, pub
	}
	f.LogPub, f.LogPriv, _ = ed25519.GenerateKey(rand.Reader)
	as, err := audit.NewAuditedStore(agent.NewMemStore(), audit.Ed25519Signer{Priv: f.LogPriv}, audit.NewMemAnchorLog())
	if err != nil {
		t.Fatal(err)
	}
	if f.Journal, err = agent.NewJournal(as); err != nil {
		t.Fatal(err)
	}
	return f
}

// Verifiers resolves the fixture's approvers.
func (f *F) Verifiers(id string) (agent.ApproverVerifier, bool) {
	pub, ok := f.Pub[id]
	if !ok {
		return nil, false
	}
	return audit.Ed25519Verifier{Pub: pub}, true
}

type model struct{}

func (model) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	results := map[string]bool{}
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if tr, ok := p.(agent.ToolResult); ok {
				results[tr.ToolUseID] = true
			}
		}
	}
	ch := make(chan agent.Emit, 2)
	switch {
	case !results["c1"]:
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "read", ArgsFragment: json.RawMessage(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	case !results["c2"]:
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c2", Name: "write", ArgsFragment: json.RawMessage(`{"amount":120,"to":"acme"}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	default:
		ch <- agent.Emit{Event: agent.TextDelta{Text: "done"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// Drive drives the run once.
func (f *F) Drive() error {
	f.T.Helper()
	read := agent.MustFunc("read", "read", func(context.Context, struct{}) (string, error) { return "text", nil },
		agent.WithSafety(agent.Safety{ReadOnly: true}), agent.Untrusted("crm"))
	var wopts []agent.ToolOption
	if f.Own != nil {
		wopts = append(wopts, agent.WithApproval(f.Own))
	}
	write := agent.MustFunc("write", "write", func(context.Context, struct {
		Amount int    `json:"amount"`
		To     string `json:"to"`
	}) (string, error) {
		f.Wrote++
		return "ok", nil
	}, wopts...)
	a, err := agent.New(model{}, f.Journal, agent.WithTools(read, write), agent.WithApproverVerifiers(f.Verifiers),
		agent.WithTaintPolicy(agent.TaintPolicy{Gate: f.Esc}))
	if err != nil {
		f.T.Fatal(err)
	}
	_, err = a.Run(context.Background(), Run, agent.UserText("pay"))
	return err
}

// Subject is the subject of call (the call, or its "#taint" instance) as the journal records it.
func (f *F) Subject(call string) agent.ApprovalSubject {
	f.T.Helper()
	recs, err := f.Journal.History(context.Background(), Run)
	if err != nil {
		f.T.Fatal(err)
	}
	_, tu, ok := agent.FindToolCall(recs, call)
	if !ok {
		f.T.Fatalf("no call %s", call)
	}
	return agent.ApprovalSubject{RunID: Run, ToolUseID: call, ToolName: tu.Name, Args: tu.Args}
}

// Decision is approver id's decision on subject call, signed over the subject signedFor.
func (f *F) Decision(call, signedFor, id string, approved bool) agent.Decision {
	f.T.Helper()
	return agent.Decision{RunID: Run, ToolUseID: call, ApproverID: id, Approved: approved, DecidedAt: f.At, Alg: audit.AlgEd25519,
		Signature: ed25519.Sign(f.Priv[id], agent.ApprovalDecisionBytes(f.Subject(signedFor), id, approved, f.At))}
}

// Submit records approver id's decision on call, signed for signedFor, checking its signature as the
// approvals intake does (agent.WithDecisionCheck): a decision signed for another subject is refused.
func (f *F) Submit(call, signedFor, id string, approved bool) error {
	f.T.Helper()
	return agent.SubmitDecision(context.Background(), f.Journal, f.Decision(call, signedFor, id, approved), agent.WithDecisionCheck(f.Verifiers))
}

// SubmitUnchecked records the decision without checking its signature, as a writer of the journal could:
// the tally and the recount must still not count it.
func (f *F) SubmitUnchecked(call, signedFor, id string, approved bool) error {
	f.T.Helper()
	return agent.SubmitDecision(context.Background(), f.Journal, f.Decision(call, signedFor, id, approved))
}
