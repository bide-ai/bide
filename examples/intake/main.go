// Command intake shows the approval intake: an application mounts approvals.Handler so approvers
// decide on a gated call over HTTP, and the application stays the only writer of its journal and
// the only signer of its audit log.
//
// The application runs a refund agent whose "refund" tool waits at a 2-of-3 gate (ops, finance,
// risk) with separation of duties: ops started the run, so ops never counts. The intake is
// approvals.New with
//
//   - WithGatePolicy: the policy the tool was built with, so the handler counts a gate as the agent
//     does (required);
//   - WithAuth: a hook that authenticates the caller from a bearer token and, once the body names
//     the approver, binds the token to that approver (required when the browser page or CORS is
//     on, and the way to keep arguments away from callers who should not see them);
//   - WithPage: the browser approve page, served from the application at /approvals/page/ (the
//     page says it is in this mode: the stronger setup hosts the page on the approver's own
//     origin, see docs/guides/hitl-approval.md#approving-in-the-browser);
//   - OnRecorded: the application's rule for continuing a run once its gate is decided.
//
// Each approver's client does what bide approve -to does: it fetches the pending call, signs the
// decision over the arguments it showed (agent.ApprovalDecisionBytes), and posts it. The
// intake verifies the signature against the recorded call and the approver's enrolled key, and
// records the decision through the application's journal, which an audit.AuditedStore signs as it
// is written. The example ends by checking, from the receipt the intake returned, that a signed
// tree head of the journal commits to the decision.
//
// It runs with no API key and no network beyond a loopback listener:
//
//	go run ./examples/intake
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/approvals"
	"github.com/bide-ai/bide/audit"
)

const (
	runID  = "refund-1001"
	callID = "call-1"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// approver is a person with a key. The application knows the public key and the bearer token its
// identity provider issued; the private key stays on the approver's side.
type approver struct {
	id    string
	token string
	priv  ed25519.PrivateKey
	pub   ed25519.PublicKey
}

func newApprover(id string) approver {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tok := make([]byte, 8)
	_, _ = rand.Read(tok)
	return approver{id: id, token: fmt.Sprintf("%s-%x", id, tok), priv: priv, pub: pub}
}

func run(w io.Writer) error {
	ctx := context.Background()
	people := map[string]approver{}
	for _, id := range []string{"ops", "finance", "risk"} {
		people[id] = newApprover(id)
	}

	// The application: a journal over a store, signed and anchored as it is written.
	logPub, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	anchor := audit.NewMemAnchorLog()
	signed, err := audit.NewAuditedStore(agent.NewMemStore(), audit.Ed25519Signer{Priv: logPriv}, anchor)
	if err != nil {
		return err
	}
	journal, err := agent.NewJournal(signed)
	if err != nil {
		return err
	}

	// The keys the application enrolled, and the gate the refund tool was built with.
	verifierFor := func(id string) (agent.ApproverVerifier, bool) {
		p, ok := people[id]
		return audit.Ed25519Verifier{Pub: p.pub}, ok
	}
	policy := &agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}, ExcludePrincipal: true}
	var refunds atomic.Int32
	refund, err := agent.Func("refund", "refund a customer", func(_ context.Context, a struct {
		Order  string `json:"order"`
		Amount int    `json:"amount"`
	}) (string, error) {
		refunds.Add(1)
		return fmt.Sprintf("refunded %d on %s", a.Amount, a.Order), nil
	}, agent.WithApproval(policy))
	if err != nil {
		return err
	}
	newAgent := func() (*agent.Agent, error) {
		model := agenttest.NewScriptedModel(
			agenttest.ToolTurn(callID, "refund", `{"order":"A-77","amount":120}`),
			agenttest.TextTurn("refund done"))
		return agent.New(model, journal, agent.WithTools(refund), agent.WithApproverVerifiers(verifierFor))
	}
	drive := func() error {
		a, err := newAgent()
		if err != nil {
			return err
		}
		// ops started the run, so ops is the requester a gate with ExcludePrincipal never counts.
		_, err = a.Run(ctx, runID, agent.UserText("refund order A-77"), agent.WithIdentity(agent.Identity{OnBehalfOf: "ops"}))
		return err
	}
	if err := drive(); !agent.IsPause(err) {
		return fmt.Errorf("the run did not pause at the gate: %v", err)
	}
	fmt.Fprintln(w, "the run paused at the refund gate: 2 of ops, finance, risk, and ops is the requester")

	// The intake.
	decided := make(chan struct{}, 1)
	h, err := approvals.New(journal, verifierFor,
		approvals.WithGatePolicy(func(tool string) (*agent.ApprovalPolicy, bool) {
			if tool == "refund" {
				return policy, true
			}
			return nil, false
		}),
		approvals.WithAuth(bearerAuth(people)),
		approvals.WithPage(),
		approvals.OnRecorded(func(_ context.Context, r approvals.Recorded) {
			if r.Decided {
				select { // the application's rule: continue the run once the gate is settled
				case decided <- struct{}{}:
				default:
				}
			}
		}))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/approvals/", http.StripPrefix("/approvals", h))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base := srv.URL + "/approvals"
	fmt.Fprintln(w, "the intake is mounted at /approvals; the browser page is served at /approvals/page/")

	// What the intake refuses before it looks at a signature.
	code, err := get(base+"/pending?run="+url.QueryEscape(runID)+"&call="+callID, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "no token: HTTP %d\n", code)

	// The approvers decide, each with their own key and token.
	var finance approvals.Response
	for _, id := range []string{"ops", "finance", "risk"} {
		resp, status, err := decide(base, people[id], id, true)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s approves: HTTP %d, counted=%v, gate %d of %d (excluded: %v)\n", id, status, resp.Counted, resp.Gate.Approved, resp.Gate.Need, resp.Gate.Excluded)
		if id == "finance" {
			finance = resp
		}
	}
	// A token is bound to its approver: risk's token cannot post finance's decision.
	_, status, err := decide(base, people["risk"], "finance", true)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "risk's token posting finance's decision: HTTP %d\n", status)

	select {
	case <-decided:
	case <-time.After(5 * time.Second):
		return errors.New("the gate was not decided")
	}
	h.Wait() // the hook has returned; nothing of the handler is still running
	if err := drive(); err != nil {
		return fmt.Errorf("the resumed run: %w", err)
	}
	fmt.Fprintf(w, "the gate passed and the run continued: refund executed %d time\n", refunds.Load())

	// The receipt finance kept is checked against the signed heads of the application's journal.
	if finance.Receipt == nil {
		return errors.New("no receipt in finance's response")
	}
	if err := finance.Receipt.Verify(audit.Ed25519Verifier{Pub: people["finance"].pub}); err != nil {
		return err
	}
	recs, err := journal.History(ctx, runID)
	if err != nil {
		return err
	}
	var heads []audit.SignedTreeHead
	for _, e := range anchor.Entries() {
		if e.RunID == runID {
			heads = append(heads, e.STH)
		}
	}
	f := audit.CheckReceipt(*finance.Receipt, recs, heads, audit.Ed25519Verifier{Pub: logPub})
	fmt.Fprintf(w, "finance's receipt: %s (%d signed heads of the journal)\n", f.Verdict, len(heads))
	if f.Verdict.Failed() || f.Verdict != audit.ReceiptAnchored {
		return fmt.Errorf("receipt not anchored: %s: %s", f.Verdict, f.Detail)
	}
	return nil
}

// bearerAuth authenticates a caller by bearer token and binds the token to the approver a decision
// names. It is called twice for a POST (PhaseIdentity before the body is read, PhaseRequest after),
// so it keeps no state. A real deployment checks tokens its identity provider issued, or a client
// certificate, instead of a table.
func bearerAuth(people map[string]approver) func(*http.Request, approvals.Access) error {
	return func(r *http.Request, a approvals.Access) error {
		tok, ok := bearer(r)
		if !ok {
			return approvals.ErrUnauthenticated
		}
		who := ""
		for id, p := range people {
			if subtle.ConstantTimeCompare([]byte(tok), []byte(p.token)) == 1 {
				who = id
			}
		}
		if who == "" {
			return approvals.ErrUnauthenticated
		}
		if a.Approver != "" && a.Approver != who {
			return approvals.ErrForbidden // a token decides for its own approver only
		}
		return nil
	}
}

func bearer(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return "", false
	}
	return h[len(prefix):], true
}

func get(u, token string) (int, error) {
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// decide is an approver's client: it fetches the pending call, signs over what it fetched and posts
// the decision as approver as (a different id than the token's is refused by the hook). It returns
// the intake's response and status.
func decide(base string, who approver, as string, approved bool) (approvals.Response, int, error) {
	var out approvals.Response
	req, _ := http.NewRequest(http.MethodGet, base+"/pending?run="+url.QueryEscape(runID)+"&call="+url.QueryEscape(callID), nil)
	req.Header.Set("Authorization", "Bearer "+who.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, 0, err
	}
	var p approvals.Pending
	err = json.NewDecoder(resp.Body).Decode(&p)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		return out, resp.StatusCode, fmt.Errorf("GET /pending: %d %v", resp.StatusCode, err)
	}
	subject := agent.ApprovalSubject{RunID: p.Run, ToolUseID: p.Call, ToolName: p.Tool, Args: p.Args}
	if got := audit.ApprovalArgsDigest(subject); got != p.ArgsSHA256 {
		return out, 0, fmt.Errorf("the endpoint's digest %s is not the SHA-256 of the arguments it showed (%s)", p.ArgsSHA256, got)
	}
	at := time.Now().UTC()
	sig := ed25519.Sign(who.priv, agent.ApprovalDecisionBytes(subject, as, approved, at))
	body, _ := json.Marshal(approvals.Request{Run: p.Run, Call: p.Call, Approver: as, Approved: approved,
		DecidedAt: agent.FormatTimestamp(at), Alg: string(audit.AlgEd25519), Signature: sig})
	post, _ := http.NewRequest(http.MethodPost, base+"/decisions", bytes.NewReader(body))
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Authorization", "Bearer "+who.token)
	resp, err = http.DefaultClient.Do(post)
	if err != nil {
		return out, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		err = json.NewDecoder(resp.Body).Decode(&out)
	}
	return out, resp.StatusCode, err
}
