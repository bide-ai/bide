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

// SubmitDecision with WithDecisionCheck verifies a decision that carries a certificate chain, through the
// approver's certificate-bound verifier (CertApprover.VerifyCertIntake), before anything is written: the
// chain at the time of the write, the v4 signature under the leaf's key, status usable now. Test plan item 22.

// intakeGate is a one-seat gate whose alice has not decided; the clock for the check is just after the
// harness's status.
func intakeGate(t *testing.T) *certGate {
	t.Helper()
	c := newCertGate(t, 1, "alice")
	c.status = c.longStatus()
	c.now = func() time.Time { return c.crlsAt.Add(time.Minute) }
	c.execNow = func() time.Time { return c.crlsAt.Add(time.Minute) }
	c.pause()
	return c
}

// decision is alice's v4 decision under her leaf (mutate may change it).
func (c *certGate) decision(id string, mutate func(*agent.Decision)) agent.Decision {
	c.t.Helper()
	l := c.leaves[id]
	sig := ed25519.Sign(l.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(c.subject(c.t), id, true, testDecidedAt, agent.CertLeafHash(l.DER)))
	d := agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: id, Approved: true, DecidedAt: testDecidedAt,
		Alg: audit.AlgEd25519, Signature: sig, Cert: l.Chain()}
	if mutate != nil {
		mutate(&d)
	}
	return d
}

func (c *certGate) submit(d agent.Decision, wrote *bool) error {
	opts := []agent.ApproveOption{agent.WithDecisionCheck(c.resolve), agent.WithClock(c.now)}
	if wrote != nil {
		opts = append(opts, agent.ReportWritten(wrote))
	}
	return agent.SubmitDecision(context.Background(), c.store, d, opts...)
}

func (c *certGate) decisionRecords() int {
	c.t.Helper()
	recs, err := c.store.History(context.Background(), gateRun)
	if err != nil {
		c.t.Fatal(err)
	}
	n := 0
	for _, r := range recs {
		if agent.IsApprovalDecision(r, "c1") {
			n++
		}
	}
	return n
}

func TestIntake_AValidCertificateDecisionIsWrittenWithItsChain(t *testing.T) {
	c := intakeGate(t)
	var wrote bool
	if err := c.submit(c.decision("alice", nil), &wrote); err != nil || !wrote {
		t.Fatalf("%v wrote=%v", err, wrote)
	}
	recs, _ := c.store.History(context.Background(), gateRun)
	var chain [][]byte
	for _, r := range recs {
		if agent.IsApprovalDecision(r, "c1") {
			chain = r.ApproverCert()
		}
	}
	if len(chain) != 2 || string(chain[0]) != string(c.leaves["alice"].DER) {
		t.Fatalf("recorded chain %d certificates", len(chain))
	}
	// Written, not yet counted: a covering head has to follow (the gate anchors); once it does, it counts.
	c.anchor(certA)
	c.params.CountLive = true
	c.params.MaxStatusAge = 24 * time.Hour
	if tally, checks := c.tally(); !tally.Passed() {
		t.Fatalf("%+v", checks)
	}
	// The same decision again is a no-op.
	wrote = true
	if err := c.submit(c.decision("alice", nil), &wrote); err != nil || wrote {
		t.Fatalf("replay: %v wrote=%v", err, wrote)
	}
	if n := c.decisionRecords(); n != 1 {
		t.Fatalf("%d decision records", n)
	}
}

// Every failure writes nothing, and says why.
func TestIntake_RefusalsWriteNothing(t *testing.T) {
	foreign := testca.NewRoot(t, "other", testca.Opts{})
	for name, tc := range map[string]struct {
		mut  func(*certGate, *agent.Decision)
		want error
		sub  string
	}{
		"bad signature": {func(_ *certGate, d *agent.Decision) {
			d.Signature = append([]byte{}, d.Signature...)
			d.Signature[0] ^= 0xff
		}, agent.ErrInvalidApproval, agent.ReasonBadSig},
		"a v3 signature": {func(c *certGate, d *agent.Decision) {
			d.Signature = ed25519.Sign(c.leaves["alice"].Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytes(c.subject(c.t), "alice", true, testDecidedAt))
		}, agent.ErrInvalidApproval, agent.ReasonBadSig},
		"no chain": {func(_ *certGate, d *agent.Decision) { d.Cert = nil }, agent.ErrInvalidApproval, agent.ReasonNoCert},
		"a foreign chain": {func(c *certGate, d *agent.Decision) {
			l := foreign.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: validAround(t, 1)})
			d.Cert = l.Chain()
			d.Signature = ed25519.Sign(l.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(c.subject(c.t), "alice", true, testDecidedAt, agent.CertLeafHash(l.DER)))
		}, agent.ErrInvalidApproval, agent.ReasonCertChain},
		"a signature by a key that is not the leaf's": {func(c *certGate, d *agent.Decision) {
			_, other, _ := ed25519.GenerateKey(nil)
			d.Signature = ed25519.Sign(other, agent.ApprovalDecisionBytesCert(c.subject(c.t), "alice", true, testDecidedAt, agent.CertLeafHash(c.leaves["alice"].DER)))
		}, agent.ErrInvalidApproval, agent.ReasonBadSig},
		"another person's certificate": {func(c *certGate, d *agent.Decision) {
			l := c.leaves["bob"]
			d.Cert = l.Chain()
			d.Signature = ed25519.Sign(l.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(c.subject(c.t), "alice", true, testDecidedAt, agent.CertLeafHash(l.DER)))
		}, agent.ErrInvalidApproval, agent.ReasonCertIdentity},
		"the scheme the key does not have": {func(_ *certGate, d *agent.Decision) { d.Alg = audit.AlgECDSAP256 }, agent.ErrInvalidApproval, agent.ReasonAlg},
		"the certificate swapped under the signature": {func(c *certGate, d *agent.Decision) {
			d.Cert = c.inter.NewLeaf(t, testca.Opts{KeyWith: c.leaves["alice"].Key, Mutate: validAround(t, 1)}).Chain()
		}, agent.ErrInvalidApproval, agent.ReasonBadSig},
		"a decision time in the certificate's past": {func(c *certGate, d *agent.Decision) {
			d.DecidedAt = testDecidedAt.Add(-3 * time.Hour)
			l := c.leaves["alice"]
			d.Signature = ed25519.Sign(l.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(c.subject(c.t), "alice", true, d.DecidedAt, agent.CertLeafHash(l.DER)))
		}, agent.ErrInvalidApproval, agent.ReasonCertWindow},
	} {
		c := intakeGate(t)
		d := c.decision("alice", func(d *agent.Decision) { tc.mut(c, d) })
		err := c.submit(d, nil)
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.sub) {
			t.Fatalf("%s: %v (want %v containing %q)", name, err, tc.want, tc.sub)
		}
		if n := c.decisionRecords(); n != 0 {
			t.Fatalf("%s: %d decision records written by a refused decision", name, n)
		}
	}
}

// A revoked chain is invalid (422), not unavailable.
func TestIntake_ARevokedChainIsInvalid(t *testing.T) {
	c := intakeGate(t)
	c.status = c.longStatus()
	later := c.crlsAt.Add(30 * time.Second)
	c.addCRLs(3, later, []x509.RevocationListEntry{testca.Revoked(c.leaves["alice"].Cert, testDecidedAt, 4)}, nil)
	err := c.submit(c.decision("alice", nil), nil)
	if !errors.Is(err, agent.ErrInvalidApproval) || !strings.Contains(err.Error(), "revoked") || c.decisionRecords() != 0 {
		t.Fatalf("%v (%d records)", err, c.decisionRecords())
	}
}

// Status that is missing or stale is not a refusal: ErrStatusUnavailable, and nothing is written.
func TestIntake_StatusFailureIsUnavailableAndWritesNothing(t *testing.T) {
	for name, mk := range map[string]func(*certGate){
		"no status": func(c *certGate) { c.status = nil },
		"empty status": func(c *certGate) {
			c.status = audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
		},
		"stale status": func(c *certGate) { // CRLs three hours old, their nextUpdate an hour past, while the certificate is valid
			old := c.crlsAt.Add(-3 * time.Hour)
			c.status = audit.NewStatusSnapshot(audit.WithLimits(agent.MaxStatusInline, 1_000_000))
			for _, ca := range []*testca.CA{c.inter, c.root} {
				if _, err := c.status.AddCRL(ca.NewCRL(c.t, testca.CRLOpts{Number: 1, ThisUpdate: old, NextUpdate: old.Add(time.Hour)})); err != nil {
					c.t.Fatal(err)
				}
			}
		},
	} {
		c := intakeGate(t)
		mk(c)
		err := c.submit(c.decision("alice", nil), nil)
		if !errors.Is(err, agent.ErrStatusUnavailable) || errors.Is(err, agent.ErrConfig) || c.decisionRecords() != 0 {
			t.Fatalf("%s: %v (%d records)", name, err, c.decisionRecords())
		}
	}
}

// The kind of the approver decides: a bare-key approver's decision with a chain is refused; a
// certificate-bound approver's v3 decision without one is refused. Mixed gate.
func TestIntake_TheApproversKindDecides(t *testing.T) {
	c := newCertGate(t, 2, "alice", "dave")
	c.status = c.longStatus()
	c.now = func() time.Time { return c.crlsAt.Add(time.Minute) }
	c.execNow = c.now
	c.pub("dave")
	c.pause()
	s := c.subject(t)
	// dave (bare key) sends a chain.
	dsig := ed25519.Sign(c.privs["dave"], agent.ApprovalDecisionBytes(s, "dave", true, testDecidedAt))
	err := c.submit(agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: "dave", Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519,
		Signature: dsig, Cert: c.leaves["alice"].Chain()}, nil)
	if !errors.Is(err, agent.ErrInvalidApproval) {
		t.Fatalf("a bare-key approver with a chain: %v", err)
	}
	// dave without one is fine.
	if err := c.submit(agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: "dave", Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: dsig}, nil); err != nil {
		t.Fatalf("a bare-key decision: %v", err)
	}
	// alice (certificate-bound) signs v3 with no chain: a downgrade, refused.
	asig := ed25519.Sign(c.leaves["alice"].Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytes(s, "alice", true, testDecidedAt))
	err = c.submit(agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: "alice", Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: asig}, nil)
	if !errors.Is(err, agent.ErrInvalidApproval) || !strings.Contains(err.Error(), "no usable certificate chain") {
		t.Fatalf("a v3 decision by a certificate-bound approver: %v", err)
	}
	if n := c.decisionRecords(); n != 1 {
		t.Fatalf("%d decision records, want dave's only", n)
	}
}

// One decision per approver: a second, different valid decision is ErrAlreadyDecided; a chain whose
// leaf differs makes it another decision, but it still does not count twice.
func TestIntake_SecondDecisionIsAlreadyDecided(t *testing.T) {
	c := intakeGate(t)
	if err := c.submit(c.decision("alice", nil), nil); err != nil {
		t.Fatal(err)
	}
	deny := c.decision("alice", func(d *agent.Decision) {
		d.Approved = false
		l := c.leaves["alice"]
		d.Signature = ed25519.Sign(l.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(c.subject(c.t), "alice", false, testDecidedAt, agent.CertLeafHash(l.DER)))
	})
	if err := c.submit(deny, nil); !errors.Is(err, agent.ErrAlreadyDecided) {
		t.Fatalf("%v", err)
	}
	if n := c.decisionRecords(); n != 1 {
		t.Fatalf("%d decision records", n)
	}
}

// A decision already recorded is the same decision again even while status cannot be judged: the replay is a
// no-op (nothing written), not a 503, so a client's retry loop ends.
func TestIntake_ReplayDuringAStatusOutageIsStillTheSameDecision(t *testing.T) {
	c := intakeGate(t)
	if err := c.submit(c.decision("alice", nil), nil); err != nil {
		t.Fatal(err)
	}
	c.status = nil // the outage
	var wrote bool
	if err := c.submit(c.decision("alice", nil), &wrote); err != nil || wrote {
		t.Fatalf("replay during an outage: %v wrote=%v", err, wrote)
	}
	if n := c.decisionRecords(); n != 1 {
		t.Fatalf("%d decision records", n)
	}
	// A different decision during the outage is still unavailable, not already decided or written.
	deny := c.decision("alice", func(d *agent.Decision) {
		d.Approved = false
		l := c.leaves["alice"]
		d.Signature = ed25519.Sign(l.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(c.subject(c.t), "alice", false, testDecidedAt, agent.CertLeafHash(l.DER)))
	})
	// A different decision is judged first: during the outage that is unavailable, never written; once status is
	// back, the approver has already decided.
	if err := c.submit(deny, nil); !errors.Is(err, agent.ErrStatusUnavailable) {
		t.Fatalf("a different decision during the outage: %v", err)
	}
	c.status = c.longStatus()
	if err := c.submit(deny, nil); !errors.Is(err, agent.ErrAlreadyDecided) {
		t.Fatalf("a different decision after one is held: %v", err)
	}
	if n := c.decisionRecords(); n != 1 {
		t.Fatalf("%d decision records", n)
	}
}

// A request that repeats a recorded decision's public fields (approver, verdict, time, leaf) with other
// signature bytes is not a replay: it is judged, and a garbage, empty or flipped signature is refused (never
// answered as the held decision), whatever status is doing.
func TestIntake_AReplayNeedsTheSameSignatureOrAValidOne(t *testing.T) {
	for _, outage := range []bool{false, true} {
		c := intakeGate(t)
		if err := c.submit(c.decision("alice", nil), nil); err != nil {
			t.Fatal(err)
		}
		if outage {
			c.status = nil
		}
		for name, sig := range map[string][]byte{
			"a flipped byte": func() []byte { s := append([]byte{}, c.decision("alice", nil).Signature...); s[0] ^= 0xff; return s }(),
			"garbage":        []byte("not a signature"),
		} {
			var wrote bool
			err := c.submit(c.decision("alice", func(d *agent.Decision) { d.Signature = sig }), &wrote)
			if err == nil || wrote {
				t.Fatalf("outage=%v %s: answered as a replay (err %v, wrote %v)", outage, name, err, wrote)
			}
			if !errors.Is(err, agent.ErrInvalidApproval) && !errors.Is(err, agent.ErrStatusUnavailable) {
				t.Fatalf("outage=%v %s: %v", outage, name, err)
			}
		}
		if n := c.decisionRecords(); n != 1 {
			t.Fatalf("%d decision records", n)
		}
	}
}

// The tier a finding gets is one function for the gate's record and the intake's response.
func TestIdentityTierIsOneClassifier(t *testing.T) {
	for _, tc := range []struct {
		tier, time, want string
	}{
		{"final", "log signer clock: head of size 3 root ab", agent.TierLogclockFinal},
		{"final", "rfc3161 tsa.example", agent.TierIndependentFinal},
		{"final", "anchor-receipt log.example", agent.TierIndependentFinal},
		{"final", "some future wording", agent.TierLogclockFinal}, // an unknown source is never promoted
		{"final", "", agent.TierLogclockFinal},
		{"provisional", "rfc3161 tsa.example", agent.TierProvisional},
		{"provisional", "log signer clock: head of size 3 root ab", agent.TierProvisional},
		{"no information", "rfc3161 tsa.example", agent.TierProvisional},
	} {
		if got := agent.IdentityTier(&agent.CertCheck{Tier: tc.tier, Time: tc.time}); got != tc.want {
			t.Fatalf("%q %q: %q, want %q", tc.tier, tc.time, got, tc.want)
		}
	}
}

// The same verdict and time under another leaf (a reissued certificate) is another decision, not the same
// one, and the approver has already decided.
func TestIntake_AnotherLeafIsAnotherDecision(t *testing.T) {
	c := intakeGate(t)
	if err := c.submit(c.decision("alice", nil), nil); err != nil {
		t.Fatal(err)
	}
	reissued := c.inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: validAround(t, 1)})
	d := c.decision("alice", func(d *agent.Decision) {
		d.Cert = reissued.Chain()
		d.Signature = ed25519.Sign(reissued.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(c.subject(c.t), "alice", true, testDecidedAt, agent.CertLeafHash(reissued.DER)))
	})
	var wrote bool
	if err := c.submit(d, &wrote); !errors.Is(err, agent.ErrAlreadyDecided) || wrote {
		t.Fatalf("%v wrote=%v", err, wrote)
	}
	if n := c.decisionRecords(); n != 1 {
		t.Fatalf("%d decision records", n)
	}
}

// A record of the approver's that does not verify (a junk decision written without a check) does not hold
// the approver's decision hostage: a valid one is still accepted.
func TestIntake_AJunkRecordDoesNotBlockTheRealDecision(t *testing.T) {
	c := intakeGate(t)
	junk := c.decision("alice", func(d *agent.Decision) { d.Signature = append([]byte{}, d.Signature...); d.Signature[0] ^= 0xff })
	if err := agent.SubmitDecision(context.Background(), c.store, junk); err != nil { // unchecked: as a direct writer could
		t.Fatal(err)
	}
	if n := c.decisionRecords(); n != 1 {
		t.Fatalf("setup: %d records", n)
	}
	var wrote bool
	good := c.decision("alice", nil)
	if err := c.submit(good, &wrote); err != nil || !wrote {
		t.Fatalf("%v wrote=%v", err, wrote)
	}
}

// The chain is judged at the time of the write, not at the signed time: a certificate valid when the
// approver decided and expired since is refused.
func TestIntake_JudgedAtTheTimeOfTheWriteNotTheSignedTime(t *testing.T) {
	c := intakeGate(t)
	late := testDecidedAt.Add(3 * time.Hour) // the leaf is valid until testDecidedAt+1h
	c.now = func() time.Time { return late }
	c.execNow = c.now
	err := c.submit(c.decision("alice", nil), nil)
	if !errors.Is(err, agent.ErrInvalidApproval) || !strings.Contains(err.Error(), "validity") {
		t.Fatalf("%v", err)
	}
	if n := c.decisionRecords(); n != 0 {
		t.Fatalf("%d records", n)
	}
}
