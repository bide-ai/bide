package audit_test

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// Refinement of model 15 (spec/tla/identity): every counterexample configuration switches one rule of
// the counting rule off, and the model shows the property that rule guards failing. This test runs the
// same situation through the real counting rule (agent.TallyApprovals with audit.CertApprover, a stdlib
// CA, signed heads and CRLs) twice:
//
//   - with the situation the broken model mishandles, and the counted set (or, for the tier
//     configurations, the final set) must be what the model's rule gives: the decision the broken
//     model counts is not counted;
//   - with a positive control, the same setup with the rule satisfied (the gap closed, no
//     revocation, the anchor available), and the decision must count (or be final). Without it a
//     scenario would pass for any other reason a decision might not count.
//
// A model configuration with no scenario here (and not listed as pending), or a scenario that names no
// configuration, fails the test, so the model and the code cannot drift apart unnoticed. Scenarios of
// the pre-execution check (E2b) are pending until that check exists.

// countedSet is the approvers whose decision counts, sorted.
func countedSet(checks []agent.DecisionCheck) []string {
	var out []string
	for _, c := range checks {
		if c.Counted {
			out = append(out, c.Approver)
		}
	}
	slices.Sort(out)
	return out
}

// finalSet is the approvers whose decision counts with a final status (the tier the model calls final).
func finalSet(checks []agent.DecisionCheck) []string {
	var out []string
	for _, c := range checks {
		if c.Counted && c.Cert != nil && c.Cert.Tier == "final" {
			out = append(out, c.Approver)
		}
	}
	slices.Sort(out)
	return out
}

// aliceGate is a one-seat gate with alice's valid, anchored decision.
func aliceGate(t *testing.T) *certGate {
	c := newCertGate(t, 1, "alice")
	c.pause()
	c.decideCert("alice", nil)
	c.anchor(certA)
	return c
}

// judged is the counted set of c's gate and the reason alice's decision did not count ("" if it did).
func judged(c *certGate) ([]string, string) {
	_, checks := c.tally()
	r := ""
	for _, ch := range checks {
		if ch.Approver == "alice" && !ch.Counted {
			r = ch.Reason
		}
	}
	return countedSet(checks), r
}

// snap builds a status snapshot from CRLs, bounded as a gate with no status archive requires.
func snap(t *testing.T, ders ...[]byte) *audit.StatusSnapshot {
	t.Helper()
	sn := audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
	for _, der := range ders {
		if _, err := sn.AddCRL(der); err != nil {
			t.Fatal(err)
		}
	}
	return sn
}

// revokedAlice is the entry that lists alice's leaf with the effective time e.
func revokedAlice(c *certGate, e time.Time) x509.RevocationListEntry {
	return testca.Revoked(c.leaves["alice"].Cert, e, 0)
}

// rootRevokesCA is a status series in which the root's CRL lists the issuing CA with the effective time
// e (the CA tier), and the leaf's issuer publishes a clean series (the leaf tier).
func rootRevokesCA(t *testing.T, c *certGate, e time.Time) *audit.StatusSnapshot {
	t.Helper()
	return snap(t,
		c.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(time.Hour)}),
		c.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: c.crlsAt, NextUpdate: c.crlsAt.Add(time.Hour)}),
		c.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: c.crlsAt, NextUpdate: c.crlsAt.Add(time.Hour),
			Entries: []x509.RevocationListEntry{testca.Revoked(c.inter.Cert, e, 2)}}))
}

// cleanSeries is a CRL series by the leaf's issuer at the given offsets from testDecidedAt (numbers
// 1, 2, ...), each valid for a hour, plus a clean one by the root.
func cleanSeries(t *testing.T, c *certGate, offsets ...time.Duration) *audit.StatusSnapshot {
	t.Helper()
	var ders [][]byte
	for i, off := range offsets {
		ders = append(ders, c.inter.NewCRL(t, testca.CRLOpts{Number: int64(i + 1), ThisUpdate: testDecidedAt.Add(off), NextUpdate: testDecidedAt.Add(off + time.Hour)}))
	}
	ders = append(ders,
		c.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(400 * time.Hour)}),
		c.root.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: c.crlsAt, NextUpdate: testDecidedAt.Add(400 * time.Hour)}))
	return snap(t, ders...)
}

// refScenario is one model configuration's scenario. run(t, false) builds the situation the broken model
// mishandles and run(t, true) the same setup with the rule satisfied.
type refScenario struct {
	run     func(t *testing.T, control bool) (got []string, reason string)
	want    []string // the set the model's rule gives in the situation
	why     string   // the reason expected for alice, when the scenario judges her decision
	ctlWant []string // the set with the rule satisfied; alice alone when nil
}

func TestModel15Refinement(t *testing.T) {
	alice := []string{"alice"}
	scenarios := map[string]refScenario{
		// The freshness rule: status older than MaxStatusAge never counts a decision (the live count).
		"regress/withhold-no-fresh": {want: nil, why: agent.ReasonStatusStale, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := aliceGate(t)
			c.params.CountLive, c.params.MaxStatusAge = true, time.Hour
			c.now = func() time.Time { return certA.Add(3 * time.Hour) }
			if ctl {
				c.now = func() time.Time { return certA.Add(30 * time.Second) }
			}
			return judged(c)
		}},
		"regress/stale-status": {want: nil, why: agent.ReasonStatusStale, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := aliceGate(t)
			c.params.CountLive, c.params.MaxStatusAge = true, time.Hour
			c.now = func() time.Time { return certA.Add(2 * time.Hour) }
			if ctl {
				c.now = func() time.Time { return certA.Add(10 * time.Minute) }
			}
			return judged(c)
		}},
		// A(d) is the head's time, not the decision's claim: a decision signed after the revocation, whose
		// claimed time precedes it, is judged at the time it became provable.
		"regress/backdate-by-claim": {want: nil, why: agent.ReasonRevoked, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := aliceGate(t)                   // decided_at is testDecidedAt; the head (A) is ten seconds later
			e := testDecidedAt.Add(time.Second) // after the claim, before A
			if ctl {
				e = certA.Add(time.Minute) // after A: the revocation does not reach the decision
			}
			c.status = c.buildStatus(revokedAlice(c, e))
			return judged(c)
		}},
		// The non-decreasing head-time rule: a larger head stamped earlier than the covering head would pull
		// A(d) before a revocation; the decision is refused.
		"regress/skew-no-nondecreasing": {want: nil, why: agent.ReasonHeadTimes, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := aliceGate(t)
			c.decideBare2() // grow the journal so a larger head exists
			if ctl {
				c.heads = append(c.heads, c.sth(t, certA.Add(-30*time.Second).UnixNano())) // inside the tolerance
				return judged(c)
			}
			c.heads = append(c.heads, c.sth(t, certA.Add(-2*time.Minute).UnixNano()))
			c.status = c.buildStatus(revokedAlice(c, certA.Add(-time.Minute))) // between the early stamp and A
			return judged(c)
		}},
		// DeadStaysOut: a decision a revocation removed is not counted again when a head stamped earlier
		// would move A(d) before the revocation. The count keeps no state: the same journal and heads give
		// the same answer, before and after the skewed head arrives.
		"regress/recount-dead": {want: nil, why: agent.ReasonHeadTimes, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := aliceGate(t)
			c.status = c.buildStatus(revokedAlice(c, certA.Add(-time.Minute)))
			if got, why := judged(c); len(got) != 0 || why != agent.ReasonRevoked {
				t.Fatalf("before the skewed head: counted %v, reason %q", got, why)
			}
			c.decideBare2()
			if ctl { // the later head is in order: the decision stays out because it is revoked, nothing else
				c.heads = append(c.heads, c.sth(t, certA.Add(time.Minute).UnixNano()))
				got, why := judged(c)
				if why != agent.ReasonRevoked {
					t.Fatalf("control: %q", why)
				}
				return got, "" // not counted, as the revocation says; the control's want below is that
			}
			c.heads = append(c.heads, c.sth(t, certA.Add(-2*time.Minute).UnixNano()))
			return judged(c)
		}, ctlWant: nil},
		// The gate's Anchor step: with a store that can anchor, a decision journaled directly counts;
		// without, it waits (and says so).
		"regress/no-anchor": {want: nil, why: agent.ReasonNoCoveringHead, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := newCertGate(t, 1, "alice")
			if ctl {
				c.withReanchoring(true)
			}
			c.pause()
			c.decideCert("alice", nil)
			err := c.run()
			if ctl && (err != nil || c.charged != 1) {
				t.Fatalf("the gate did not anchor and count: %v charged %d", err, c.charged)
			}
			if !ctl && (err == nil || c.charged != 0) {
				t.Fatalf("an unanchored decision ran the call: %v charged %d", err, c.charged)
			}
			return judged(c)
		}},
		// The identifier binding: a decision that names an approver id other than its leaf's identifier
		// does not count.
		"regress/misbind-no-idbind": {want: alice, why: "", ctlWant: []string{"alice", "bob"}, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := newCertGate(t, 2, "alice", "bob")
			c.pause()
			c.decideCert("alice", nil)
			if ctl {
				c.decideCert("bob", nil)
			} else {
				c.decideCert("bob", func(d *agent.Decision) { // bob's id, alice's certificate and key
					l := c.leaves["alice"]
					d.Cert = l.Chain()
					d.Signature = ed25519.Sign(l.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(c.subject(t), "bob", true, testDecidedAt, agent.CertLeafHash(l.DER)))
				})
			}
			c.anchor(certA)
			_, checks := c.tally()
			if r := reasonOf(t, checks, "bob"); !ctl && (r == "" || r == agent.ReasonSharedKey) {
				t.Fatalf("bob's misbound decision was refused for %q, not for the identifier", r)
			}
			return countedSet(checks), ""
		}},
		// One seat per identifier: one identifier under two certificates (a reissue) fills one seat, however
		// many decisions it makes.
		"regress/no-dedup-seat": {want: alice, ctlWant: []string{"alice", "bob"}, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := newCertGate(t, 2, "alice", "bob")
			c.pause()
			c.decideCert("alice", nil)
			if ctl {
				c.decideCert("bob", nil)
			} else { // alice again, under a second certificate for her identifier and another key
				c.leaves["alice"] = c.inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: validAround(t, 1)})
				c.decideCert("alice", nil)
				c.status = c.buildStatus()
			}
			c.anchor(certA)
			tally, checks := c.tally()
			if !ctl && (tally.Approved != 1 || tally.Passed()) {
				t.Fatalf("one identifier under two certificates filled %d seats: %+v", tally.Approved, tally)
			}
			return countedSet(checks), ""
		}},
		// Name constraints: with the policy requiring them, a CA that may issue any identifier is refused.
		"regress/misissue-no-name-constraints": {want: nil, why: agent.ReasonCertChain, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := newCertGate(t, 1, "alice")
			ci, err := audit.IdentityPolicy{
				Anchors: []*x509.Certificate{c.root.Cert}, Inter: []audit.SPKIHash{audit.SPKIHashOf(c.inter.Cert)},
				Identity: map[string]string{"alice": testca.Person(1), "bob": testca.Person(2), "carol": testca.Person(3), "dave": testca.Person(4)},
				IDKind:   audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID, RequireNameConstraints: !ctl,
				Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
			}.Compile()
			if err != nil {
				t.Fatal(err)
			}
			c.ci = ci
			c.pause()
			c.decideCert("alice", nil)
			c.anchor(certA)
			return judged(c)
		}},
		// FinalNeedsCompleteSeries (N2): a series with a CRL missing from the middle (the CA numbers its CRLs
		// consecutively) is not final.
		"regress/final-no-coverage": {want: nil, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := aliceGate(t)
			c.params.ConsecutiveNumbers = true
			offs := []time.Duration{0, 20 * time.Minute, 40 * time.Minute}
			c.status = cleanSeries(t, c, offs...)
			if !ctl { // number 2 withheld: 1 then 3
				c.status = snap(t,
					c.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(time.Hour)}),
					c.inter.NewCRL(t, testca.CRLOpts{Number: 3, ThisUpdate: testDecidedAt.Add(40 * time.Minute), NextUpdate: testDecidedAt.Add(100 * time.Minute)}),
					c.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(400 * time.Hour)}))
			}
			_, checks := c.tally()
			return finalSet(checks), ""
		}, ctlWant: alice},
		// Coverage by nextUpdate alone does not show that nothing lies between two CRLs: a CA that issues
		// hourly with 24 hour validity needs its declared period.
		"regress/final-time-hourly": {want: nil, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := aliceGate(t)
			c.params.CAPeriod = 10 * time.Minute // declared: a CRL at least every ten minutes
			if ctl {
				c.params.CAPeriod = time.Hour // declared: matches the series below
			}
			c.status = snap(t,
				c.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(24 * time.Hour)}),
				c.inter.NewCRL(t, testca.CRLOpts{Number: 3, ThisUpdate: testDecidedAt.Add(40 * time.Minute), NextUpdate: testDecidedAt.Add(24*time.Hour + 40*time.Minute)}),
				c.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(400 * time.Hour)}),
				c.root.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: c.crlsAt, NextUpdate: testDecidedAt.Add(400 * time.Hour)}))
			_, checks := c.tally()
			return finalSet(checks), ""
		}, ctlWant: alice},
		// AbsenceOnlyInWindow: silence from a CRL beyond the first one after notAfter (L(C)) is no evidence;
		// here the CRLs between are not in the series, so the later CRL has no predecessor it follows.
		"regress/final-no-window": {want: nil, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := aliceGate(t)
			c.params.CAPeriod = time.Hour
			offs := []time.Duration{0, 5 * time.Hour} // the leaf is valid until +1h: the second is beyond notAfter
			if ctl {
				offs = []time.Duration{0, 50 * time.Minute, 100 * time.Minute, 150 * time.Minute} // L(C) follows its predecessor
			}
			c.status = cleanSeries(t, c, offs...)
			_, checks := c.tally()
			return finalSet(checks), ""
		}, ctlWant: alice},
		// Terminal stability: a recorded tally is not changed by a later revocation. The fresh count after
		// the revocation differs (nothing counts); the recorded tally stands.
		"regress/recount-terminal": {want: alice, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := aliceGate(t)
			if err := c.run(); err != nil || c.charged != 1 {
				t.Fatalf("%v %d", err, c.charged)
			}
			before, _ := c.recordByName(agent.ApprovalTallyStep("c1"))
			if !ctl {
				c.status = c.buildStatus(revokedAlice(c, certA.Add(-time.Minute))) // learned after the count
				if got, _ := judged(c); len(got) != 0 {
					t.Fatalf("a fresh count after the revocation still counts %v", got)
				}
			}
			_ = c.run()
			after, _ := c.recordByName(agent.ApprovalTallyStep("c1"))
			if string(before.Result) != string(after.Result) || c.charged != 1 {
				t.Fatalf("the recorded tally moved: %s -> %s (charged %d)", before.Result, after.Result, c.charged)
			}
			var tl agent.ApprovalTally
			if err := json.Unmarshal(after.Result, &tl); err != nil {
				t.Fatal(err)
			}
			got := slices.Clone(tl.ApprovedBy)
			slices.Sort(got)
			return got, ""
		}},
		// A decision counted on fresh status is not uncounted when the status goes stale.
		"regress/stale-uncounts": {want: alice, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := aliceGate(t)
			c.params.CountLive, c.params.MaxStatusAge = true, 24*time.Hour
			c.now = func() time.Time { return certA.Add(time.Minute) }
			if err := c.run(); err != nil || c.charged != 1 {
				t.Fatalf("%v %d", err, c.charged)
			}
			before, _ := c.recordByName(agent.ApprovalTallyStep("c1"))
			if !ctl {
				c.now = func() time.Time { return certA.Add(72 * time.Hour) } // the status is now stale
				if got, why := judged(c); len(got) != 0 || why != agent.ReasonStatusStale {
					t.Fatalf("a fresh count on stale status: %v %q", got, why)
				}
			}
			_ = c.run()
			after, _ := c.recordByName(agent.ApprovalTallyStep("c1"))
			if string(before.Result) != string(after.Result) {
				t.Fatalf("stale status changed a recorded tally: %s -> %s", before.Result, after.Result)
			}
			var tl agent.ApprovalTally
			if err := json.Unmarshal(after.Result, &tl); err != nil {
				t.Fatal(err)
			}
			return slices.Clone(tl.ApprovedBy), ""
		}},
		// The tally looks at the archive: a decision counted before the revocation reached the gate does not
		// enter the terminal tally. K=2: alice counts on clean status, the listing arrives, then bob completes
		// the quorum.
		"regress/tally-no-recheck": {want: []string{"bob"}, ctlWant: []string{"alice", "bob"}, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := newCertGate(t, 2, "alice", "bob")
			c.pause()
			c.decideCert("alice", nil)
			c.anchor(certA)
			if got, _ := judged(c); !slices.Equal(got, alice) {
				t.Fatalf("alice should count on clean status before the listing: %v", got)
			}
			if !ctl {
				c.status = c.buildStatus(revokedAlice(c, certA.Add(-time.Minute))) // the listing reaches the gate
			}
			c.decideCert("bob", nil)
			c.anchor(certA.Add(time.Second))
			err := c.run()
			_, hasTally := c.recordByName(agent.ApprovalTallyStep("c1"))
			if ctl && (err != nil || c.charged != 1 || !hasTally) {
				t.Fatalf("control: %v charged %d tally %v", err, c.charged, hasTally)
			}
			if !ctl && (err == nil || c.charged != 0 || hasTally) {
				t.Fatalf("a decision revoked before the tally ran the call or formed a tally: %v charged %d tally %v", err, c.charged, hasTally)
			}
			_, checks := c.tally()
			return countedSet(checks), ""
		}},
		// A CRL past L(C) with no predecessor it follows is no evidence of absence, so the decision does not
		// count on it (the live count does not read it); with the series filled it counts.
		"regress/prune-no-lc-count": {want: nil, why: agent.ReasonStatusUnknown, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := aliceGate(t)
			c.params.CountLive, c.params.MaxStatusAge, c.params.CAPeriod = true, 6*time.Hour, time.Hour
			c.now = func() time.Time { return testDecidedAt.Add(100 * time.Minute) }
			offs := []time.Duration{90 * time.Minute} // only a CRL after notAfter (+1h), with no predecessor it follows
			if ctl {
				offs = []time.Duration{20 * time.Minute, 70 * time.Minute} // L(C) (the first after notAfter) follows a CRL
			}
			c.status = cleanSeries(t, c, offs...)
			return judged(c)
		}},
		// CA tier: the root's CRL listing the issuing CA with E <= A(d) removes the leaf's decision; with a
		// clean root series it counts and is final.
		"regress/count-no-ca-tier": {want: nil, why: agent.ReasonRevoked, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := aliceGate(t)
			c.status = rootRevokesCA(t, c, certA.Add(-time.Hour))
			if ctl {
				c.status = c.buildStatus()
			}
			return judged(c)
		}},
		"regress/final-no-ca-tier": {want: nil, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := aliceGate(t)
			c.status = rootRevokesCA(t, c, certA.Add(-time.Hour))
			if ctl {
				c.status = c.buildStatus()
			}
			_, checks := c.tally()
			return finalSet(checks), ""
		}, ctlWant: alice},
		// The tally looks at the CA tier too: alice counts on a clean root series, the root's listing of the CA
		// arrives, then bob completes the quorum.
		"regress/tally-no-recheck-ca": {want: nil, ctlWant: []string{"alice", "bob"}, run: func(t *testing.T, ctl bool) ([]string, string) {
			c := newCertGate(t, 2, "alice", "bob")
			c.pause()
			c.decideCert("alice", nil)
			c.anchor(certA)
			if got, _ := judged(c); !slices.Equal(got, alice) {
				t.Fatalf("alice should count on a clean root series: %v", got)
			}
			if !ctl {
				c.status = rootRevokesCA(t, c, certA.Add(-time.Hour)) // the root's CRL reaches the gate
			}
			c.decideCert("bob", nil)
			c.anchor(certA.Add(time.Second))
			err := c.run()
			_, hasTally := c.recordByName(agent.ApprovalTallyStep("c1"))
			if ctl && (err != nil || c.charged != 1 || !hasTally) {
				t.Fatalf("control: %v charged %d tally %v", err, c.charged, hasTally)
			}
			if !ctl && (err == nil || c.charged != 0 || hasTally) {
				t.Fatalf("a decision under a revoked CA ran the call or formed a tally: %v charged %d tally %v", err, c.charged, hasTally)
			}
			_, checks := c.tally()
			return countedSet(checks), ""
		}},
	}
	// Scenarios whose expected set is empty and whose control is alice alone use the defaults.
	for name, sc := range scenarios {
		if sc.ctlWant == nil && name != "regress/recount-dead" {
			sc.ctlWant = alice
			scenarios[name] = sc
		}
	}

	// pending lists the model's configurations whose Go counterpart is a later change.
	pending := map[string]string{
		"regress/no-exec-recheck":            "the status check before execution (E2b)",
		"regress/no-exec-recheck-revoked":    "the status check before execution (E2b)",
		"regress/prune-no-lc-exec":           "the status check before execution (E2b)",
		"regress/exec-no-ca-tier":            "the status check before execution (E2b)",
		"regress/exec-leaf-age-from-ca":      "the status check before execution (E2b)",
		"regress/exec-no-recheck-ca-revoked": "the status check before execution (E2b)",
		"limits/alias-two-seats":             "a known limit: see TestModel15AliasLimit",
	}

	// Drift guard: the scenarios and the pending list are exactly the model's regress and limits configs.
	var cfgs []string
	for _, dir := range []string{"regress", "limits"} {
		files, err := filepath.Glob(filepath.Join("..", "spec", "tla", "identity", dir, "*.cfg"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no model 15 %s configurations found: %v", dir, err)
		}
		for _, f := range files {
			cfgs = append(cfgs, dir+"/"+strings.TrimSuffix(filepath.Base(f), ".cfg"))
		}
	}
	slices.Sort(cfgs)
	for _, name := range cfgs {
		_, haveS := scenarios[name]
		_, haveP := pending[name]
		if !haveS && !haveP {
			t.Errorf("model 15 configuration %s has no Go scenario in TestModel15Refinement and is not listed as pending", name)
		}
		if haveS && haveP {
			t.Errorf("%s is both a scenario and pending", name)
		}
	}
	for name := range scenarios {
		if !slices.Contains(cfgs, name) {
			t.Errorf("scenario %s names no model 15 configuration", name)
		}
	}

	for name, sc := range scenarios {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			got, reason := sc.run(t, false)
			if !slices.Equal(got, sc.want) {
				t.Fatalf("counted %v, the model's rule gives %v", got, sc.want)
			}
			if sc.why != "" && reason != sc.why {
				t.Fatalf("alice's decision was refused for %q, want %q", reason, sc.why)
			}
			cgot, creason := sc.run(t, true)
			if !slices.Equal(cgot, sc.ctlWant) {
				t.Fatalf("control (the rule satisfied): counted %v, want %v (reason %q)", cgot, sc.ctlWant, creason)
			}
		})
	}
}

// The limit model 15 states (limits/alias-two-seats): two different identifiers owned by one person fill two
// seats. The code takes injectivity from identifier to person as the bank's evidenced control: both count and
// a two-of-two gate passes. What it does refuse is the same identifier under two approver ids (at compile).
func TestModel15AliasLimit(t *testing.T) {
	c := newCertGate(t, 2, "alice", "bob") // Person(1) and Person(2): say both are one person's aliases
	c.pause()
	c.decideCert("alice", nil)
	c.decideCert("bob", nil)
	c.anchor(certA)
	tally, checks := c.tally()
	if !tally.Passed() || !slices.Equal(countedSet(checks), []string{"alice", "bob"}) {
		t.Fatalf("two identifiers did not fill two seats: %+v", tally)
	}
	_, err := audit.IdentityPolicy{
		Anchors: []*x509.Certificate{c.root.Cert}, Inter: []audit.SPKIHash{audit.SPKIHashOf(c.inter.Cert)},
		Identity: map[string]string{"alice": testca.Person(1), "bob": testca.Person(1)},
		IDKind:   audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
	}.Compile()
	if err == nil {
		t.Fatal("two approver ids with one identifier compiled")
	}
}

// The number rule: a CRL dated later than another with a higher number is not a legitimate successor (a CA
// restored from a backup), and its silence is not evidence of absence. With the only CRL at or after A the
// restored one, the decision has no usable status; with a proper successor it counts.
func TestModel15NumberRule(t *testing.T) {
	for _, ctl := range []bool{false, true} {
		c := aliceGate(t)
		second := int64(3) // number 5 was issued before A; number 3 is dated after it: a restored backup
		if ctl {
			second = 6
		}
		c.status = snap(t,
			c.inter.NewCRL(t, testca.CRLOpts{Number: 5, ThisUpdate: testDecidedAt.Add(-time.Hour), NextUpdate: testDecidedAt.Add(time.Hour)}),
			c.inter.NewCRL(t, testca.CRLOpts{Number: second, ThisUpdate: c.crlsAt, NextUpdate: c.crlsAt.Add(time.Hour)}),
			c.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(400 * time.Hour)}),
			c.root.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: c.crlsAt, NextUpdate: testDecidedAt.Add(400 * time.Hour)}))
		got, why := judged(c)
		if ctl && !slices.Equal(got, []string{"alice"}) {
			t.Fatalf("control: counted %v (%q)", got, why)
		}
		if !ctl && (len(got) != 0 || why != agent.ReasonStatusUnknown) {
			t.Fatalf("silence from a lower-numbered CRL dated later counted: %v (%q)", got, why)
		}
	}
}

// A listing in any CRL stays whatever an older CRL that arrives later says (the series, not a snapshot).
func TestModel15SeriesNotSnapshot(t *testing.T) {
	c := aliceGate(t)
	e := revokedAlice(c, certA.Add(-time.Minute))
	c.status = snap(t,
		c.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: c.crlsAt.Add(time.Hour)}),
		c.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: c.crlsAt, NextUpdate: c.crlsAt.Add(time.Hour), Entries: []x509.RevocationListEntry{e}}),
		c.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(time.Hour)})) // the older CRL arrives last
	got, reason := judged(c)
	if len(got) != 0 || reason != agent.ReasonRevoked {
		t.Fatalf("counted %v, reason %q", got, reason)
	}
}

// id-prune: with a pruning CA, the first CRL beyond notAfter withheld and the pruned one delivered, the live
// count falls back to an earlier fresh CRL: the decision counts provisionally, never final.
func TestModel15PruneFallback(t *testing.T) {
	c := aliceGate(t)
	c.params.CountLive, c.params.MaxStatusAge, c.params.CAPeriod = true, 6*time.Hour, time.Hour
	c.now = func() time.Time { return testDecidedAt.Add(3 * time.Hour) }
	c.status = snap(t,
		c.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt.Add(-5 * time.Minute), NextUpdate: testDecidedAt.Add(10 * time.Hour)}),  // fresh, before A
		c.inter.NewCRL(t, testca.CRLOpts{Number: 4, ThisUpdate: testDecidedAt.Add(170 * time.Minute), NextUpdate: testDecidedAt.Add(10 * time.Hour)}), // the pruned one, long after notAfter
		c.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt.Add(-5 * time.Minute), NextUpdate: testDecidedAt.Add(400 * time.Hour)}))
	_, checks := c.tally()
	if got := countedSet(checks); !slices.Equal(got, []string{"alice"}) {
		t.Fatalf("the earlier fresh CRL did not count the decision: %v (%+v)", got, checks[0])
	}
	if cc := checks[0].Cert; cc == nil || cc.Tier == "final" || cc.BasisKind != agent.BasisLiveBeforeDecision {
		t.Fatalf("a count that fell back to status before the decision is final or mislabelled: %+v", cc)
	}
}
