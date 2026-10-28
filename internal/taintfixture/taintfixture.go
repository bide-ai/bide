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

// WithCapReason returns a copy of the fixture's journal in which the gate-reason record of the write call (c2)
// (written if the call has none yet) also holds a hand-written cap reason: rule "refund_one" gating a call of 6,000.00 USD over a limit of
// 5,000.00 USD. A cap gate does not exist yet, so the engine writes no such record; the record format is
// the one it will write, and this lets the readers of it be tested before the engine writes it.
func (f *F) WithCapReason() *agent.Journal {
	f.T.Helper()
	ctx := context.Background()
	ms := agent.NewMemStore()
	cap := map[string]any{"kind": "cap", "rule": "refund_one", "action": "gate", "why": "over", "currency": "USD", "amount": 600000, "limit": 500000}
	found := false
	for e, err := range f.Journal.Store().Load(ctx, Run, -1) {
		if err != nil {
			f.T.Fatal(err)
		}
		data := e.Data
		if e.Name == "gate-reason:c2" {
			found = true
			var rec map[string]any
			if err := json.Unmarshal(data, &rec); err != nil {
				f.T.Fatal(err)
			}
			body := rec["result"].(map[string]any)
			body["reasons"] = append(body["reasons"].([]any), cap)
			data, _ = json.Marshal(rec)
		}
		if _, _, err := ms.Insert(ctx, Run, e.Name, data); err != nil {
			f.T.Fatal(err)
		}
	}
	if !found { // the call's own gate still waits: the engine has not yet written a record for it
		body, _ := json.Marshal(map[string]any{"format": "bide.gate-reason.v1", "tool_use_id": "c2", "reasons": []any{cap}, "recorded_at": "2026-10-24T10:43:00.000Z"})
		data, _ := json.Marshal(agent.Record{Kind: agent.StepValue, Name: "gate-reason:c2", Result: body})
		if _, _, err := ms.Insert(ctx, Run, "gate-reason:c2", data); err != nil {
			f.T.Fatal(err)
		}
	}
	j, err := agent.NewJournal(ms)
	if err != nil {
		f.T.Fatal(err)
	}
	return j
}

// Separated returns a copy of j in which the gate-reason record of call (written by the engine) also says that the
// taint policy separates approvers (SeparateApprovers), so the readers of a journal can be tested on an escalation
// instance that leaves out the approvers its call's own gate counted. The engine refuses a policy whose Gate shares
// an approver with a tool's own gate, so such a run cannot be made by driving it: the record is edited, as
// WithCapReason does for a cap reason.
func Separated(t *testing.T, j *agent.Journal, run, call string) *agent.Journal {
	t.Helper()
	ctx := context.Background()
	ms := agent.NewMemStore()
	found := false
	for e, err := range j.Store().Load(ctx, run, -1) {
		if err != nil {
			t.Fatal(err)
		}
		data := e.Data
		if e.Name == "gate-reason:"+call {
			found = true
			var rec map[string]any
			if err := json.Unmarshal(data, &rec); err != nil {
				t.Fatal(err)
			}
			rec["result"].(map[string]any)["separate_approvers"] = true
			data, _ = json.Marshal(rec)
		}
		if _, _, err := ms.Insert(ctx, run, e.Name, data); err != nil {
			t.Fatal(err)
		}
	}
	if !found {
		t.Fatalf("no gate-reason record for %s in run %s", call, run)
	}
	nj, err := agent.NewJournal(ms)
	if err != nil {
		t.Fatal(err)
	}
	return nj
}
