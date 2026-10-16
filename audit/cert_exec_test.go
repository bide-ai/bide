package audit_test

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// The execution-time re-check of a counted decision (CertApprover.VerifyCertExec), with its two tiers: the
// leaf's issuing CA is judged by ExecStatusAge, a CA above it by its own ExecStatusAgeFor. Test plan item 31
// of docs/design/approver-identity.md.

// execDecision is the CertDecision of id's recorded decision in the gate's journal.
func (c *certGate) execDecision(id string) agent.CertDecision {
	c.t.Helper()
	recs, err := c.store.History(context.Background(), gateRun)
	if err != nil {
		c.t.Fatal(err)
	}
	for i, r := range recs {
		if agent.IsApprovalDecision(r, "c1") && r.Approver() == id {
			return agent.CertDecision{Subject: c.subject(c.t), Record: r, Journal: recs, Index: i}
		}
	}
	c.t.Fatalf("no decision by %s", id)
	return agent.CertDecision{}
}

// longStatus is the harness's series of CRLs (one in force at the decision, one issued at crlsAt) with a
// nextUpdate two days on, so the age of a CRL, not its nextUpdate, is what the execution check judges.
func (c *certGate) longStatus() *audit.StatusSnapshot {
	sn := audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
	for _, x := range []struct {
		ca *testca.CA
		at time.Time
		n  int64
	}{{c.inter, testDecidedAt, 1}, {c.inter, c.crlsAt, 2}, {c.root, testDecidedAt, 1}, {c.root, c.crlsAt, 2}} {
		if _, err := sn.AddCRL(x.ca.NewCRL(c.t, testca.CRLOpts{Number: x.n, ThisUpdate: x.at, NextUpdate: x.at.Add(48 * time.Hour)})); err != nil {
			c.t.Fatal(err)
		}
	}
	return sn
}

// execGate is a counted decision by alice, with live status: the latest CRLs are issued at crlsAt.
func execGate(t *testing.T) *certGate {
	t.Helper()
	c := newCertGate(t, 1, "alice")
	c.status = c.longStatus()
	c.pause()
	c.decideCert("alice", nil)
	c.anchor(certA)
	c.params.CountLive, c.params.MaxStatusAge = true, time.Hour
	c.now = func() time.Time { return c.crlsAt.Add(time.Minute) }
	if tally, checks := c.tally(); !tally.Passed() {
		t.Fatalf("the decision does not count: %+v", checks)
	}
	return c
}

// at sets the execution clock to d after the CRLs' (whole-second) thisUpdate.
func (c *certGate) at(d time.Duration) {
	c.execNow = func() time.Time { return c.crlsAt.Truncate(time.Second).Add(d) }
}

func (c *certGate) execCheck() agent.CertCheck {
	c.t.Helper()
	return c.approver("alice").VerifyCertExec(c.execDecision("alice"))
}

// addCRLs publishes a CRL of the given number at 'later' for the intermediate (listing e, if any) and the
// root (listing the intermediate, if rootListsInter).
func (c *certGate) addCRLs(number int64, later time.Time, interEntries []x509.RevocationListEntry, rootEntries []x509.RevocationListEntry) {
	c.t.Helper()
	for _, x := range []struct {
		ca  *testca.CA
		ent []x509.RevocationListEntry
	}{{c.inter, interEntries}, {c.root, rootEntries}} {
		if _, err := c.status.AddCRL(x.ca.NewCRL(c.t, testca.CRLOpts{Number: number, ThisUpdate: later, NextUpdate: later.Add(48 * time.Hour), Entries: x.ent})); err != nil {
			c.t.Fatal(err)
		}
	}
}

func TestVerifyCertExec_FreshStatusPasses(t *testing.T) {
	c := execGate(t)
	c.at(5 * time.Minute)
	got := c.execCheck()
	if got.Reason != "" || got.Identity != testca.Person(1) || !got.Anchored.Equal(certA) {
		t.Fatalf("%+v", got)
	}
}

// The leaf's issuing CA is judged by ExecStatusAge, not MaxStatusAge: a CRL 90 minutes old counts a
// decision (MaxStatusAge two hours here) and holds the call (ExecStatusAge is one hour).
func TestVerifyCertExec_LeafTierUsesExecStatusAge(t *testing.T) {
	c := execGate(t)
	c.params.MaxStatusAge = 2 * time.Hour
	c.now = func() time.Time { return c.crlsAt.Add(90 * time.Minute) }
	if tally, checks := c.tally(); !tally.Passed() {
		t.Fatalf("count: %+v", checks)
	}
	c.at(90 * time.Minute)
	a := c.approver("alice")
	a.ExecStatusAge = time.Hour
	a.StatusParams.CAPeriod = 30 * time.Minute
	if got := a.VerifyCertExec(c.execDecision("alice")); got.Reason != agent.ReasonStatusStale {
		t.Fatalf("a 90 minute old CRL against ExecStatusAge one hour: %+v", got)
	}
	a.ExecStatusAge = 2 * time.Hour
	if got := a.VerifyCertExec(c.execDecision("alice")); got.Reason != "" {
		t.Fatalf("%+v", got)
	}
}

// The age is inclusive: exactly ExecStatusAge old is usable, one second more is stale.
func TestVerifyCertExec_AgeBoundary(t *testing.T) {
	c := execGate(t)
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{{2 * time.Hour, ""}, {2*time.Hour + time.Second, agent.ReasonStatusStale}} {
		c.at(tc.age)
		// The root's CRLs are 24 hours fresh for the CA tier; the intermediate's are the leaf tier.
		if got := c.execCheck(); got.Reason != tc.want {
			t.Fatalf("age %s: %+v", tc.age, got)
		}
	}
}

// A CA tier is judged by its own ExecStatusAgeFor: with every CRL 10 hours old, a leaf tier age of 11 hours
// passes the leaf's certificate, and a root tier age of one hour holds the call at the intermediate's
// certificate (position 1), where 24 hours passes. A per-issuer MaxStatusAgeFor, which a count honors, does
// not stretch the execution age.
func TestVerifyCertExec_CATierUsesExecStatusAgeFor(t *testing.T) {
	c := execGate(t)
	rk := audit.SPKIHashOf(c.root.Cert)
	c.at(10 * time.Hour)
	a := c.approver("alice")
	a.ExecStatusAge = 11 * time.Hour
	a.StatusParams.CAPeriod = time.Hour
	if got := a.VerifyCertExec(c.execDecision("alice")); got.Reason != "" {
		t.Fatalf("root tier age 24h, root CRL 10h old: %+v", got)
	}
	a.ExecStatusAgeFor = map[audit.SPKIHash]time.Duration{rk: time.Hour}
	got := a.VerifyCertExec(c.execDecision("alice"))
	if got.Reason != agent.ReasonStatusStale || !strings.Contains(got.Detail, "certificate 1 of the chain") {
		t.Fatalf("root tier age 1h, root CRL 10h old: %+v", got)
	}
	// The root's MaxStatusAgeFor (48 hours here) does not decide the execution check.
	a.StatusParams.MaxStatusAgeFor = map[audit.SPKIHash]time.Duration{rk: 48 * time.Hour}
	if got := a.VerifyCertExec(c.execDecision("alice")); got.Reason != agent.ReasonStatusStale {
		t.Fatalf("MaxStatusAgeFor stretched the execution age: %+v", got)
	}
	// And the leaf tier is judged by ExecStatusAge, not by the root's age: one hour holds at certificate 0.
	a.ExecStatusAgeFor = map[audit.SPKIHash]time.Duration{rk: 24 * time.Hour}
	a.ExecStatusAge = time.Hour
	if got := a.VerifyCertExec(c.execDecision("alice")); got.Reason != agent.ReasonStatusStale || !strings.Contains(got.Detail, "certificate 0 of the chain") {
		t.Fatalf("leaf tier age 1h: %+v", got)
	}
}

// A CA tier with no ExecStatusAgeFor holds the call and the finding names what is missing; so does a
// missing ExecStatusAge.
func TestVerifyCertExec_UndeclaredTierHolds(t *testing.T) {
	c := execGate(t)
	c.at(5 * time.Minute)
	a := c.approver("alice")
	a.ExecStatusAgeFor = nil
	got := a.VerifyCertExec(c.execDecision("alice"))
	if got.Reason != agent.ReasonStatusUnknown || !strings.Contains(got.Detail, "ExecStatusAgeFor") || !strings.Contains(got.Detail, "position 1") {
		t.Fatalf("%+v", got)
	}
	a = c.approver("alice")
	a.ExecStatusAge = 0
	if got := a.VerifyCertExec(c.execDecision("alice")); got.Reason != agent.ReasonStatusUnknown || !strings.Contains(got.Detail, "ExecStatusAge") {
		t.Fatalf("no ExecStatusAge: %+v", got)
	}
}

// A revocation published after the decision was counted, dated at or before A(d), stops the call; one dated
// after A(d) does not. The finding says which certificate, when, why and in which CRL.
func TestVerifyCertExec_RevokedAfterCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"dated before A", certA.Add(-time.Minute), agent.ReasonRevoked},
		{"dated at A", certA.Truncate(time.Second), agent.ReasonRevoked},
		{"dated after A", certA.Add(time.Minute), ""},
	} {
		c := execGate(t)
		later := c.crlsAt.Add(5 * time.Minute)
		c.addCRLs(3, later, []x509.RevocationListEntry{testca.Revoked(c.leaves["alice"].Cert, tc.at, 0)}, nil)
		c.execNow = func() time.Time { return later.Add(time.Minute) }
		got := c.execCheck()
		if got.Reason != tc.want {
			t.Fatalf("%s: %+v", tc.name, got)
		}
		if tc.want != "" && (got.Revocation == nil || got.Revocation.Serial != c.leaves["alice"].Cert.SerialNumber.String() || got.Revocation.ReasonCode != 0 ||
			len(got.Revocation.CRL) != 64 || got.Revocation.E == "") {
			t.Fatalf("%s: revocation %+v", tc.name, got.Revocation)
		}
		if tc.want == "" && got.Revocation != nil {
			t.Fatalf("%s: a revocation is reported for a decision that holds", tc.name)
		}
	}
}

// A revoked intermediate (the root lists it) takes the decision with it at execution too.
func TestVerifyCertExec_RevokedIntermediate(t *testing.T) {
	c := execGate(t)
	later := c.crlsAt.Add(5 * time.Minute)
	c.addCRLs(3, later, nil, []x509.RevocationListEntry{testca.Revoked(c.inter.Cert, certA.Add(-time.Hour), 2)})
	c.execNow = func() time.Time { return later.Add(time.Minute) }
	got := c.execCheck()
	if got.Reason != agent.ReasonRevoked || got.Revocation == nil || got.Revocation.Serial != c.inter.Cert.SerialNumber.String() {
		t.Fatalf("%+v", got)
	}
}

// No status, an empty snapshot each hold the call.
func TestVerifyCertExec_FailsClosed(t *testing.T) {
	c := execGate(t)
	c.at(5 * time.Minute)
	cd := c.execDecision("alice")
	c.status = nil
	if got := c.approver("alice").VerifyCertExec(cd); got.Reason != agent.ReasonStatusUnknown {
		t.Fatalf("no status: %+v", got)
	}
	c.status = audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
	if got := c.approver("alice").VerifyCertExec(cd); got.Reason != agent.ReasonStatusUnknown {
		t.Fatalf("empty status: %+v", got)
	}
}

// An execution check never runs without a clock: with neither ExecNow nor Now it reads the wall clock, so a
// CRL from 2026-10 is stale rather than judged against the decision. ExecNow wins over Now.
func TestVerifyCertExec_Clocks(t *testing.T) {
	c := execGate(t)
	c.execNow, c.now = nil, nil
	a := c.approver("alice")
	a.ExecNow = nil
	if got := a.VerifyCertExec(c.execDecision("alice")); got.Reason != agent.ReasonStatusStale {
		t.Fatalf("wall clock: %+v", got)
	}
	// Now alone is the clock of the execution check when ExecNow is nil.
	a.Now = func() time.Time { return c.crlsAt.Add(time.Minute) }
	if got := a.VerifyCertExec(c.execDecision("alice")); got.Reason != "" {
		t.Fatalf("Now: %+v", got)
	}
	// ExecNow overrides it.
	a.ExecNow = func() time.Time { return c.crlsAt.Add(100 * time.Hour) }
	if got := a.VerifyCertExec(c.execDecision("alice")); got.Reason != agent.ReasonStatusStale {
		t.Fatalf("ExecNow: %+v", got)
	}
}

// The check is the whole check: a decision no signed head covers any more fails at execution as at count.
func TestVerifyCertExec_RechecksTheWholeDecision(t *testing.T) {
	c := execGate(t)
	c.at(5 * time.Minute)
	cd := c.execDecision("alice")
	c.heads = nil
	if got := c.approver("alice").VerifyCertExec(cd); got.Reason != agent.ReasonNoCoveringHead {
		t.Fatalf("%+v", got)
	}
	var none *audit.CertApprover
	if got := none.VerifyCertExec(cd); got.Reason != agent.ReasonNoKey {
		t.Fatalf("nil verifier: %+v", got)
	}
}

func TestExecLatencyBound(t *testing.T) {
	if got := audit.ExecLatencyBound(5*time.Minute, 30*time.Minute, 0, time.Hour); got != 95*time.Minute {
		t.Fatalf("leaf tier %s", got)
	}
	if got := audit.ExecLatencyBound(24*time.Hour, 7*24*time.Hour, 12*time.Hour, 8*24*time.Hour); got != 7*24*time.Hour+24*time.Hour+12*time.Hour+8*24*time.Hour {
		t.Fatalf("CA tier %s", got)
	}
}

var _ = errors.New

// ---- the two tiers at configuration time ----

func TestCertApprover_ValidateConfigTiers(t *testing.T) {
	cg := newCertGate(t, 1, "alice")
	rk, ik := audit.SPKIHashOf(cg.root.Cert), audit.SPKIHashOf(cg.inter.Cert)
	day := 24 * time.Hour
	mk := func(mod func(*audit.CertApprover)) *audit.CertApprover {
		c := &audit.CertApprover{ApproverID: "alice", Policy: cg.ci, PolicyEffectiveFrom: testDecidedAt, ExecStatusAge: 2 * time.Hour,
			StatusParams: audit.StatusParams{CAPeriod: time.Hour, MaxStatusAge: time.Hour,
				CAPeriodFor:     map[audit.SPKIHash]time.Duration{rk: 7 * day},
				MaxStatusAgeFor: map[audit.SPKIHash]time.Duration{rk: 14 * day}},
			ExecStatusAgeFor: map[audit.SPKIHash]time.Duration{rk: 8 * day},
			CAOverlapFor:     map[audit.SPKIHash]time.Duration{rk: 12 * time.Hour}}
		mod(c)
		return c
	}
	// A root that publishes weekly is a good CA tier (its period is far above ExecStatusAge/2, which is the
	// leaf issuer's rule).
	if err := mk(func(*audit.CertApprover) {}).ValidateConfig(); err != nil {
		t.Fatalf("a weekly root tier: %v", err)
	}
	for name, c := range map[string]*audit.CertApprover{
		"the anchor declares nothing":     mk(func(c *audit.CertApprover) { c.ExecStatusAgeFor = nil }),
		"non-positive age":                mk(func(c *audit.CertApprover) { c.ExecStatusAgeFor[rk] = 0 }),
		"no CAPeriodFor":                  mk(func(c *audit.CertApprover) { c.StatusParams.CAPeriodFor = nil }),
		"no MaxStatusAgeFor":              mk(func(c *audit.CertApprover) { c.StatusParams.MaxStatusAgeFor = nil }),
		"period plus overlap above age":   mk(func(c *audit.CertApprover) { c.ExecStatusAgeFor[rk] = 7*day + 11*time.Hour }),
		"age above MaxStatusAgeFor":       mk(func(c *audit.CertApprover) { c.StatusParams.MaxStatusAgeFor[rk] = 8*day - time.Second }),
		"negative overlap":                mk(func(c *audit.CertApprover) { c.CAOverlapFor[rk] = -time.Second }),
		"overlap for a CA with no tier":   mk(func(c *audit.CertApprover) { c.CAOverlapFor[ik] = time.Minute }),
		"a non-tier CA with a weekly CRL": mk(func(c *audit.CertApprover) { c.StatusParams.CAPeriodFor[ik] = 7 * day }),
		"a tier entry for a stray issuer": mk(func(c *audit.CertApprover) { c.ExecStatusAgeFor[ik] = day }),
	} {
		err := c.ValidateConfig()
		if err == nil || !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Exactly the sum is allowed, and so is an age equal to MaxStatusAgeFor.
	if err := mk(func(c *audit.CertApprover) {
		c.ExecStatusAgeFor[rk] = 7*day + 12*time.Hour
		c.StatusParams.MaxStatusAgeFor[rk] = 7*day + 12*time.Hour
	}).ValidateConfig(); err != nil {
		t.Errorf("the boundary: %v", err)
	}
	// An intermediate that declares itself a tier is judged as one, not by ExecStatusAge/2.
	if err := mk(func(c *audit.CertApprover) {
		c.StatusParams.CAPeriodFor[ik], c.StatusParams.MaxStatusAgeFor[ik], c.ExecStatusAgeFor[ik] = 5*time.Hour, 2*day, day
	}).ValidateConfig(); err != nil {
		t.Errorf("a middle CA declared as a tier: %v", err)
	}
	// A leaf issued directly by the anchor: the anchor is the leaf's issuing CA, with no tier to declare.
	direct, err := audit.IdentityPolicy{
		Anchors: []*x509.Certificate{cg.root.Cert}, Identity: map[string]string{"alice": testca.Person(1)},
		IDKind: audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
	}.Compile()
	if err != nil {
		t.Fatal(err)
	}
	c := mk(func(c *audit.CertApprover) { c.Policy, c.ExecStatusAgeFor, c.CAOverlapFor = direct, nil, nil })
	c.StatusParams.CAPeriodFor, c.StatusParams.MaxStatusAgeFor = nil, nil
	if err := c.ValidateConfig(); err != nil {
		t.Errorf("a leaf under the anchor: %v", err)
	}
}

// The record carries the tier values, and a change of any of them changes the policy digest.
func TestCertApprover_TierValuesAreInTheDigest(t *testing.T) {
	cg := newCertGate(t, 1, "alice")
	rk := audit.SPKIHashOf(cg.root.Cert)
	a := cg.approver("alice")
	base := a.PolicyDigest()
	d := a.DescribeIdentity()
	if d.ExecStatusAgeFor[hexOf(rk)] != "24h0m0s" || d.MaxStatusAgeFor[hexOf(rk)] != "24h0m0s" || d.CAPeriodFor[hexOf(rk)] != "1h0m0s" {
		t.Fatalf("%+v", d)
	}
	for name, mod := range map[string]func(*audit.CertApprover){
		"ExecStatusAgeFor": func(x *audit.CertApprover) { x.ExecStatusAgeFor = map[audit.SPKIHash]time.Duration{rk: 25 * time.Hour} },
		"CAOverlapFor":     func(x *audit.CertApprover) { x.CAOverlapFor = map[audit.SPKIHash]time.Duration{rk: time.Minute} },
		"MaxStatusAgeFor": func(x *audit.CertApprover) {
			x.StatusParams.MaxStatusAgeFor = map[audit.SPKIHash]time.Duration{rk: 30 * time.Hour}
		},
	} {
		x := cg.approver("alice")
		mod(x)
		if x.PolicyDigest() == base {
			t.Errorf("%s does not change the policy digest", name)
		}
	}
}

func hexOf(k audit.SPKIHash) string { return hex.EncodeToString(k[:]) }
