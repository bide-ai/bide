package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

func TestCertApprover_ProvableAt(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	c.pause()
	short := c.sth(t, certA.UnixNano()) // a head over the journal before the decision exists
	c.decideCert("alice", nil)
	if _, checks := c.tally(); reasonOf(t, checks, "alice") != agent.ReasonNoCoveringHead {
		t.Fatalf("no heads: %+v", checks)
	}
	c.heads = append(c.heads, short)
	if _, checks := c.tally(); reasonOf(t, checks, "alice") != agent.ReasonNoCoveringHead {
		t.Fatalf("a head short of the decision: %+v", checks)
	}
	// A head signed by another key is ignored.
	_, other, _ := ed25519.GenerateKey(nil)
	th, err := audit.NewTreeHead(context.Background(), c.store, gateRun, certA.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	c.heads = append(c.heads, signTH(t, th, other))
	if _, checks := c.tally(); reasonOf(t, checks, "alice") != agent.ReasonNoCoveringHead {
		t.Fatalf("a head under another key: %+v", checks)
	}
	// The smallest covering head fixes A: a later, larger head does not move it.
	c.anchor(certA)
	c.heads = append(c.heads, c.sth(t, certA.Add(time.Hour).UnixNano()))
	tally, checks := c.tally()
	if !tally.Passed() || !checks[0].Cert.Anchored.Equal(certA) {
		t.Fatalf("%+v %+v", tally, checks)
	}
}

// decideBare2 appends a record to the journal (a decision by an ineligible id) so that a later head is
// larger.
func (c *certGate) decideBare2() {
	c.pub("zed")
	c.decide(c.t, "zed", true, false)
}

// A larger head stamped before a smaller one, beyond the tolerance, implicates both: a decision whose
// A(d) would come from either is not provable. Heads of equal size are not a contradiction, and skew
// inside the tolerance is jitter.
func TestCertApprover_ContradictoryHeadTimes(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	c.pause()
	c.decideCert("alice", nil)
	c.anchor(certA)
	c.heads = append(c.heads, c.sth(t, certA.Add(-5*time.Second).UnixNano())) // the same size, an earlier stamp
	tally, checks := c.tally()
	if !tally.Passed() || !checks[0].Cert.Anchored.Equal(certA.Add(-5*time.Second)) {
		t.Fatalf("equal-size heads: the earliest stamp is A: %+v %+v", tally, checks)
	}
	c2 := newCertGate(t, 1, "alice")
	c2.pause()
	c2.decideCert("alice", nil)
	c2.anchor(certA)
	c2.decideBare2() // grow the journal
	c2.heads = append(c2.heads, c2.sth(t, certA.Add(-time.Hour).UnixNano()))
	if _, checks := c2.tally(); reasonOf(t, checks, "alice") != agent.ReasonHeadTimes {
		t.Fatalf("%+v", checks)
	}
	c3 := newCertGate(t, 1, "alice")
	c3.pause()
	c3.decideCert("alice", nil)
	c3.anchor(certA)
	c3.decideBare2()
	c3.heads = append(c3.heads, c3.sth(t, certA.Add(-30*time.Second).UnixNano()))
	if tally, _ := c3.tally(); !tally.Passed() {
		t.Fatal("30 s of skew is inside the tolerance")
	}
}

func validAround(t *testing.T, n int) func(*x509.Certificate) {
	return func(c *x509.Certificate) {
		c.NotBefore, c.NotAfter = testDecidedAt.Add(-time.Hour), testDecidedAt.Add(time.Hour)
		c.URIs = nil
		c.URIs = append(c.URIs, mustURL(t, testca.Person(n)))
	}
}

func TestCertApprover_ChainAndWindow(t *testing.T) {
	type tc struct {
		name   string
		reason string
		setup  func(c *certGate)
		at     time.Time
	}
	otherRoot := testca.NewRoot(t, "other", testca.Opts{})
	for _, k := range []tc{
		{"wrong anchor", agent.ReasonCertChain, func(c *certGate) {
			oi := otherRoot.NewIntermediate(t, "inter", testca.Opts{})
			c.leaves["alice"] = oi.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: validAround(t, 1)})
		}, certA},
		{"another person's certificate", agent.ReasonCertIdentity, func(c *certGate) { c.leaves["alice"] = c.leaves["bob"] }, certA},
		{"no key usage for approving", agent.ReasonCertUsage, func(c *certGate) {
			c.leaves["alice"] = c.inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(x *x509.Certificate) {
				validAround(t, 1)(x)
				x.UnknownExtKeyUsage = nil
			}})
		}, certA},
		{"expired when it became provable", agent.ReasonCertWindow, nil, testDecidedAt.Add(3 * time.Hour)},
		{"not yet valid when it became provable", agent.ReasonCertWindow, nil, testDecidedAt.Add(-3 * time.Hour)},
	} {
		c := newCertGate(t, 1, "alice", "bob")
		if k.setup != nil {
			k.setup(c)
		}
		c.pause()
		c.decideCert("alice", nil)
		c.anchor(k.at)
		_, checks := c.tally()
		if got := reasonOf(t, checks, "alice"); got != k.reason {
			detail := ""
			if checks[0].Cert != nil {
				detail = checks[0].Cert.Detail
			}
			t.Fatalf("%s: reason %q (%s), want %q", k.name, got, detail, k.reason)
		}
	}
}

// Certificate substitution: the same signature with another valid certificate for the same key.
func TestCertApprover_CertificateSubstitution(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	c.pause()
	l := c.leaves["alice"]
	twin := c.inter.NewLeaf(t, testca.Opts{KeyWith: l.Key, Mutate: validAround(t, 1)})
	c.decideCert("alice", func(d *agent.Decision) { d.Cert = twin.Chain() }) // signed over l's hash, attached to twin
	c.anchor(certA)
	if _, checks := c.tally(); reasonOf(t, checks, "alice") != agent.ReasonBadSig {
		t.Fatalf("%+v", checks)
	}
}

func TestCertApprover_Status(t *testing.T) {
	// Revoked at or before A: does not count; after A: counts.
	for _, tc := range []struct {
		name   string
		at     time.Time
		reason string
	}{{"before A", certA.Add(-time.Minute), agent.ReasonRevoked}, {"after A", certA.Add(time.Minute), ""}} {
		c := newCertGate(t, 1, "alice")
		c.pause()
		c.decideCert("alice", nil)
		c.anchor(certA)
		c.status = c.buildStatus(testca.Revoked(c.leaves["alice"].Cert, tc.at, 0))
		_, checks := c.tally()
		if got := reasonOf(t, checks, "alice"); got != tc.reason {
			t.Fatalf("%s: %q (%s)", tc.name, got, checks[0].Cert.Detail)
		}
	}
	// A revoked intermediate takes the leaf's decisions with it.
	c := newCertGate(t, 1, "alice")
	c.pause()
	c.decideCert("alice", nil)
	c.anchor(certA)
	sn := audit.NewStatusSnapshot()
	for _, der := range [][]byte{
		c.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(time.Hour)}),
		c.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: c.crlsAt, NextUpdate: c.crlsAt.Add(time.Hour)}),
		c.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: c.crlsAt, NextUpdate: c.crlsAt.Add(time.Hour),
			Entries: []x509.RevocationListEntry{testca.Revoked(c.inter.Cert, certA.Add(-time.Hour), 2)}}),
	} {
		if _, err := sn.AddCRL(der); err != nil {
			t.Fatal(err)
		}
	}
	c.status = sn
	if _, checks := c.tally(); reasonOf(t, checks, "alice") != agent.ReasonRevoked {
		t.Fatalf("revoked intermediate: %+v", checks)
	}
	// No status at all, and an empty snapshot: unknown.
	c.status = nil
	if _, checks := c.tally(); reasonOf(t, checks, "alice") != agent.ReasonStatusUnknown {
		t.Fatalf("no status: %+v", checks)
	}
	c.status = audit.NewStatusSnapshot()
	if _, checks := c.tally(); reasonOf(t, checks, "alice") != agent.ReasonStatusUnknown {
		t.Fatalf("empty status: %+v", checks)
	}
	// Live: stale status is stale, and a fresh pre-A CRL counts provisionally.
	live := newCertGate(t, 1, "alice")
	live.pause()
	live.decideCert("alice", nil)
	live.anchor(certA)
	live.params.CountLive, live.params.MaxStatusAge = true, time.Hour
	live.now = func() time.Time { return certA.Add(3 * time.Hour) }
	if _, checks := live.tally(); reasonOf(t, checks, "alice") != agent.ReasonStatusStale {
		t.Fatalf("stale: %+v", checks)
	}
	live.now = func() time.Time { return certA.Add(30 * time.Second) }
	tally, checks := live.tally()
	if !tally.Passed() || checks[0].Cert == nil {
		t.Fatalf("live: %+v %+v", tally, checks)
	}
}

// A live count that rests on status issued before the decision is provisional, and the check says so.
func TestCertApprover_LiveCountIsProvisional(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	c.pause()
	c.decideCert("alice", nil)
	c.anchor(certA)
	// Only CRLs issued before A: offline there is no status; live they count, provisionally.
	sn := audit.NewStatusSnapshot()
	for _, der := range [][]byte{
		c.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: certA.Add(-time.Second), NextUpdate: certA.Add(time.Hour)}),
		c.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: certA.Add(-time.Second), NextUpdate: certA.Add(time.Hour)}),
	} {
		if _, err := sn.AddCRL(der); err != nil {
			t.Fatal(err)
		}
	}
	c.status = sn
	if _, checks := c.tally(); reasonOf(t, checks, "alice") != agent.ReasonStatusUnknown {
		t.Fatalf("offline: %+v", checks)
	}
	c.params.CountLive, c.params.MaxStatusAge = true, time.Hour
	c.now = func() time.Time { return certA.Add(10 * time.Second) }
	tally, checks := c.tally()
	if !tally.Passed() || checks[0].Cert.Tier != "provisional" || checks[0].Cert.Basis != "live count: status issued before the decision (provisional only)" {
		t.Fatalf("%+v %+v", tally, checks[0].Cert)
	}
}

// One key, one seat: the same leaf key under two identities does not fill two seats.
func TestCertApprover_OneKeyTwoSeats(t *testing.T) {
	c := newCertGate(t, 2, "alice", "bob")
	c.leaves["bob"] = c.inter.NewLeaf(t, testca.Opts{KeyWith: c.leaves["alice"].Key, Mutate: validAround(t, 2)})
	c.pause()
	c.decideCert("alice", nil)
	c.decideCert("bob", nil)
	c.anchor(certA)
	tally, checks := c.tally()
	if tally.Passed() || tally.Approved != 1 {
		t.Fatalf("%+v", tally)
	}
	if reasonOf(t, checks, "alice") != "" || reasonOf(t, checks, "bob") != agent.ReasonSharedKey {
		t.Fatalf("%+v", checks)
	}
}

// Separation of duties by identity: the requester's identifier excludes a certificate-bound approver.
func TestCertApprover_ExcludePrincipalByIdentity(t *testing.T) {
	c := newCertGate(t, 1, "alice", "bob")
	c.policy.ExcludePrincipal = true
	wantPaused(t, c.runAs(testca.Person(1)), 0) // requested by person 1: alice's identity
	c.decideCert("alice", nil)
	c.decideCert("bob", nil)
	c.anchor(certA)
	tally, checks := c.tally()
	if reasonOf(t, checks, "alice") != agent.ReasonRequester || reasonOf(t, checks, "bob") != "" || !tally.Passed() {
		t.Fatalf("%+v %+v", tally, checks)
	}
}

func TestCertApprover_VerifyIsNeverTrue(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	a := c.approver("alice")
	if a.Alg() != agent.AlgCertificate || a.Verify([]byte("m"), []byte("s")) {
		t.Fatal("a certificate-bound verifier accepted a bare signature")
	}
	var _ agent.CertDecisionVerifier = a
	if err := (&audit.CertApprover{}).VerifyCertDecision(agent.CertDecision{}); err.Reason == "" {
		t.Fatal("a verifier with no policy accepted a decision")
	}
}
