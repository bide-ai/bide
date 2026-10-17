package ssocerts

// The recipe's second half, against the same containers: the CRL step-ca publishes is fetched by the real
// `bide identity fetch-crl`, a gated call is approved with `bide approve -cert` and counted by a real
// audit.CertApprover, a CA operator revokes the certificate, the next CRL is fetched, and the decision made
// with the revoked certificate is refused.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/policyfile"
	"github.com/bide-ai/bide/internal/testca"
	"github.com/bide-ai/bide/store/sqlite"
)

// gate is a refund tool that needs ops's approval, run on a SQLite journal, with a real CertApprover
// whose status is what bide identity fetch-crl archived.
type gate struct {
	r       *rig
	t       *testing.T
	j       *agent.Journal
	store   *sqlite.Store
	db      string
	ci      *audit.CertIdentity
	logPub  ed25519.PublicKey
	logKey  audit.Ed25519Signer
	heads   map[string][]audit.SignedTreeHead
	archive string
	charged int
}

func newGate(t *testing.T, r *rig, archive string) *gate {
	t.Helper()
	g := &gate{r: r, t: t, db: filepath.Join(r.dir, "app.db"), heads: map[string][]audit.SignedTreeHead{}, archive: archive}
	var err error
	if g.store, err = sqlite.Open(g.db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.store.Close() })
	if g.j, err = agent.NewJournal(g.store); err != nil {
		t.Fatal(err)
	}
	if g.ci, err = policyfile.Read(r.policy); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	g.logPub, g.logKey = pub, audit.Ed25519Signer{Priv: priv}
	return g
}

// status loads the CRLs bide identity fetch-crl archived into a snapshot.
func (g *gate) status() *audit.StatusSnapshot {
	g.t.Helper()
	sn := audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
	err := filepath.WalkDir(g.archive, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".crl") {
			return err
		}
		der, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		_, err = sn.AddCRL(der)
		return err
	})
	if err != nil {
		g.t.Fatal(err)
	}
	return sn
}

// verifier is the gate's approver for ops, declared with the numbers of ca-timings.json.
func (g *gate) verifier(run string, status *audit.StatusSnapshot, now func() time.Time) agent.ApproverVerifierFor {
	rk, ik := audit.SPKIHashOf(g.r.root.Cert), audit.SPKIHashOf(g.r.inter.Cert)
	return func(id string) (agent.ApproverVerifier, bool) {
		if id != "ops" {
			return nil, false
		}
		return &audit.CertApprover{ApproverID: id, Policy: g.ci, LogKey: audit.Ed25519Verifier{Pub: g.logPub},
			Heads:  func(string) ([]audit.SignedTreeHead, error) { return g.heads[run], nil },
			Status: status, Now: now, ExecNow: now, ExecStatusAge: 30 * time.Minute, PolicyEffectiveFrom: time.Now().Add(-time.Hour),
			StatusParams: audit.StatusParams{CAPeriod: 10 * time.Minute, W: 5 * time.Second, MinW: time.Second, CountLive: true, MaxStatusAge: 30 * time.Minute,
				CAPeriodFor:     map[audit.SPKIHash]time.Duration{rk: 24 * time.Hour, ik: 10 * time.Minute},
				MaxStatusAgeFor: map[audit.SPKIHash]time.Duration{rk: 96 * time.Hour, ik: 30 * time.Minute}},
			ExecStatusAgeFor: map[audit.SPKIHash]time.Duration{rk: 96 * time.Hour, ik: 30 * time.Minute},
			CAOverlapFor:     map[audit.SPKIHash]time.Duration{rk: 0, ik: 0}}, true
	}
}

// drive runs the refund agent for the run. It returns the run's error: an *agent.ApprovalPending while the
// gate waits.
func (g *gate) drive(run string, status *audit.StatusSnapshot) error {
	g.t.Helper()
	policy := agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}
	tool := agent.MustFunc("refund", "refund an order to the customer's card", func(context.Context, json.RawMessage) (string, error) {
		g.charged++
		return "refunded", nil
	}, agent.WithApproval(&policy))
	model := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{"order":"1049","amount":4800,"currency":"USD"}`), agenttest.TextTurn("Refunded."))
	a, err := agent.New(model, g.j, agent.WithTools(tool), agent.WithApproverVerifiers(g.verifier(run, status, time.Now)))
	if err != nil {
		g.t.Fatal(err)
	}
	_, err = a.Run(context.Background(), run, agent.UserText("Refund order 1049."))
	return err
}

// anchor signs a head over the run's journal now, as the application's log does.
func (g *gate) anchor(run string) {
	g.t.Helper()
	th, err := audit.NewTreeHead(context.Background(), g.j, run, time.Now().UnixNano())
	if err != nil {
		g.t.Fatal(err)
	}
	h, err := audit.SignTreeHead(th, g.logKey)
	if err != nil {
		g.t.Fatal(err)
	}
	g.heads[run] = append(g.heads[run], h)
}

// checks is what the gate's counting rule says about each decision of the run.
func (g *gate) checks(run string, status *audit.StatusSnapshot) []agent.DecisionCheck {
	g.t.Helper()
	recs, err := g.j.History(context.Background(), run)
	if err != nil {
		g.t.Fatal(err)
	}
	_, call, ok := agent.FindToolCall(recs, "c1")
	if !ok {
		g.t.Fatal("no call c1")
	}
	subj := agent.ApprovalSubject{RunID: run, ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
	_, checks := agent.TallyApprovals(recs, subj, agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, g.verifier(run, status, time.Now))
	return checks
}

// approve runs the real bide approve -cert for ops and returns its output and error.
func (g *gate) approve(run, key, chain string, extra ...string) (string, error) {
	args := append([]string{"approve", "-db", g.db, "-run", run, "-call", "c1", "-as", "ops", "-key", key, "-cert", chain,
		"-identity-policy", g.r.policy, "-yes", "-color", "never"}, extra...)
	return tryRun(nil, g.r.dir, g.r.bide, args...)
}

func (r *rig) fetchCRLs(archive string) string {
	r.t.Helper()
	out, err := tryRun(nil, r.dir, r.bide, "identity", "fetch-crl", "-once", "-policy", r.policy, "-params", r.params, "-cache", archive)
	if err != nil {
		r.t.Fatalf("bide identity fetch-crl: %v\n%s", err, out)
	}
	return out
}

// staticRootCRL serves the offline root's CRL, which the bank's PKI team publishes (empty until a CA is revoked).
func staticRootCRL(t *testing.T, r *rig) string {
	t.Helper()
	now := time.Now().UTC()
	der := r.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(48 * time.Hour)})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pkix-crl")
		_, _ = w.Write(der)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/root.crl"
}

func onlyFile(t *testing.T, pattern string) string {
	t.Helper()
	m, err := filepath.Glob(pattern)
	if err != nil || len(m) != 1 {
		t.Fatalf("%s matched %v (%v)", pattern, m, err)
	}
	return m[0]
}

// TestRevocation: issue, fetch step-ca's CRL, approve and run a gated call, revoke, fetch the next CRL, and
// see the decision made with the revoked certificate refused; an expiring certificate is refused by bide approve.
func TestRevocation(t *testing.T) {
	r := newRig(t)
	r.writeGateFiles(staticRootCRL(t, r))
	out := filepath.Join(r.dir, "ops")
	if log, err := r.renew("sso", out); err != nil {
		t.Fatalf("renew.sh: %v\n%s", err, log)
	}
	chain := filepath.Join(out, "ops-chain.pem")
	key := onlyFile(t, filepath.Join(out, "ops-*.key"))
	leaf := leafOf(t, chain)

	// step-ca's CRL, fetched by the command a deployment runs from cron, and the pre-flight over it.
	archive := filepath.Join(r.dir, "crl-archive")
	r.fetchCRLs(archive)
	checks, code := r.identityCheckWith(chain, "-crl-dir", archive)
	for _, id := range []string{"cert.chain", "cert.status.count", "cert.status.exec"} {
		if checks[id].Status != "OK" {
			t.Fatalf("%s: %+v (all: %+v)", id, checks[id], checks)
		}
	}
	if code != 4 {
		t.Fatalf("bide identity check exit %d, want 4 (partial: no recorded gate policy)", code)
	}

	// A gated call is approved with the certificate and runs.
	g := newGate(t, r, archive)
	st := g.status()
	var pending *agent.ApprovalPending
	if err := g.drive("refund-1", st); !errors.As(err, &pending) {
		t.Fatalf("the gate did not wait: %v", err)
	}
	if log, err := g.approve("refund-1", key, chain); err != nil {
		t.Fatalf("bide approve -cert: %v\n%s", err, log)
	}
	g.anchor("refund-1")
	if err := g.drive("refund-1", st); err != nil || g.charged != 1 {
		t.Fatalf("the approved call did not run: %v, charged %d", err, g.charged)
	}

	// The CA operator revokes the certificate; step-ca publishes the CRL at once; the gate's next fetch has it.
	time.Sleep(2 * time.Second)                                // the revocation is later than the first CRL, as in a real day
	r.revoke(leaf.SerialNumber.String(), "affiliationChanged") // a person who left: routine, effective from the revocation
	r.fetchCRLs(archive)
	checks, _ = r.identityCheckWith(chain, "-crl-dir", archive)
	if checks["cert.status.count"].Status != "FAIL" {
		t.Fatalf("after the revocation bide identity check does not fail the status: %+v", checks["cert.status.count"])
	}

	// The approver, who does not know, decides again with the revoked certificate (bide approve checks the
	// chain, not revocation); the gate refuses to count it, and the call does not run.
	st = g.status()
	if err := g.drive("refund-2", st); !errors.As(err, &pending) {
		t.Fatalf("the second gate did not wait: %v", err)
	}
	if log, err := g.approve("refund-2", key, chain); err != nil {
		t.Fatalf("bide approve -cert: %v\n%s", err, log)
	}
	g.anchor("refund-2")
	err := g.drive("refund-2", st)
	if err == nil || g.charged != 1 {
		t.Fatalf("a decision with a revoked certificate ran the call: %v, charged %d", err, g.charged)
	}
	cs := g.checks("refund-2", st)
	if len(cs) != 1 || cs[0].Counted || cs[0].Reason != agent.ReasonRevoked {
		t.Fatalf("the decision with the revoked certificate: %+v, want not counted for %q", cs, agent.ReasonRevoked)
	}
	// And the first decision, made before the revocation, still counts: a routine revocation (a person who
	// left) is effective from its time and does not undo what the holder decided while the certificate was
	// good. (A keyCompromise revocation would: it is effective from the certificate's notBefore.)
	if cs := g.checks("refund-1", g.status()); len(cs) != 1 || !cs[0].Counted {
		t.Fatalf("the decision made before the revocation no longer counts: %+v", cs)
	}

	// Expiry: with less validity left than the approver needs, bide approve refuses before anything is signed.
	left := time.Until(leaf.NotAfter).Round(time.Hour) + time.Hour
	if log, err := g.approve("refund-2", key, chain, "-cert-min-left", left.String()); err == nil || !strings.Contains(log, "left") {
		t.Fatalf("bide approve accepted a certificate with less than %v left: %v\n%s", left, err, log)
	}
}
