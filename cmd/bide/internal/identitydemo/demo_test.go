// The journal is written entirely by the real gate: refunds that wait at 2-of-3 gates (ops and finance
// bound to certificates from a demo CA, risk on a bare key) run on the real agent runtime with a real
// audit.CertApprover (heads signed by a demo log key, CRLs from the demo CA), on a controllable clock so
// the times are relative to now. Four runs: refund-3001 counts ops as logclock-final and finance only
// provisionally (a live count: the status in hand predates her decision) and executes; refund-3002 waits
// for revocation status; refund-3003 waits for a covering signed head; refund-3004 is counted and held
// before execution because the status is too old to run on.
// Package identitydemo writes, as a test that runs only on request, the journal behind the run viewer
// guide's identity screenshots (assets/demo/shots-identity.sh).
package identitydemo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
	"github.com/bide-ai/bide/store/sqlite"
)

func TestWriteDemoJournal(t *testing.T) {
	dir := os.Getenv("BIDE_IDENTITY_DEMO_DIR")
	if dir == "" {
		t.Skip("set BIDE_IDENTITY_DEMO_DIR to write the demo journal")
	}
	if err := run(t, dir); err != nil {
		t.Fatal(err)
	}
}

func bareKey(id string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("bide identity demo " + id))
	return ed25519.NewKeyFromSeed(seed[:])
}

func run(t *testing.T, dir string) error {
	ctx := context.Background()
	s, err := sqlite.Open(filepath.Join(dir, "identity.db"))
	if err != nil {
		return err
	}
	defer s.Close()
	j, err := agent.NewJournal(s)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Second)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	// The demo CA: a root and the intermediate that issues the approvers' certificates.
	caValid := func(c *x509.Certificate) { c.NotBefore, c.NotAfter = ago(72*time.Hour), now.Add(365*24*time.Hour) }
	root := testca.NewRoot(t, "Example Bank Root CA", testca.Opts{Mutate: caValid})
	inter := root.NewIntermediate(t, "Example Bank Approver CA", testca.Opts{Mutate: caValid})
	type person struct {
		id, name string
		n        int
		leaf     *testca.Leaf
	}
	people := map[string]*person{"ops": {id: "ops", name: "Priya Raman", n: 1}, "finance": {id: "finance", name: "Marcus Webb", n: 2}}
	for _, p := range people {
		u, _ := url.Parse(testca.Person(p.n))
		p.leaf = inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(c *x509.Certificate) {
			c.Subject.CommonName, c.URIs = p.name, []*url.URL{u}
			c.NotBefore, c.NotAfter = ago(11*time.Hour), now.Add(11*time.Hour)
		}})
	}
	ci, err := audit.IdentityPolicy{
		Anchors: []*x509.Certificate{root.Cert}, Inter: []audit.SPKIHash{audit.SPKIHashOf(inter.Cert)},
		Identity: map[string]string{"ops": testca.Person(1), "finance": testca.Person(2), "risk": testca.Person(3)},
		IDKind:   audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
	}.Compile()
	if err != nil {
		return err
	}
	logPub, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	logSigner := audit.Ed25519Signer{Priv: logPriv}

	// Revocation status: both CAs publish hourly. CRL 1 predates every decision; CRL 2 is issued 39
	// minutes ago, after the early decisions and before the late one.
	crls := func(withStatus bool) *audit.StatusSnapshot {
		sn := audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
		if !withStatus {
			return sn
		}
		for _, ca := range []*testca.CA{inter, root} {
			for n, at := range []time.Time{ago(60 * time.Minute), ago(39 * time.Minute)} {
				if _, err := sn.AddCRL(ca.NewCRL(t, testca.CRLOpts{Number: int64(n + 1), ThisUpdate: at, NextUpdate: at.Add(time.Hour)})); err != nil {
					panic(err)
				}
			}
		}
		return sn
	}

	clock := now
	clk := func() time.Time { return clock }
	heads := map[string][]audit.SignedTreeHead{}
	anchor := func(run string, at time.Time) error {
		th, err := audit.NewTreeHead(ctx, j, run, at.UnixNano())
		if err != nil {
			return err
		}
		h, err := audit.SignTreeHead(th, logSigner)
		heads[run] = append(heads[run], h)
		return err
	}
	rk, ik := audit.SPKIHashOf(root.Cert), audit.SPKIHashOf(inter.Cert)
	resolver := func(run string, status *audit.StatusSnapshot, execNow func() time.Time) agent.ApproverVerifierFor {
		return func(id string) (agent.ApproverVerifier, bool) {
			if id == "risk" {
				return audit.Ed25519Verifier{Pub: bareKey("risk").Public().(ed25519.PublicKey)}, true
			}
			return &audit.CertApprover{ApproverID: id, Policy: ci, LogKey: audit.Ed25519Verifier{Pub: logPub},
				Heads:  func(string) ([]audit.SignedTreeHead, error) { return heads[run], nil },
				Status: status, Now: clk, ExecNow: execNow, ExecStatusAge: 2 * time.Hour, PolicyEffectiveFrom: ago(72 * time.Hour),
				StatusParams: audit.StatusParams{CAPeriod: time.Hour, W: 5 * time.Second, MinW: time.Second, CountLive: true, MaxStatusAge: time.Hour,
					CAPeriodFor:     map[audit.SPKIHash]time.Duration{rk: time.Hour, ik: time.Hour},
					MaxStatusAgeFor: map[audit.SPKIHash]time.Duration{rk: 24 * time.Hour, ik: 2 * time.Hour}},
				ExecStatusAgeFor: map[audit.SPKIHash]time.Duration{rk: 24 * time.Hour, ik: 2 * time.Hour},
				CAOverlapFor:     map[audit.SPKIHash]time.Duration{rk: 0, ik: 0}}, true
		}
	}
	drive := func(run, order string, verify agent.ApproverVerifierFor) error {
		policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}
		tool := agent.MustFunc("refund", "refund an order to the customer's card", func(context.Context, json.RawMessage) (string, error) { return "refunded", nil }, agent.WithApproval(&policy))
		model := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", fmt.Sprintf(`{"order":%q,"amount":4800,"currency":"USD"}`, order)), agenttest.TextTurn("Refunded order "+order+"."))
		a, err := agent.New(model, j, agent.WithTools(tool), agent.WithApproverVerifiers(verify), agent.WithClock(clk))
		if err != nil {
			return err
		}
		_, err = a.Run(ctx, run, agent.UserText("Refund order "+order+"."))
		return err
	}
	decide := func(run, who string, at time.Time) error {
		recs, err := j.History(ctx, run)
		if err != nil {
			return err
		}
		_, call, _ := agent.FindToolCall(recs, "c1")
		subj := agent.ApprovalSubject{RunID: run, ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
		p := people[who]
		sig := ed25519.Sign(p.leaf.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(subj, who, true, at, agent.CertLeafHash(p.leaf.DER)))
		clock = at.Add(2 * time.Second)
		return agent.SubmitDecision(ctx, j, agent.Decision{RunID: run, ToolUseID: "c1", ApproverID: who, Approved: true, DecidedAt: at, Alg: audit.AlgEd25519, Signature: sig, Cert: p.leaf.Chain()}, agent.WithClock(clk))
	}
	pending := func(err error) error {
		if !errors.As(err, new(*agent.ApprovalPending)) {
			return fmt.Errorf("the gate did not pause: %v", err)
		}
		return nil
	}
	fresh := func() time.Time { return clock }

	// refund-3001: ops counted on status issued after her decision (logclock-final); finance on the live
	// status in hand, issued before hers (provisional). The call runs: a provisional count executes.
	v := resolver("refund-3001", crls(true), fresh)
	clock = ago(50 * time.Minute)
	if err := pending(drive("refund-3001", "3001", v)); err != nil {
		return err
	}
	for _, d := range []struct {
		who string
		at  time.Time
	}{{"ops", ago(40 * time.Minute)}, {"finance", ago(10 * time.Minute)}} {
		if err := decide("refund-3001", d.who, d.at); err != nil {
			return err
		}
		if err := anchor("refund-3001", d.at.Add(10*time.Second)); err != nil {
			return err
		}
	}
	clock = ago(5 * time.Minute)
	if err := drive("refund-3001", "3001", v); err != nil {
		recs, _ := j.History(ctx, "refund-3001")
		_, checks := agent.TallyApprovals(recs, agent.ApprovalSubject{RunID: "refund-3001", ToolUseID: "c1", ToolName: "refund", Args: json.RawMessage(`{"order":"3001","amount":4800,"currency":"USD"}`)}, agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}, v)
		for _, c := range checks {
			fmt.Fprintf(os.Stderr, "check %s counted=%v reason=%q cert=%+v\n", c.Approver, c.Counted, c.Reason, c.Cert)
		}
		return fmt.Errorf("refund-3001: %w", err)
	}

	// refund-3002: ops decided and was anchored, but the gate holds no revocation status.
	v = resolver("refund-3002", crls(false), fresh)
	clock = ago(30 * time.Minute)
	if err := pending(drive("refund-3002", "3002", v)); err != nil {
		return err
	}
	if err := decide("refund-3002", "ops", ago(25*time.Minute)); err != nil {
		return err
	}
	if err := anchor("refund-3002", ago(25*time.Minute-10*time.Second)); err != nil {
		return err
	}
	clock = ago(20 * time.Minute)
	if err := pending(drive("refund-3002", "3002", v)); err != nil {
		return err
	}

	// refund-3003: ops decided, and no signed head covers the decision yet.
	v = resolver("refund-3003", crls(true), fresh)
	clock = ago(15 * time.Minute)
	if err := pending(drive("refund-3003", "3003", v)); err != nil {
		return err
	}
	if err := decide("refund-3003", "ops", ago(10*time.Minute)); err != nil {
		return err
	}
	clock = ago(8 * time.Minute)
	if err := pending(drive("refund-3003", "3003", v)); err != nil {
		return err
	}

	// refund-3004: both decided before CRL 2 and counted final; the check before the call runs finds the
	// status older than the execution bound, so the call is held, not run.
	stale := func() time.Time { return now.Add(3 * time.Hour) }
	v = resolver("refund-3004", crls(true), stale)
	clock = ago(50 * time.Minute)
	if err := pending(drive("refund-3004", "3004", v)); err != nil {
		return err
	}
	for _, d := range []struct {
		who string
		at  time.Time
	}{{"ops", ago(45 * time.Minute)}, {"finance", ago(44 * time.Minute)}} {
		if err := decide("refund-3004", d.who, d.at); err != nil {
			return err
		}
	}
	if err := anchor("refund-3004", ago(43*time.Minute)); err != nil {
		return err
	}
	clock = ago(6 * time.Minute)
	if err := drive("refund-3004", "3004", v); err == nil {
		fmt.Fprintln(os.Stderr, "identity.go: note: refund-3004 was not held")
	}
	return nil
}
