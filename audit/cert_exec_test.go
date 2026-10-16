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
	a.StatusParams.CAPeriodFor[audit.SPKIHashOf(c.inter.Cert)] = 30 * time.Minute
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
	a.StatusParams.CAPeriodFor[audit.SPKIHashOf(c.inter.Cert)] = 30 * time.Minute
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
				CAPeriodFor:     map[audit.SPKIHash]time.Duration{rk: 7 * day, ik: time.Hour},
				MaxStatusAgeFor: map[audit.SPKIHash]time.Duration{rk: 14 * day, ik: 4 * time.Hour}},
			ExecStatusAgeFor: map[audit.SPKIHash]time.Duration{rk: 8 * day, ik: 2 * time.Hour},
			CAOverlapFor:     map[audit.SPKIHash]time.Duration{rk: 12 * time.Hour, ik: 0}}
		mod(c)
		return c
	}
	// A root that publishes weekly is a good CA tier (its period is far above ExecStatusAge/2, which is the
	// leaf issuer's rule), beside an intermediate that publishes hourly.
	if err := mk(func(*audit.CertApprover) {}).ValidateConfig(); err != nil {
		t.Fatalf("a weekly root tier: %v", err)
	}
	for name, mod := range map[string]func(*audit.CertApprover){
		"the anchor declares nothing":             func(c *audit.CertApprover) { delete(c.ExecStatusAgeFor, rk) },
		"the pinned intermediate declares none":   func(c *audit.CertApprover) { delete(c.ExecStatusAgeFor, ik) },
		"no ExecStatusAgeFor at all":              func(c *audit.CertApprover) { c.ExecStatusAgeFor = nil },
		"non-positive age":                        func(c *audit.CertApprover) { c.ExecStatusAgeFor[rk] = 0 },
		"no CAPeriodFor for the root":             func(c *audit.CertApprover) { delete(c.StatusParams.CAPeriodFor, rk) },
		"no CAPeriodFor for the intermediate":     func(c *audit.CertApprover) { delete(c.StatusParams.CAPeriodFor, ik) },
		"no CAOverlapFor for the root":            func(c *audit.CertApprover) { delete(c.CAOverlapFor, rk) },
		"no CAOverlapFor for the intermediate":    func(c *audit.CertApprover) { delete(c.CAOverlapFor, ik) },
		"no MaxStatusAgeFor for the root":         func(c *audit.CertApprover) { delete(c.StatusParams.MaxStatusAgeFor, rk) },
		"no MaxStatusAgeFor for the intermediate": func(c *audit.CertApprover) { delete(c.StatusParams.MaxStatusAgeFor, ik) },
		"period plus overlap above age":           func(c *audit.CertApprover) { c.ExecStatusAgeFor[rk] = 7*day + 12*time.Hour - time.Nanosecond },
		"age above MaxStatusAgeFor":               func(c *audit.CertApprover) { c.StatusParams.MaxStatusAgeFor[rk] = 8*day - time.Nanosecond },
		"negative overlap":                        func(c *audit.CertApprover) { c.CAOverlapFor[rk] = -time.Nanosecond },
		"overlap above the period":                func(c *audit.CertApprover) { c.CAOverlapFor[ik] = time.Hour + time.Nanosecond },
		"a stray weekly CA with no tier": func(c *audit.CertApprover) {
			c.StatusParams.CAPeriodFor[audit.SPKIHash{9}] = 2*time.Hour/2 + time.Nanosecond
		},
		"overlap for a CA with no tier":    func(c *audit.CertApprover) { c.CAOverlapFor[audit.SPKIHash{9}] = time.Minute },
		"the leaf issuer's default period": func(c *audit.CertApprover) { c.StatusParams.CAPeriod = time.Hour + time.Nanosecond },
	} {
		err := mk(mod).ValidateConfig()
		if err == nil || !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The boundaries are accepted exactly: period plus overlap equal to the age, the age equal to
	// MaxStatusAgeFor, the overlap equal to the period, the default period equal to ExecStatusAge/2.
	for name, mod := range map[string]func(*audit.CertApprover){
		"period plus overlap equal to the age": func(c *audit.CertApprover) { c.ExecStatusAgeFor[rk] = 7*day + 12*time.Hour },
		"age equal to MaxStatusAgeFor":         func(c *audit.CertApprover) { c.StatusParams.MaxStatusAgeFor[rk] = 8 * day },
		"overlap equal to the period":          func(c *audit.CertApprover) { c.CAOverlapFor[ik] = time.Hour; c.ExecStatusAgeFor[ik] = 2 * time.Hour },
		"a zero overlap":                       func(c *audit.CertApprover) { c.CAOverlapFor[rk] = 0 },
		"a stray period at ExecStatusAge/2":    func(c *audit.CertApprover) { c.StatusParams.CAPeriodFor[audit.SPKIHash{9}] = time.Hour },
	} {
		if err := mk(mod).ValidateConfig(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A leaf issued directly by the anchor, with no intermediate pinned: the anchor is the leaf's issuing CA,
	// which declares no tier.
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

// The tier is the position in the accepted chain, never the issuer's key: an intermediate that issues one
// approver's leaf and sits above another's issuing CA is judged by ExecStatusAge for the first and by its
// ExecStatusAgeFor for the second.
func TestVerifyCertExec_TierIsThePositionInTheChain(t *testing.T) {
	c := newCertGate(t, 2, "alice", "bob")
	// bob's leaf is issued by interY, which interX (the harness's inter) issued; alice's by interX.
	interY := c.inter.NewIntermediate(t, "interY", testca.Opts{})
	// Long-lived leaves: a CRL issued after a certificate expires is not evidence about it (except the first),
	// so a five hour series needs certificates that outlive it.
	long := func(n int) func(*x509.Certificate) {
		return func(x *x509.Certificate) {
			validAround(t, n)(x)
			x.NotAfter = testDecidedAt.Add(12 * time.Hour)
		}
	}
	c.leaves["alice"] = c.inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: long(1)})
	c.leaves["bob"] = interY.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: long(2)})
	ci, err := audit.IdentityPolicy{
		Anchors: []*x509.Certificate{c.root.Cert}, Inter: []audit.SPKIHash{audit.SPKIHashOf(c.inter.Cert), audit.SPKIHashOf(interY.Cert)},
		Identity: map[string]string{"alice": testca.Person(1), "bob": testca.Person(2)},
		IDKind:   audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID, MaxChain: 5,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
	}.Compile()
	if err != nil {
		t.Fatal(err)
	}
	c.ci = ci
	ik, yk := audit.SPKIHashOf(c.inter.Cert), audit.SPKIHashOf(interY.Cert)
	// interX: judged by ExecStatusAge where it issues the leaf (alice), by its own age where it sits above
	// interY (bob). Every pinned CA declares its tier values.
	c.tweak = func(a *audit.CertApprover) {
		a.ExecStatusAge = time.Hour
		a.StatusParams.CAPeriod = 30 * time.Minute
		a.StatusParams.CAPeriodFor[ik], a.StatusParams.CAPeriodFor[yk] = 30*time.Minute, 30*time.Minute
		a.StatusParams.MaxStatusAgeFor[ik], a.StatusParams.MaxStatusAgeFor[yk] = 48*time.Hour, 48*time.Hour
		a.ExecStatusAgeFor[ik], a.ExecStatusAgeFor[yk] = 24*time.Hour, time.Hour
		a.CAOverlapFor[ik], a.CAOverlapFor[yk] = 0, 0
	}
	// Every CA publishes hourly CRLs with two-day nextUpdates; interX's newest is issued 5 hours after the
	// others, who have an older one (alice's leaf CRLs are interX's).
	c.status = audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
	add := func(ca *testca.CA, n int64, at time.Time) {
		if _, err := c.status.AddCRL(ca.NewCRL(t, testca.CRLOpts{Number: n, ThisUpdate: at, NextUpdate: at.Add(48 * time.Hour)})); err != nil {
			t.Fatal(err)
		}
	}
	for _, ca := range []*testca.CA{c.root, c.inter, interY} {
		add(ca, 1, testDecidedAt)
		add(ca, 2, c.crlsAt)
	}
	c.pause()
	c.decideCert("alice", nil)
	c.decideCert("bob", nil)
	c.anchor(certA)
	c.params.CountLive, c.params.MaxStatusAge = true, 48*time.Hour
	c.now = func() time.Time { return c.crlsAt.Add(time.Minute) }
	c.at(5 * time.Hour) // every CRL is five hours old at the check
	approver := c.approver
	// alice: certificate 0 is issued by interX, the leaf's issuing CA: its CRL is five hours old against one hour.
	got := approver("alice").VerifyCertExec(c.execDecision("alice"))
	if got.Reason != agent.ReasonStatusStale || !strings.Contains(got.Detail, "certificate 0 of the chain") {
		t.Fatalf("alice: %+v", got)
	}
	// bob: certificate 0 is issued by interY (five hours old against one hour: stale there, at position 0), so
	// give interY a fresh CRL; then interX, now at position 1 (a tier), passes under its 24 hours.
	for n, at := int64(3), c.crlsAt.Add(30*time.Minute); !at.After(c.crlsAt.Add(5*time.Hour)); n, at = n+1, at.Add(30*time.Minute) {
		add(interY, n, at) // interY keeps publishing every 30 minutes
	}
	got = approver("bob").VerifyCertExec(c.execDecision("bob"))
	if got.Reason != "" {
		t.Fatalf("bob: interX as a tier above interY should pass under 24h: %+v", got)
	}
	// And with interX's tier age under its CRL's age, bob is held at certificate 1, the position of interX.
	b := approver("bob")
	b.ExecStatusAgeFor[audit.SPKIHashOf(c.inter.Cert)] = time.Hour
	got = b.VerifyCertExec(c.execDecision("bob"))
	if got.Reason != agent.ReasonStatusStale || !strings.Contains(got.Detail, "certificate 1 of the chain") {
		t.Fatalf("bob with a one hour tier: %+v", got)
	}
}

// A three-tier chain whose middle CA declares nothing holds the call, naming what is missing (the policy
// cannot know at configuration which pinned CA will sit above a leaf's issuer for which chain).
func TestVerifyCertExec_UndeclaredMiddleCAHolds(t *testing.T) {
	c := execGate(t)
	c.at(5 * time.Minute)
	a := c.approver("alice")
	delete(a.ExecStatusAgeFor, audit.SPKIHashOf(c.root.Cert))
	got := a.VerifyCertExec(c.execDecision("alice"))
	if got.Reason != agent.ReasonStatusUnknown || !strings.Contains(got.Detail, "ExecStatusAgeFor") || !strings.Contains(got.Detail, "position 1") {
		t.Fatalf("%+v", got)
	}
}

// The leaf's issuing CA must publish at least twice within ExecStatusAge: the accepted chain tells which CA
// that is, so the check refuses a leaf whose issuer's period does not fit, at the boundary exactly.
func TestVerifyCertExec_LeafIssuerPeriodMustFit(t *testing.T) {
	c := execGate(t)
	c.at(5 * time.Minute)
	ik := audit.SPKIHashOf(c.inter.Cert)
	a := c.approver("alice")
	a.ExecStatusAge = 2 * time.Hour
	a.StatusParams.CAPeriodFor[ik] = time.Hour
	if got := a.VerifyCertExec(c.execDecision("alice")); got.Reason != "" {
		t.Fatalf("period = ExecStatusAge/2: %+v", got)
	}
	a.StatusParams.CAPeriodFor[ik] = time.Hour + time.Nanosecond
	if got := a.VerifyCertExec(c.execDecision("alice")); got.Reason != agent.ReasonStatusUnknown || !strings.Contains(got.Detail, "ExecStatusAge/2") {
		t.Fatalf("period above ExecStatusAge/2: %+v", got)
	}
	// With no per-issuer period the default applies.
	delete(a.StatusParams.CAPeriodFor, ik)
	a.StatusParams.CAPeriod = time.Hour + time.Nanosecond
	if got := a.VerifyCertExec(c.execDecision("alice")); got.Reason != agent.ReasonStatusUnknown {
		t.Fatalf("default period above ExecStatusAge/2: %+v", got)
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
