package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// The counting rule over certificate-bound approvers: agent.TallyApprovals with audit.CertApprover
// verifiers, a stdlib CA, signed tree heads for A(d) and CRLs for status. Test plan items 1-8, 14,
// 16-19 of docs/design/approver-identity.md.

type certGate struct {
	*gate
	t       *testing.T
	root    *testca.CA
	inter   *testca.CA
	ci      *audit.CertIdentity
	status  *audit.StatusSnapshot
	leaves  map[string]*testca.Leaf
	heads   []audit.SignedTreeHead
	params  audit.StatusParams
	now     func() time.Time
	crlsAt  time.Time                 // when the status CRLs are issued
	execNow func() time.Time          // the execution check's clock, when a test sets it
	tweak   func(*audit.CertApprover) // applied to every verifier the harness builds, when a test sets it
	pending bool
	archive audit.StatusArchive // the verifier's status archive, when set
	goDebug string              // the x509 GODEBUG settings the verifier declares
}

// head times: the decision is signed at testDecidedAt and the head that first covers it is stamped
// 10 s later (A). The leaf is valid for an hour around the decision.
var certA = testDecidedAt.Add(10 * time.Second)

// newCertGate is a Need=need gate over approvers, where alice and bob (when listed) are
// certificate-bound under one CA, each with a leaf (Ed25519 key) for Person(1) and Person(2).
func newCertGate(t *testing.T, need int, approvers ...string) *certGate {
	t.Helper()
	g := newGate(need, approvers, approvers)
	root := testca.NewRoot(t, "root", testca.Opts{})
	inter := root.NewIntermediate(t, "inter", testca.Opts{})
	ci, err := audit.IdentityPolicy{
		Anchors: []*x509.Certificate{root.Cert}, Inter: []audit.SPKIHash{audit.SPKIHashOf(inter.Cert)},
		Identity: map[string]string{"alice": testca.Person(1), "bob": testca.Person(2), "carol": testca.Person(3), "dave": testca.Person(4)},
		IDKind:   audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
	}.Compile()
	if err != nil {
		t.Fatal(err)
	}
	cg := &certGate{gate: g, t: t, root: root, inter: inter, ci: ci, leaves: map[string]*testca.Leaf{}, crlsAt: certA.Add(10 * time.Second)}
	cg.params = audit.StatusParams{CAPeriod: time.Hour, W: 5 * time.Second, MinW: time.Second}
	for i, id := range []string{"alice", "bob", "carol"} {
		n := i + 1
		cg.leaves[id] = inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(c *x509.Certificate) {
			c.NotBefore, c.NotAfter = testDecidedAt.Add(-time.Hour), testDecidedAt.Add(time.Hour)
			c.URIs = nil
			c.URIs = append(c.URIs, mustURL(t, testca.Person(n)))
		}})
	}
	g.custom = cg.resolve
	cg.status = cg.buildStatus()
	return cg
}

// buildStatus is a clean series for the leaf's issuer and the intermediate's: a CRL in force at A and
// one issued after it, so the decision's status is final.
func (c *certGate) buildStatus(revoked ...x509.RevocationListEntry) *audit.StatusSnapshot {
	sn := audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
	add := func(der []byte) {
		if _, err := sn.AddCRL(der); err != nil {
			c.t.Fatal(err)
		}
	}
	add(c.inter.NewCRL(c.t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(time.Hour), Entries: revoked}))
	add(c.inter.NewCRL(c.t, testca.CRLOpts{Number: 2, ThisUpdate: c.crlsAt, NextUpdate: c.crlsAt.Add(time.Hour), Entries: revoked}))
	add(c.root.NewCRL(c.t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(time.Hour)}))
	add(c.root.NewCRL(c.t, testca.CRLOpts{Number: 2, ThisUpdate: c.crlsAt, NextUpdate: c.crlsAt.Add(time.Hour)}))
	return sn
}

// execClock is the clock of the execution check: shortly after the status the harness publishes, unless a
// test moved it (execNow).
func (c *certGate) execClock() func() time.Time {
	return func() time.Time {
		if c.execNow != nil {
			return c.execNow()
		}
		return c.crlsAt.Add(30 * time.Second)
	}
}

func (c *certGate) approver(id string) *audit.CertApprover {
	// The trust anchor is a CA tier (the policy pins the intermediate): it declares its own periods and ages,
	// as an operator does from the CA's capture.
	p := c.params
	rk := audit.SPKIHashOf(c.root.Cert)
	withKey := func(m map[audit.SPKIHash]time.Duration, k audit.SPKIHash, d time.Duration) map[audit.SPKIHash]time.Duration {
		out := map[audit.SPKIHash]time.Duration{}
		for kk, v := range m {
			out[kk] = v
		}
		if _, ok := out[k]; !ok {
			out[k] = d
		}
		return out
	}
	with := func(m map[audit.SPKIHash]time.Duration, d time.Duration) map[audit.SPKIHash]time.Duration {
		out := map[audit.SPKIHash]time.Duration{}
		for k, v := range m {
			out[k] = v
		}
		if _, ok := out[rk]; !ok {
			out[rk] = d
		}
		return out
	}
	p.CAPeriodFor, p.MaxStatusAgeFor = with(p.CAPeriodFor, time.Hour), with(p.MaxStatusAgeFor, 24*time.Hour)
	// The pinned intermediate declares its tier values too (the policy cannot tell it only issues leaves).
	ik := audit.SPKIHashOf(c.inter.Cert)
	ikPeriod := p.CAPeriod
	if ikPeriod <= 0 {
		ikPeriod = time.Hour
	}
	p.CAPeriodFor = withKey(p.CAPeriodFor, ik, ikPeriod)
	p.MaxStatusAgeFor = withKey(p.MaxStatusAgeFor, ik, 2*time.Hour)
	a := &audit.CertApprover{
		ApproverID: id, Policy: c.ci, LogKey: audit.Ed25519Verifier{Pub: c.logPub},
		Heads:  func(string) ([]audit.SignedTreeHead, error) { return c.heads, nil },
		Status: c.status, StatusParams: p, Now: c.now, ExecNow: c.execClock(), ExecStatusAge: 2 * time.Hour, StatusArchive: c.archive, GoDebug: c.goDebug,
		ExecStatusAgeFor:    map[audit.SPKIHash]time.Duration{rk: 24 * time.Hour, ik: 2 * time.Hour},
		CAOverlapFor:        map[audit.SPKIHash]time.Duration{rk: 0, ik: 0},
		PolicyEffectiveFrom: certA.Add(-24 * time.Hour),
	}
	if c.tweak != nil {
		c.tweak(a)
	}
	return a
}

// resolve: alice, bob and carol are certificate-bound; any other id is a bare key.
func (c *certGate) resolve(id string) (agent.ApproverVerifier, bool) {
	switch id {
	case "alice", "bob", "carol":
		return c.approver(id), true
	}
	pub, ok := c.pubs[id]
	if !ok {
		return nil, false
	}
	return audit.Ed25519Verifier{Pub: pub}, true
}

// decideCert records id's v4 decision with its chain (mutate may change the decision before it is
// recorded).
func (c *certGate) decideCert(id string, mutate func(*agent.Decision)) agent.Decision {
	c.t.Helper()
	l := c.leaves[id]
	s := c.subject(c.t)
	sig := ed25519.Sign(l.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(s, id, true, testDecidedAt, agent.CertLeafHash(l.DER)))
	d := agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: id, Approved: true, DecidedAt: testDecidedAt,
		Alg: audit.AlgEd25519, Signature: sig, Cert: l.Chain()}
	if mutate != nil {
		mutate(&d)
	}
	if err := agent.SubmitDecision(context.Background(), c.store, d); err != nil {
		c.t.Fatalf("SubmitDecision %s: %v", id, err)
	}
	return d
}

// anchor signs a head over the journal as it stands, stamped at, and keeps it for A(d).
func (c *certGate) anchor(at time.Time) audit.SignedTreeHead {
	c.t.Helper()
	h := c.sth(c.t, at.UnixNano())
	c.heads = append(c.heads, h)
	return h
}

func (c *certGate) tally() (agent.ApprovalTally, []agent.DecisionCheck) {
	c.t.Helper()
	recs, err := c.store.History(context.Background(), gateRun)
	if err != nil {
		c.t.Fatal(err)
	}
	return agent.TallyApprovals(recs, c.subject(c.t), c.policy, c.resolve)
}

func (c *certGate) pause() {
	c.t.Helper()
	wantPaused(c.t, c.run(), 0)
}

// reasonOf returns the reason the approver's decision did not count ("" when it did).
func reasonOf(t *testing.T, checks []agent.DecisionCheck, id string) string {
	t.Helper()
	for _, ch := range checks {
		if ch.Approver == id {
			if ch.Counted {
				return ""
			}
			return ch.Reason
		}
	}
	t.Fatalf("no check for %s in %+v", id, checks)
	return ""
}

func TestCertApprover_CountsAValidDecision(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	c.pause()
	c.decideCert("alice", nil)
	c.anchor(certA)
	tally, checks := c.tally()
	if !tally.Passed() || len(checks) != 1 || !checks[0].Counted || checks[0].Cert == nil {
		t.Fatalf("%+v %+v", tally, checks)
	}
	cc := checks[0].Cert
	if cc.Identity != testca.Person(1) || cc.Tier != "final" || !strings.Contains(cc.Basis, "at or after the decision") || cc.StatusTime == "" || !cc.Anchored.Equal(certA) {
		t.Fatalf("%+v", cc)
	}
	// The gate runs and charges on it.
	if err := c.run(); err != nil || c.charged != 1 {
		t.Fatalf("run: %v, charged %d", err, c.charged)
	}
}

// The seat is the person: two approver ids of one identity are refused by the same check as two ids on
// one key; and a verifier with no identity for its id has no seat.
func TestCertApprover_SeatIsTheIdentity(t *testing.T) {
	c := newCertGate(t, 1, "alice", "bob")
	ids := c.approver("alice").KeyIDs()
	if len(ids) != 1 || ids[0] != "ident:"+testca.Person(1) {
		t.Fatalf("KeyIDs %v", ids)
	}
	if got := (&audit.CertApprover{ApproverID: "zed", Policy: c.ci}).KeyIDs(); got != nil {
		t.Fatalf("an id with no identity reports %v", got)
	}
	// Two approver ids that the policy maps to one identity cannot be compiled: injectivity.
	_, err := audit.IdentityPolicy{
		Anchors: []*x509.Certificate{c.root.Cert}, Identity: map[string]string{"alice": testca.Person(1), "alice2": testca.Person(1)},
		IDKind: audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
	}.Compile()
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("%v", err)
	}
	// And a policy whose two approvers resolve to one seat is refused.
	shared := func(string) (agent.ApproverVerifier, bool) { return c.approver("alice"), true }
	if err := (agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}).ValidateKeys(shared); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("ValidateKeys: %v", err)
	}
	if err := c.policy.ValidateKeys(c.resolve); err != nil {
		t.Fatalf("distinct identities: %v", err)
	}
}

func TestCertApprover_DowngradeAndKindRules(t *testing.T) {
	// A v3 signature by a certificate-bound approver, with and without a chain, does not count.
	c := newCertGate(t, 1, "alice")
	c.pause()
	pub, priv, _ := ed25519.GenerateKey(nil)
	_ = pub
	s := c.subject(t)
	v3sig := ed25519.Sign(priv, agent.ApprovalDecisionBytes(s, "alice", true, testDecidedAt))
	c.decideCert("alice", func(d *agent.Decision) { d.Signature, d.Cert = v3sig, nil })
	c.anchor(certA)
	tally, checks := c.tally()
	if tally.Passed() || reasonOf(t, checks, "alice") != agent.ReasonNoCert {
		t.Fatalf("v3, no chain: %+v %+v", tally, checks)
	}
	// v3 bytes signed by the leaf's own key, chain attached: not a v4 decision.
	c2 := newCertGate(t, 1, "alice")
	c2.pause()
	l := c2.leaves["alice"]
	c2.decideCert("alice", func(d *agent.Decision) {
		d.Signature = ed25519.Sign(l.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytes(c2.subject(t), "alice", true, testDecidedAt))
	})
	c2.anchor(certA)
	if _, checks := c2.tally(); reasonOf(t, checks, "alice") != agent.ReasonBadSig {
		t.Fatalf("v3 bytes with a chain: %+v", checks)
	}
	// A bare-key approver in the same gate still counts under v3 (mixed policy).
	c3 := newCertGate(t, 2, "alice", "dave")
	c3.pub("dave")
	c3.pause()
	c3.decideCert("alice", nil)
	c3.decideBare("dave")
	c3.anchor(certA)
	if tally, _ := c3.tally(); !tally.Passed() {
		t.Fatalf("mixed: %+v", tally)
	}
}

func (c *certGate) pub(id string) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	c.pubs[id], c.privs[id] = pub, priv
}

func (c *certGate) decideBare(id string) { c.decide(c.t, id, true, false) }
