package approvals_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/approvals"
	"github.com/bide-ai/bide/audit"
)

const runID = "run-pay"

// chargeModel calls the "charge" tool until a tool result is in the conversation, then answers.
type chargeModel struct{ id string }

func (m chargeModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	done := false
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if _, ok := p.(agent.ToolResult); ok {
				done = true
			}
		}
	}
	if done {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "charged"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	} else {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: m.id, Name: "charge", ArgsFragment: json.RawMessage(`{"amount":120, "to":"acme"}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// rig is an application: a 2-of-{alice,bob,carol} gated "charge" tool over a journal that an
// audit.AuditedStore signs, with the intake handler mounted on a server.
type rig struct {
	identity agent.Identity // the run's acting identity, when set (separation of duties)
	call     string
	stall    *stallStore
	t        *testing.T
	inner    agent.Store
	log      *audit.MemAnchorLog
	logPub   ed25519.PublicKey
	app      *agent.Journal // the application's: signs what it writes
	privs    map[string]ed25519.PrivateKey
	enrolled map[string]ed25519.PublicKey // dave is enrolled but not on the gate
	policy   agent.ApprovalPolicy
	charged  atomic.Int32
	h        *approvals.Handler
	srv      *httptest.Server
	now      time.Time
	nowMu    sync.Mutex
}

func (r *rig) clock() time.Time {
	r.nowMu.Lock()
	defer r.nowMu.Unlock()
	return r.now
}

func (r *rig) advance(d time.Duration) {
	r.nowMu.Lock()
	r.now = r.now.Add(d)
	r.nowMu.Unlock()
}

func (r *rig) verifiers(id string) (agent.ApproverVerifier, bool) {
	pub, ok := r.enrolled[id]
	if !ok {
		return nil, false
	}
	return audit.Ed25519Verifier{Pub: pub}, true
}

// newRig builds the application and drives its run to the approval pause.
func newRig(t *testing.T, opts ...approvals.Option) *rig {
	t.Helper()
	return newRigWith(t, "c1", nil, opts...)
}

// newRigWith is newRig for a call id, and a gate policy other than the 2-of-3 (nil keeps it).
func newRigWith(t *testing.T, call string, policy *agent.ApprovalPolicy, opts ...approvals.Option) *rig {
	t.Helper()
	return newRigAs(t, call, policy, agent.Identity{}, opts...)
}

// newRigAs is newRigWith for a run acting as identity (its OnBehalfOf is the requester a policy with
// ExcludePrincipal excludes).
func newRigAs(t *testing.T, call string, policy *agent.ApprovalPolicy, id agent.Identity, opts ...approvals.Option) *rig {
	t.Helper()
	r := &rig{t: t, call: call, identity: id, privs: map[string]ed25519.PrivateKey{}, enrolled: map[string]ed25519.PublicKey{},
		policy: agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob", "carol"}},
		now:    time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	for _, id := range []string{"alice", "bob", "carol", "dave"} {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		r.privs[id], r.enrolled[id] = priv, pub
	}
	if policy != nil {
		r.policy = *policy
	}
	var logPriv ed25519.PrivateKey
	r.logPub, logPriv, _ = ed25519.GenerateKey(rand.Reader)
	r.stall = &stallStore{Store: agent.NewMemStore(), entered: make(chan struct{}, 4)}
	r.inner = r.stall
	r.log = audit.NewMemAnchorLog()
	as, err := audit.NewAuditedStore(r.inner, audit.Ed25519Signer{Priv: logPriv}, r.log)
	if err != nil {
		t.Fatal(err)
	}
	r.app = agenttest.MustJournal(as)
	if err := r.run(); !agent.IsPause(err) {
		t.Fatalf("the run did not pause: %v", err)
	}
	opts = append([]approvals.Option{approvals.WithClock(r.clock)}, opts...)
	r.h, err = approvals.New(r.app, r.verifiers, opts...)
	if err != nil {
		t.Fatal(err)
	}
	r.srv = httptest.NewServer(r.h)
	t.Cleanup(r.srv.Close)
	return r
}

// run drives the application's run once.
func (r *rig) run() error {
	charge := agent.MustFunc("charge", "charge the card", func(context.Context, struct {
		Amount int    `json:"amount"`
		To     string `json:"to"`
	}) (string, error) {
		r.charged.Add(1)
		return "ok", nil
	}, agent.WithApproval(&r.policy))
	var ropts []agent.RunOption
	if !r.identity.Empty() {
		ropts = append(ropts, agent.WithIdentity(r.identity))
	}
	_, err := agenttest.MustNew(chargeModel{id: r.call}, r.app, agent.WithTools(charge), agent.WithApproverVerifiers(r.verifiers)).
		Run(context.Background(), runID, agent.UserText("pay"), ropts...)
	return err
}

func (r *rig) subject() agent.ApprovalSubject {
	r.t.Helper()
	recs, err := r.app.History(context.Background(), runID)
	if err != nil {
		r.t.Fatal(err)
	}
	_, call, ok := agent.FindToolCall(recs, r.call)
	if !ok {
		r.t.Fatal("no call c1")
	}
	return agent.ApprovalSubject{RunID: runID, ToolUseID: r.call, ToolName: call.Name, Args: call.Args}
}

// body is the JSON a client posts for approver id's decision, signed over s.
type body struct {
	Run       string `json:"run"`
	Call      string `json:"call"`
	Approver  string `json:"approver"`
	Approved  bool   `json:"approved"`
	DecidedAt string `json:"decided_at"`
	Alg       string `json:"alg"`
	Signature string `json:"signature"`
}

func (r *rig) decision(id string, approved bool) body {
	r.t.Helper()
	return r.decisionOn(r.subject(), id, approved)
}

func (r *rig) decisionOn(s agent.ApprovalSubject, id string, approved bool) body {
	at := time.Date(2026, 10, 13, 12, 0, 1, 0, time.UTC)
	sig := ed25519.Sign(r.privs[id], agent.ApprovalDecisionBytes(s, id, approved, at))
	return body{Run: s.RunID, Call: s.ToolUseID, Approver: id, Approved: approved, DecidedAt: agent.FormatTimestamp(at),
		Alg: string(audit.AlgEd25519), Signature: base64.StdEncoding.EncodeToString(sig)}
}

// post sends v as JSON and returns the status and the decoded body.
func (r *rig) post(v any) (int, map[string]any) {
	r.t.Helper()
	b, _ := json.Marshal(v)
	return r.postRaw(b, "application/json")
}

func (r *rig) postRaw(b []byte, contentType string, hdr ...string) (int, map[string]any) {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/decisions", bytes.NewReader(b))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return r.do(req)
}

func (r *rig) get(path string) (int, map[string]any) {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+path, nil)
	return r.do(req)
}

func (r *rig) do(req *http.Request) (int, map[string]any) {
	r.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (r *rig) records() int {
	r.t.Helper()
	recs, err := r.app.History(context.Background(), runID)
	if err != nil {
		r.t.Fatal(err)
	}
	return len(recs)
}

func code(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func ed25519Sign(r *rig, id string, msg []byte) []byte { return ed25519.Sign(r.privs[id], msg) }

// stallStore blocks Load while armed, to hold a request inside the journal.
type stallStore struct {
	agent.Store
	mu      sync.Mutex
	release chan struct{}
	entered chan struct{}
	preempt *preemption
}

func (s *stallStore) Unwrap() agent.Store { return s.Store }

// arm makes the next Load wait until the returned channel is closed.
func (s *stallStore) arm() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.release = make(chan struct{})
	return s.release
}

// preempt, when set, is called before an Insert of the named entry: a competing writer that records
// the step first, as another process would.
func (s *stallStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	s.mu.Lock()
	p := s.preempt
	if p != nil && p.name == name {
		s.preempt = nil
	} else {
		p = nil
	}
	s.mu.Unlock()
	if p != nil {
		if _, _, err := s.Store.Insert(ctx, runID, name, p.data); err != nil {
			return agent.Entry{}, false, err
		}
	}
	return s.Store.Insert(ctx, runID, name, data)
}

type preemption struct {
	name string
	data []byte
}

func (s *stallStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	s.mu.Lock()
	rel := s.release
	s.release = nil
	s.mu.Unlock()
	if rel != nil {
		s.entered <- struct{}{}
		<-rel
	}
	return s.Store.Load(ctx, runID, after)
}
