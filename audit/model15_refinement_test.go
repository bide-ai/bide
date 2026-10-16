package audit_test

import (
	"crypto/ed25519"
	"crypto/x509"
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
// the counting rule off and the model shows the property that rule guards failing. This test runs the
// same scenario through the real counting rule (agent.TallyApprovals with audit.CertApprover, a stdlib
// CA, signed heads and CRLs) with the rule on, and compares the set of counted approvers with what the
// model says the rule gives: the decision the broken model counts is not counted. A configuration with no
// scenario here, or one named here that the model does not have, fails the test, so the model and the code
// cannot drift apart unnoticed. Scenarios of the pre-execution check (E2b) are listed as pending.

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

type refScenario struct {
	// run builds the scenario and returns the counted set and the reason alice's decision did not count
	// ("" when it counted, or no decision of alice's is judged).
	run  func(t *testing.T) (counted []string, reason string)
	want []string // the counted set the model's rule gives
	why  string   // the reason expected for alice, when the scenario judges her decision
}

// aliceGate is a one-seat gate with alice's valid, anchored decision.
func aliceGate(t *testing.T) *certGate {
	c := newCertGate(t, 1, "alice")
	c.pause()
	c.decideCert("alice", nil)
	c.anchor(certA)
	return c
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

func TestModel15Refinement(t *testing.T) {
	revokedBeforeA := func(c *certGate) x509.RevocationListEntry {
		return testca.Revoked(c.leaves["alice"].Cert, certA.Add(-time.Minute), 0)
	}
	scenarios := map[string]refScenario{
		// The freshness rule: status older than MaxStatusAge never counts a decision.
		"regress/withhold-no-fresh": {want: nil, why: agent.ReasonStatusStale, run: func(t *testing.T) ([]string, string) {
			c := aliceGate(t)
			c.params.CountLive, c.params.MaxStatusAge = true, time.Hour
			c.now = func() time.Time { return certA.Add(3 * time.Hour) }
			return judged(c)
		}},
		"regress/stale-status": {want: nil, why: agent.ReasonStatusStale, run: func(t *testing.T) ([]string, string) {
			c := aliceGate(t)
			c.params.CountLive, c.params.MaxStatusAge = true, time.Hour
			c.now = func() time.Time { return certA.Add(2 * time.Hour) }
			return judged(c)
		}},
		"regress/recount-dead": {want: nil, why: agent.ReasonRevoked, run: func(t *testing.T) ([]string, string) {
			c := aliceGate(t)
			e := revokedBeforeA(c)
			c.status = c.buildStatus(e) // CRL 2 lists it; a still later CRL that no longer lists it does not undo it
			if _, err := c.status.AddCRL(c.inter.NewCRL(t, testca.CRLOpts{Number: 3, ThisUpdate: c.crlsAt.Add(time.Minute), NextUpdate: c.crlsAt.Add(time.Hour)})); err != nil {
				t.Fatal(err)
			}
			return judged(c)
		}},
		"regress/tally-no-recheck": {want: nil, why: agent.ReasonRevoked, run: func(t *testing.T) ([]string, string) {
			c := aliceGate(t)
			c.status = c.buildStatus(revokedBeforeA(c))
			if err := c.run(); err == nil || c.charged != 0 {
				t.Fatalf("a revoked decision ran the call: %v charged %d", err, c.charged)
			}
			if _, ok := c.recordByName(agent.ApprovalTallyStep("c1")); ok {
				t.Fatal("a tally formed on a revoked decision")
			}
			return judged(c)
		}},
		// A(d) is the head's time, not the decision's claim: a decision signed after the revocation counts as such.
		"regress/backdate-by-claim": {want: nil, why: agent.ReasonRevoked, run: func(t *testing.T) ([]string, string) {
			c := aliceGate(t) // decided_at is testDecidedAt; the head is certA, ten seconds later
			c.status = c.buildStatus(testca.Revoked(c.leaves["alice"].Cert, testDecidedAt.Add(time.Second), 0))
			return judged(c)
		}},
		// The non-decreasing head-time rule: a larger head stamped earlier implicates the covering head.
		"regress/skew-no-nondecreasing": {want: nil, why: agent.ReasonHeadTimes, run: func(t *testing.T) ([]string, string) {
			c := aliceGate(t)
			c.decideBare2() // grow the journal
			c.heads = append(c.heads, c.sth(t, certA.Add(-time.Hour).UnixNano()))
			return judged(c)
		}},
		// The gate's Anchor step: with a store that can anchor, the decision counts; without, it waits.
		"regress/no-anchor": {want: []string{"alice"}, run: func(t *testing.T) ([]string, string) {
			c := newCertGate(t, 1, "alice")
			c.withReanchoring(true)
			c.pause()
			c.decideCert("alice", nil)
			if err := c.run(); err != nil || c.charged != 1 {
				t.Fatalf("the gate did not anchor and count: %v charged %d", err, c.charged)
			}
			return judged(c)
		}},
		// The identifier binding: a decision naming an approver id other than its leaf's identifier does not count.
		"regress/misbind-no-idbind": {want: []string{"alice"}, run: func(t *testing.T) ([]string, string) {
			c := newCertGate(t, 2, "alice", "bob")
			c.pause()
			c.decideCert("alice", nil)
			c.decideCert("bob", func(d *agent.Decision) { // bob's id, alice's certificate and key
				l := c.leaves["alice"]
				d.Cert = l.Chain()
				d.Signature = ed25519.Sign(l.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(c.subject(t), "bob", true, testDecidedAt, agent.CertLeafHash(l.DER)))
			})
			c.anchor(certA)
			_, checks := c.tally()
			if r := reasonOf(t, checks, "bob"); r == "" || r == agent.ReasonSharedKey {
				t.Fatalf("bob's misbound decision: %q", r)
			}
			return countedSet(checks), ""
		}},
		// One seat per person: counting identifiers, not decisions (one key under two approver ids).
		"regress/no-dedup-seat": {want: []string{"alice"}, run: func(t *testing.T) ([]string, string) {
			c := newCertGate(t, 2, "alice", "bob")
			c.leaves["bob"] = c.inter.NewLeaf(t, testca.Opts{KeyWith: c.leaves["alice"].Key, Mutate: validAround(t, 2)})
			c.pause()
			c.decideCert("alice", nil)
			c.decideCert("bob", nil)
			c.anchor(certA)
			_, checks := c.tally()
			return countedSet(checks), ""
		}},
		// Name constraints: a CA that may issue any identifier is refused when the policy requires constraints.
		"regress/misissue-no-name-constraints": {want: nil, why: agent.ReasonCertChain, run: func(t *testing.T) ([]string, string) {
			c := newCertGate(t, 1, "alice")
			ci, err := audit.IdentityPolicy{
				Anchors: []*x509.Certificate{c.root.Cert}, Inter: []audit.SPKIHash{audit.SPKIHashOf(c.inter.Cert)},
				Identity: map[string]string{"alice": testca.Person(1), "bob": testca.Person(2), "carol": testca.Person(3), "dave": testca.Person(4)},
				IDKind:   audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID, RequireNameConstraints: true,
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
		// Coverage of the series: a gap in the CRL series (an omitted CRL) leaves the status provisional, not final.
		"regress/final-no-coverage": {want: nil, run: func(t *testing.T) ([]string, string) {
			c := aliceGate(t)
			c.params.CAPeriod = time.Hour
			sn := audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
			for _, der := range [][]byte{
				c.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: c.crlsAt.Add(24 * time.Hour)}),
				c.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(24 * time.Hour)}),
				// number 2 omitted: the next CRL comes three hours after the first
				c.inter.NewCRL(t, testca.CRLOpts{Number: 3, ThisUpdate: testDecidedAt.Add(3 * time.Hour), NextUpdate: testDecidedAt.Add(27 * time.Hour)}),
			} {
				if _, err := sn.AddCRL(der); err != nil {
					t.Fatal(err)
				}
			}
			c.status = sn
			_, checks := c.tally()
			return finalSet(checks), ""
		}},
		"regress/final-time-hourly": {want: nil, run: func(t *testing.T) ([]string, string) {
			c := aliceGate(t)
			c.params.CAPeriod = time.Hour // coverage by nextUpdate alone (24 h) does not show nothing lies between two CRLs
			sn := audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
			for _, der := range [][]byte{
				c.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(48 * time.Hour)}),
				c.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(24 * time.Hour)}),
				c.inter.NewCRL(t, testca.CRLOpts{Number: 3, ThisUpdate: testDecidedAt.Add(2 * time.Hour), NextUpdate: testDecidedAt.Add(26 * time.Hour)}),
			} {
				if _, err := sn.AddCRL(der); err != nil {
					t.Fatal(err)
				}
			}
			c.status = sn
			_, checks := c.tally()
			return finalSet(checks), ""
		}},
		"regress/final-no-window": {want: nil, run: func(t *testing.T) ([]string, string) {
			// Status that ends before A+W' is provisional: only a CRL at or after A+W' (or L(C)) is final.
			c := aliceGate(t)
			c.params.W, c.params.MinW, c.params.CAPeriod = 24*time.Hour, time.Minute, time.Hour
			_, checks := c.tally()
			return finalSet(checks), ""
		}},
		// Terminal stability: a recorded tally is not changed by a later revocation or by status going stale.
		"regress/recount-terminal": {want: []string{"alice"}, run: func(t *testing.T) ([]string, string) {
			c := aliceGate(t)
			if err := c.run(); err != nil || c.charged != 1 {
				t.Fatalf("%v %d", err, c.charged)
			}
			before, _ := c.recordByName(agent.ApprovalTallyStep("c1"))
			c.status = c.buildStatus(revokedBeforeA(c)) // a revocation dated before the decision, learned after
			_ = c.run()
			after, _ := c.recordByName(agent.ApprovalTallyStep("c1"))
			if string(before.Result) != string(after.Result) || c.charged != 1 {
				t.Fatalf("the recorded tally moved: %s -> %s (charged %d)", before.Result, after.Result, c.charged)
			}
			tally, _ := agent.RecordedTally(map[string]agent.Record{after.Name: after}, "c1")
			_ = tally
			return []string{"alice"}, ""
		}},
		"regress/stale-uncounts": {want: []string{"alice"}, run: func(t *testing.T) ([]string, string) {
			c := aliceGate(t)
			c.params.CountLive, c.params.MaxStatusAge = true, 24*time.Hour
			c.now = func() time.Time { return certA.Add(time.Minute) }
			if err := c.run(); err != nil || c.charged != 1 {
				t.Fatalf("%v %d", err, c.charged)
			}
			before, _ := c.recordByName(agent.ApprovalTallyStep("c1"))
			c.now = func() time.Time { return certA.Add(72 * time.Hour) } // the status is now stale
			_ = c.run()
			after, _ := c.recordByName(agent.ApprovalTallyStep("c1"))
			if string(before.Result) != string(after.Result) {
				t.Fatalf("stale status changed a recorded tally: %s -> %s", before.Result, after.Result)
			}
			return []string{"alice"}, ""
		}},
		// A CRL past the first one after notAfter (L(C)) is listing-only: its silence is not evidence of absence.
		"regress/prune-no-lc-count": {want: nil, run: func(t *testing.T) ([]string, string) {
			c := newCertGate(t, 1, "alice")
			c.params.CAPeriod = time.Hour
			c.pause()
			c.decideCert("alice", nil)
			c.anchor(certA)
			// The CRLs that list the revocation (the one before notAfter and L(C)) are withheld; only one issued
			// well after notAfter (the leaf is valid to testDecidedAt+1h), with the entry pruned, is delivered.
			sn := audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
			late := testDecidedAt.Add(5 * time.Hour)
			for _, der := range [][]byte{
				c.root.NewCRL(t, testca.CRLOpts{Number: 9, ThisUpdate: late, NextUpdate: late.Add(time.Hour)}),
				c.inter.NewCRL(t, testca.CRLOpts{Number: 9, ThisUpdate: late, NextUpdate: late.Add(time.Hour)}),
			} {
				if _, err := sn.AddCRL(der); err != nil {
					t.Fatal(err)
				}
			}
			c.status = sn
			_, checks := c.tally()
			return finalSet(checks), ""
		}},
	}

	// pending lists the model's configurations whose Go counterpart is a later change.
	pending := map[string]string{
		"regress/no-exec-recheck":         "the status check before execution (E2b)",
		"regress/no-exec-recheck-revoked": "the status check before execution (E2b)",
		"regress/prune-no-lc-exec":        "the status check before execution (E2b)",
		// The injectivity limit is a control the bank evidences; Go refuses what it can see (below).
		"limits/alias-two-seats": "see TestModel15AliasLimit",
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
			got, reason := sc.run(t)
			if !slices.Equal(got, sc.want) {
				t.Fatalf("counted %v, the model's rule gives %v", got, sc.want)
			}
			if sc.why != "" && reason != sc.why {
				t.Fatalf("alice's decision was refused for %q, want %q", reason, sc.why)
			}
		})
	}
}

// The injectivity limit (limits/alias-two-seats): two approver ids for one identifier are refused when the
// policy is compiled, which is the part of "one person, one seat" the code can see; that a person has one
// identifier is the bank's evidenced control, as the model states.
func TestModel15AliasLimit(t *testing.T) {
	c := newCertGate(t, 1, "alice")
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

// The series rule (the model's RollbackCRL, folded into the status rule): a listing in any CRL stays,
// whatever an older CRL that arrives later says.
func TestModel15SeriesNotSnapshot(t *testing.T) {
	c := aliceGate(t)
	sn := audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
	add := func(der []byte) {
		if _, err := sn.AddCRL(der); err != nil {
			t.Fatal(err)
		}
	}
	e := testca.Revoked(c.leaves["alice"].Cert, certA.Add(-time.Minute), 0)
	add(c.root.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: c.crlsAt.Add(time.Hour)}))
	add(c.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: c.crlsAt, NextUpdate: c.crlsAt.Add(time.Hour), Entries: []x509.RevocationListEntry{e}}))
	add(c.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: testDecidedAt, NextUpdate: testDecidedAt.Add(time.Hour)})) // the older CRL arrives last
	c.status = sn
	got, reason := judged(c)
	if len(got) != 0 || reason != agent.ReasonRevoked {
		t.Fatalf("counted %v, reason %q", got, reason)
	}
}
