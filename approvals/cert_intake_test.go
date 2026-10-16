package approvals_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/approvals"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// The approvals intake takes a certificate chain in the request (cert, base64 DER, leaf first), verifies it
// with the approver's certificate-bound verifier before writing (agent.WithDecisionCheck), and records it in
// the decision record. Test plan item 22 of docs/design/approver-identity.md.

// certRig is a one-seat gate whose approver "alice" is certificate-bound ("bob" is a bare key), over a journal
// an audit.AuditedStore signs, with the intake mounted.
type certRig struct {
	t      *testing.T
	base   time.Time
	root   *testca.CA
	inter  *testca.CA
	leaf   *testca.Leaf
	status *audit.StatusSnapshot
	log    *audit.MemAnchorLog
	logPub ed25519.PublicKey
	app    *agent.Journal
	srv    *httptest.Server
	bobKey ed25519.PrivateKey
	bobPub ed25519.PublicKey
	appr   *audit.CertApprover
}

func newCertRig(t *testing.T, opts ...approvals.Option) *certRig {
	t.Helper()
	r := &certRig{t: t, base: time.Now().UTC().Truncate(time.Second)}
	r.root = testca.NewRoot(t, "root", testca.Opts{})
	r.inter = r.root.NewIntermediate(t, "inter", testca.Opts{})
	r.leaf = r.inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(c *x509.Certificate) {
		c.NotBefore, c.NotAfter = r.base.Add(-time.Hour), r.base.Add(time.Hour)
		c.URIs = nil
		u, _ := url.Parse(testca.Person(1))
		c.URIs = append(c.URIs, u)
	}})
	r.status = r.cleanStatus()
	ci, err := audit.IdentityPolicy{
		Anchors: []*x509.Certificate{r.root.Cert}, Inter: []audit.SPKIHash{audit.SPKIHashOf(r.inter.Cert)},
		Identity: map[string]string{"alice": testca.Person(1), "bob": testca.Person(2)},
		IDKind:   audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
	}.Compile()
	if err != nil {
		t.Fatal(err)
	}
	var logPriv ed25519.PrivateKey
	r.logPub, logPriv, _ = ed25519.GenerateKey(rand.Reader)
	r.log = audit.NewMemAnchorLog()
	as, err := audit.NewAuditedStore(agent.NewMemStore(), audit.Ed25519Signer{Priv: logPriv}, r.log)
	if err != nil {
		t.Fatal(err)
	}
	r.app = agenttest.MustJournal(as)
	rk, ik := audit.SPKIHashOf(r.root.Cert), audit.SPKIHashOf(r.inter.Cert)
	r.appr = &audit.CertApprover{
		ApproverID: "alice", Policy: ci, LogKey: audit.Ed25519Verifier{Pub: r.logPub},
		Heads: func(run string) ([]audit.SignedTreeHead, error) {
			var out []audit.SignedTreeHead
			for _, e := range r.log.Entries() {
				if e.RunID == run {
					out = append(out, e.STH)
				}
			}
			return out, nil
		},
		Status: r.status,
		StatusParams: audit.StatusParams{CAPeriod: time.Hour, W: time.Minute, MinW: time.Second, CountLive: true, MaxStatusAge: 24 * time.Hour,
			CAPeriodFor:     map[audit.SPKIHash]time.Duration{rk: time.Hour, ik: time.Hour},
			MaxStatusAgeFor: map[audit.SPKIHash]time.Duration{rk: 24 * time.Hour, ik: 2 * time.Hour}},
		ExecStatusAge: 2 * time.Hour, ExecStatusAgeFor: map[audit.SPKIHash]time.Duration{rk: 24 * time.Hour, ik: 2 * time.Hour},
		CAOverlapFor: map[audit.SPKIHash]time.Duration{rk: 0, ik: 0}, PolicyEffectiveFrom: r.base.Add(-24 * time.Hour),
		Now: time.Now, ExecNow: time.Now,
	}
	r.bobPub, r.bobKey, _ = ed25519.GenerateKey(rand.Reader)
	if err := r.run(); !agent.IsPause(err) {
		t.Fatalf("the run did not pause: %v", err)
	}
	opts = append([]approvals.Option{approvals.WithOnError(func(err error) { t.Log(err) }),
		approvals.WithGatePolicy(func(tool string) (*agent.ApprovalPolicy, bool) { return r.policy(), tool == "charge" })}, opts...)
	h, err := approvals.New(r.app, r.verifiers, opts...)
	if err != nil {
		t.Fatal(err)
	}
	r.srv = httptest.NewServer(h)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *certRig) cleanStatus() *audit.StatusSnapshot {
	sn := audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
	for _, ca := range []*testca.CA{r.inter, r.root} {
		if _, err := sn.AddCRL(ca.NewCRL(r.t, testca.CRLOpts{Number: 1, ThisUpdate: r.base.Add(-5 * time.Minute), NextUpdate: r.base.Add(48 * time.Hour)})); err != nil {
			r.t.Fatal(err)
		}
	}
	return sn
}

func (r *certRig) policy() *agent.ApprovalPolicy {
	return &agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}
}

// verifiers resolves alice to her certificate-bound verifier and bob to a bare key.
func (r *certRig) verifiers(id string) (agent.ApproverVerifier, bool) {
	switch id {
	case "alice":
		return r.appr, true
	case "bob":
		return audit.Ed25519Verifier{Pub: r.bobPub}, true
	}
	return nil, false
}

func (r *certRig) run() error {
	charge, err := agent.Func("charge", "charge the card", func(context.Context, struct {
		Amount int    `json:"amount"`
		To     string `json:"to"`
	}) (string, error) {
		return "ok", nil
	}, agent.WithApproval(r.policy()))
	if err != nil {
		return err
	}
	a, err := agent.New(chargeModel{id: "c1"}, r.app, agent.WithTools(charge), agent.WithApproverVerifiers(r.verifiers))
	if err != nil {
		return err
	}
	_, err = a.Run(context.Background(), runID, agent.UserText("pay"))
	return err
}

func (r *certRig) subject() agent.ApprovalSubject {
	r.t.Helper()
	recs, err := r.app.History(context.Background(), runID)
	if err != nil {
		r.t.Fatal(err)
	}
	_, call, ok := agent.FindToolCall(recs, "c1")
	if !ok {
		r.t.Fatal("no call c1")
	}
	return agent.ApprovalSubject{RunID: runID, ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
}

func (r *certRig) records() int {
	r.t.Helper()
	recs, err := r.app.History(context.Background(), runID)
	if err != nil {
		r.t.Fatal(err)
	}
	return len(recs)
}

type certBody struct {
	Run       string   `json:"run"`
	Call      string   `json:"call"`
	Approver  string   `json:"approver"`
	Approved  bool     `json:"approved"`
	DecidedAt string   `json:"decided_at"`
	Alg       string   `json:"alg"`
	Signature string   `json:"signature"`
	Cert      []string `json:"cert,omitempty"`
}

// aliceBody is alice's v4 decision under her chain.
func (r *certRig) aliceBody() certBody {
	at := r.base
	sig := ed25519.Sign(r.leaf.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(r.subject(), "alice", true, at, agent.CertLeafHash(r.leaf.DER)))
	b := certBody{Run: runID, Call: "c1", Approver: "alice", Approved: true, DecidedAt: agent.FormatTimestamp(at), Alg: string(audit.AlgEd25519), Signature: base64.StdEncoding.EncodeToString(sig)}
	for _, c := range r.leaf.Chain() {
		b.Cert = append(b.Cert, base64.StdEncoding.EncodeToString(c))
	}
	return b
}

func (r *certRig) post(v any) (int, http.Header, map[string]any) {
	r.t.Helper()
	b, _ := json.Marshal(v)
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/decisions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, resp.Header, out
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestCertIntake_AValidChainIsRecordedCountedAndReceipted(t *testing.T) {
	r := newCertRig(t)
	before := r.records()
	status, _, out := r.post(r.aliceBody())
	if status != http.StatusOK || out["recorded"] != true {
		t.Fatalf("%d %v", status, out)
	}
	if r.records() != before+1 {
		t.Fatalf("records %d -> %d", before, r.records())
	}
	recs, _ := r.app.History(context.Background(), runID)
	var chain [][]byte
	for _, rec := range recs {
		if agent.IsApprovalDecision(rec, "c1") {
			chain = rec.ApproverCert()
		}
	}
	if len(chain) != 2 || !bytes.Equal(chain[0], r.leaf.DER) {
		t.Fatalf("the journal holds %d certificates", len(chain))
	}
	// The covering head followed the write (the audited store signs it), so the gate counts the decision.
	gate, _ := out["gate"].(map[string]any)
	if out["counted"] != true || gate["passed"] != true {
		t.Fatalf("not counted: %v", out)
	}
	// Counted is not final: the status in hand predates the decision, so the count is a live, provisional
	// one, and the response says so (tier, provisional, basis) beside counted: true.
	decs, _ := gate["decisions"].([]any)
	if len(decs) != 1 {
		t.Fatalf("decisions: %v", gate["decisions"])
	}
	gd, _ := decs[0].(map[string]any)
	if gd["tier"] != agent.TierProvisional || gd["provisional"] != true || gd["basis"] != agent.BasisLiveBeforeDecision {
		t.Fatalf("a count on status that predates the decision is shown as final: %v", gd)
	}
	if out["anchored"] != true {
		t.Fatalf("anchored: %v", out["anchored"])
	}
	// The receipt carries the chain.
	rc, _ := out["receipt"].(map[string]any)
	certs, _ := rc["cert"].([]any)
	if len(certs) != 2 {
		t.Fatalf("receipt cert: %v", rc["cert"])
	}
	// A replay of the same request records nothing more and answers the same.
	before = r.records()
	status, _, out = r.post(r.aliceBody())
	if status != http.StatusOK || out["recorded"] != false || r.records() != before {
		t.Fatalf("replay: %d %v (records %d -> %d)", status, out, before, r.records())
	}
}

func TestCertIntake_PendingShowsTheIdentityRule(t *testing.T) {
	r := newCertRig(t)
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/pending?run="+runID+"&call=c1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Gate struct {
			Identity map[string]string `json:"identity"`
		} `json:"gate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Gate.Identity) != 1 || out.Gate.Identity["alice"] != testca.Person(1) {
		t.Fatalf("identity rule: %v (bob is a bare key and is not listed)", out.Gate.Identity)
	}
}

// Status that cannot be judged: 503, nothing written, retry.
func TestCertIntake_StatusFailureIs503AndWritesNothing(t *testing.T) {
	for name, mk := range map[string]func(*certRig){
		"no status": func(r *certRig) { r.appr.Status = nil },
		"empty status": func(r *certRig) {
			r.appr.Status = audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
		},
		"stale status": func(r *certRig) {
			old := r.base.Add(-10 * time.Hour)
			sn := audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
			for _, ca := range []*testca.CA{r.inter, r.root} {
				if _, err := sn.AddCRL(ca.NewCRL(r.t, testca.CRLOpts{Number: 1, ThisUpdate: old, NextUpdate: old.Add(time.Hour)})); err != nil {
					r.t.Fatal(err)
				}
			}
			r.appr.Status = sn
		},
	} {
		r := newCertRig(t)
		mk(r)
		before := r.records()
		status, hdr, out := r.post(r.aliceBody())
		if status != http.StatusServiceUnavailable || errCode(out) != "status_unavailable" || hdr.Get("Retry-After") == "" {
			t.Fatalf("%s: %d %v", name, status, out)
		}
		if r.records() != before {
			t.Fatalf("%s: a refused request wrote %d records", name, r.records()-before)
		}
	}
}

// Every other failure is 422 (or 400 for size) and writes nothing.
func TestCertIntake_RefusalsWriteNothing(t *testing.T) {
	foreign := testca.NewRoot(t, "other", testca.Opts{})
	big := make([][]byte, agent.MaxDecisionCertChain+1)
	for i := range big {
		big[i] = []byte("x")
	}
	for name, tc := range map[string]struct {
		mut    func(*certRig, *certBody)
		status int
		code   string
	}{
		"bad signature": {func(_ *certRig, b *certBody) {
			sig, _ := base64.StdEncoding.DecodeString(b.Signature)
			sig[0] ^= 0xff
			b.Signature = base64.StdEncoding.EncodeToString(sig)
		}, 422, "invalid_approval"},
		"a v3 signature with the chain": {func(r *certRig, b *certBody) {
			sig := ed25519.Sign(r.leaf.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytes(r.subject(), "alice", true, r.base))
			b.Signature = base64.StdEncoding.EncodeToString(sig)
		}, 422, "invalid_approval"},
		"no chain (a downgrade to a bare signature)": {func(r *certRig, b *certBody) {
			sig := ed25519.Sign(r.leaf.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytes(r.subject(), "alice", true, r.base))
			b.Signature, b.Cert = base64.StdEncoding.EncodeToString(sig), nil
		}, 422, "invalid_approval"},
		"a chain to an unknown anchor": {func(r *certRig, b *certBody) {
			l := foreign.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(c *x509.Certificate) {
				c.NotBefore, c.NotAfter = r.base.Add(-time.Hour), r.base.Add(time.Hour)
				c.URIs = nil
				u, _ := url.Parse(testca.Person(1))
				c.URIs = append(c.URIs, u)
			}})
			sig := ed25519.Sign(l.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(r.subject(), "alice", true, r.base, agent.CertLeafHash(l.DER)))
			b.Signature, b.Cert = base64.StdEncoding.EncodeToString(sig), nil
			for _, c := range l.Chain() {
				b.Cert = append(b.Cert, base64.StdEncoding.EncodeToString(c))
			}
		}, 422, "invalid_approval"},
		"a signature by a key that is not the leaf's": {func(r *certRig, b *certBody) {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			sig := ed25519.Sign(other, agent.ApprovalDecisionBytesCert(r.subject(), "alice", true, r.base, agent.CertLeafHash(r.leaf.DER)))
			b.Signature = base64.StdEncoding.EncodeToString(sig)
		}, 422, "invalid_approval"},
		"too many certificates": {func(_ *certRig, b *certBody) {
			b.Cert = nil
			for _, c := range big {
				b.Cert = append(b.Cert, base64.StdEncoding.EncodeToString(c))
			}
		}, 400, "bad_request"},
		"an oversized certificate": {func(r *certRig, b *certBody) {
			b.Cert = []string{base64.StdEncoding.EncodeToString(make([]byte, agent.MaxDecisionCertDER+1))}
		}, 400, "bad_request"},
		"an empty certificate": {func(_ *certRig, b *certBody) { b.Cert = []string{""} }, 400, "bad_request"},
		"a bare-key approver with a chain": {func(r *certRig, b *certBody) {
			sig := ed25519.Sign(r.bobKey, agent.ApprovalDecisionBytes(r.subject(), "bob", true, r.base))
			b.Approver, b.Signature = "bob", base64.StdEncoding.EncodeToString(sig)
		}, 422, "invalid_approval"},
	} {
		r := newCertRig(t)
		b := r.aliceBody()
		tc.mut(r, &b)
		before := r.records()
		status, _, out := r.post(b)
		if status != tc.status || errCode(out) != tc.code {
			t.Fatalf("%s: %d %v", name, status, out)
		}
		if r.records() != before {
			t.Fatalf("%s: a refused request wrote %d records", name, r.records()-before)
		}
	}
}

// A revoked chain is invalid, not unavailable, and writes nothing.
func TestCertIntake_ARevokedChainIs422(t *testing.T) {
	r := newCertRig(t)
	sn := audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
	for _, x := range []struct {
		ca  *testca.CA
		ent []x509.RevocationListEntry
	}{{r.inter, []x509.RevocationListEntry{testca.Revoked(r.leaf.Cert, r.base.Add(-30*time.Minute), 4)}}, {r.root, nil}} {
		if _, err := sn.AddCRL(x.ca.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: r.base.Add(-5 * time.Minute), NextUpdate: r.base.Add(48 * time.Hour), Entries: x.ent})); err != nil {
			t.Fatal(err)
		}
	}
	r.appr.Status = sn
	before := r.records()
	status, _, out := r.post(r.aliceBody())
	if status != 422 || errCode(out) != "invalid_approval" || r.records() != before {
		t.Fatalf("%d %v (%d records written)", status, out, r.records()-before)
	}
}

// A bare-key approver is unaffected: a decision without a chain is recorded as before.
func TestCertIntake_BareKeyApproverUnchanged(t *testing.T) {
	r := newCertRig(t)
	sig := ed25519.Sign(r.bobKey, agent.ApprovalDecisionBytes(r.subject(), "bob", true, r.base))
	status, _, out := r.post(certBody{Run: runID, Call: "c1", Approver: "bob", Approved: true, DecidedAt: agent.FormatTimestamp(r.base), Alg: string(audit.AlgEd25519),
		Signature: base64.StdEncoding.EncodeToString(sig)})
	if status != http.StatusOK || out["recorded"] != true || out["counted"] != true {
		t.Fatalf("%d %v", status, out)
	}
}

// A bare-key decision's response carries no tier and no anchored member.
func TestCertIntake_BareKeyResponseHasNoIdentityMembers(t *testing.T) {
	r := newCertRig(t)
	sig := ed25519.Sign(r.bobKey, agent.ApprovalDecisionBytes(r.subject(), "bob", true, r.base))
	_, _, out := r.post(certBody{Run: runID, Call: "c1", Approver: "bob", Approved: true, DecidedAt: agent.FormatTimestamp(r.base), Alg: string(audit.AlgEd25519),
		Signature: base64.StdEncoding.EncodeToString(sig)})
	if _, ok := out["anchored"]; ok {
		t.Fatalf("anchored on a bare-key response: %v", out)
	}
	gate, _ := out["gate"].(map[string]any)
	for _, d := range gate["decisions"].([]any) {
		m := d.(map[string]any)
		if _, ok := m["tier"]; ok {
			t.Fatalf("tier on a bare-key decision: %v", m)
		}
	}
}

// A replay of a recorded decision during a status outage is answered from the record (200, recorded false),
// not 503: the decision is held, and a client retrying ends.
func TestCertIntake_ReplayDuringAStatusOutageIsAnswered(t *testing.T) {
	r := newCertRig(t)
	if status, _, out := r.post(r.aliceBody()); status != 200 || out["recorded"] != true {
		t.Fatalf("%d %v", status, out)
	}
	r.appr.Status = nil
	before := r.records()
	status, _, out := r.post(r.aliceBody())
	if status != http.StatusOK || out["recorded"] != false || r.records() != before {
		t.Fatalf("%d %v (records %d -> %d)", status, out, before, r.records())
	}
	// A replay that carries a different chain for the same leaf is the same decision and the receipt is of
	// the journaled record (its chain), not of the request's.
	b := r.aliceBody()
	b.Cert = b.Cert[:1]
	status, _, out = r.post(b)
	rc, _ := out["receipt"].(map[string]any)
	if status != http.StatusOK || out["recorded"] != false || len(rc["cert"].([]any)) != 2 {
		t.Fatalf("%d %v", status, out)
	}
}

// After the write the intake asks the store to anchor, and a failing anchor is not an error to the client:
// the decision is recorded, and the response says whether a head covers it.
type reanchorSpy struct {
	agent.Store
	calls int
	err   error
}

func (s *reanchorSpy) Unwrap() agent.Store { return s.Store }
func (s *reanchorSpy) Reanchor(context.Context, string) error {
	s.calls++
	return s.err
}

func TestCertIntake_AnchorsAfterTheWriteAndSaysWhenNotCovered(t *testing.T) {
	r := newCertRig(t)
	spy := &reanchorSpy{Store: agent.NewMemStore(), err: context.DeadlineExceeded}
	j, err := agent.NewJournal(spy)
	if err != nil {
		t.Fatal(err)
	}
	// A journal whose store can anchor (and fails to) and publishes no head the verifier can find.
	r.app = j
	r.appr.Heads = func(string) ([]audit.SignedTreeHead, error) { return nil, nil }
	if err := r.run(); !agent.IsPause(err) {
		t.Fatalf("%v", err)
	}
	h, err := approvals.New(j, r.verifiers, approvals.WithOnError(func(error) {}),
		approvals.WithGatePolicy(func(string) (*agent.ApprovalPolicy, bool) { return r.policy(), true }))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	r.srv = srv
	status, _, out := r.post(r.aliceBody())
	if status != http.StatusOK || out["recorded"] != true || spy.calls != 1 {
		t.Fatalf("%d %v (anchor calls %d)", status, out, spy.calls)
	}
	if out["anchored"] != false || out["counted"] != false || out["reason"] != agent.ReasonNoCoveringHead {
		t.Fatalf("a decision no head covers is presented as covered: %v", out)
	}
	// A bare-key decision does not ask the store to anchor.
	sig := ed25519.Sign(r.bobKey, agent.ApprovalDecisionBytes(r.subject(), "bob", true, r.base))
	r.post(certBody{Run: runID, Call: "c1", Approver: "bob", Approved: true, DecidedAt: agent.FormatTimestamp(r.base), Alg: string(audit.AlgEd25519), Signature: base64.StdEncoding.EncodeToString(sig)})
	if spy.calls != 1 {
		t.Fatalf("a bare-key decision made %d anchor calls", spy.calls)
	}
}

// Gate.Identity lists person identifiers: it is behind the same authorization as the gate read, and a client
// the hook denies learns none.
func TestCertIntake_IdentityRuleIsBehindTheGateReadAuthorization(t *testing.T) {
	deny := func(r *http.Request, a approvals.Access) error {
		if a.Method == http.MethodGet && a.RunID != "" {
			return approvals.ErrForbidden
		}
		return nil
	}
	r := newCertRig(t, approvals.WithAuth(deny))
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/pending?run="+runID+"&call=c1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusForbidden || bytes.Contains(buf.Bytes(), []byte(testca.Person(1))) {
		t.Fatalf("%d %s", resp.StatusCode, buf.String())
	}
}

// A request that repeats a recorded decision's public fields with a flipped signature byte is refused, with
// no receipt, whether or not status is available; only the exact bytes (or a valid signature) are a replay.
func TestCertIntake_AReplayWithAFlippedSignatureIsRefused(t *testing.T) {
	for _, outage := range []bool{false, true} {
		r := newCertRig(t)
		if status, _, out := r.post(r.aliceBody()); status != 200 || out["recorded"] != true {
			t.Fatalf("%d %v", status, out)
		}
		if outage {
			r.appr.Status = nil
		}
		b := r.aliceBody()
		sig, _ := base64.StdEncoding.DecodeString(b.Signature)
		sig[0] ^= 0xff
		b.Signature = base64.StdEncoding.EncodeToString(sig)
		before := r.records()
		status, _, out := r.post(b)
		if status == http.StatusOK || out["receipt"] != nil || r.records() != before {
			t.Fatalf("outage=%v: a flipped signature was answered: %d %v", outage, status, out)
		}
		if status != 422 && status != 503 {
			t.Fatalf("outage=%v: %d %v", outage, status, out)
		}
	}
}
