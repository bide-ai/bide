package identitypreflight

import (
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/policyfile"
)

var effectiveFrom = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// completeParams states every member the policy digest covers, and returns the digest of a CertApprover built
// directly from the same values, as a gate declares them (not through StatusDecl.Approver).
func (g *rig) completeParams() (gateDigest string) {
	g.t.Helper()
	g.params["coverage_tolerance"] = "1m30s"
	g.params["w"], g.params["min_w"], g.params["routine_grace"], g.params["clock_tolerance"] = "12h0m0s", "20m0s", "2m0s", "2m0s"
	g.params["allow_holds"], g.params["consecutive_numbers"], g.params["retained"] = true, true, false
	g.params["effective_from"] = effectiveFrom.Format(time.RFC3339)
	pol, err := policyfile.Read(g.writeJSON("policy.json", g.policy))
	if err != nil {
		g.t.Fatal(err)
	}
	rk, ik := audit.SPKIHashOf(g.root.Cert), audit.SPKIHashOf(g.inter.Cert)
	gate := &audit.CertApprover{
		ApproverID: "alice", Policy: pol, ClockTolerance: 2 * time.Minute, PolicyEffectiveFrom: effectiveFrom, ExecStatusAge: time.Hour,
		ExecStatusAgeFor: map[audit.SPKIHash]time.Duration{rk: 48 * time.Hour, ik: time.Hour},
		CAOverlapFor:     map[audit.SPKIHash]time.Duration{rk: 0, ik: 0},
		StatusParams: audit.StatusParams{
			W: 12 * time.Hour, MinW: 20 * time.Minute, RoutineGrace: 2 * time.Minute, CAPeriod: 30 * time.Minute, MaxStatusAge: 3 * time.Hour,
			CoverageTolerance: 90 * time.Second, AllowHolds: true, ConsecutiveNumbers: true, CountLive: true,
			CAPeriodFor:     map[audit.SPKIHash]time.Duration{rk: 24 * time.Hour, ik: 30 * time.Minute},
			MaxStatusAgeFor: map[audit.SPKIHash]time.Duration{rk: 96 * time.Hour, ik: 3 * time.Hour},
		},
	}
	return gate.PolicyDigest()
}

func TestExpectDigestIsAnEqualityWhenTheFileIsComplete(t *testing.T) {
	g := newRig(t)
	want := g.completeParams()
	o := g.options(g.flatDir(g.freshCRLs()))
	o.Expect, o.ExpectDigest = "", want
	r := g.run(o)
	r.want(t, "params.digest", OK, want)
	r.mustBeReady(t)

	o.ExpectDigest = strings.Repeat("0", 64)
	r = g.run(o)
	r.want(t, "params.digest", Fail, "the gate's is "+o.ExpectDigest)
	if r.Ready || r.ExitCode() != ExitFail {
		t.Fatal("a digest that differs is not ready")
	}
}

func TestExpectDigestRefusedWhenTheFileIsIncomplete(t *testing.T) {
	g := newRig(t)
	g.completeParams()
	for _, drop := range []string{"w", "min_w", "routine_grace", "clock_tolerance", "coverage_tolerance", "allow_holds", "consecutive_numbers", "retained", "effective_from"} {
		t.Run(drop, func(t *testing.T) {
			h := newRig(t)
			want := h.completeParams()
			delete(h.params, drop)
			o := h.options(h.flatDir(h.freshCRLs()))
			o.Expect, o.ExpectDigest = "", want
			r := h.run(o)
			r.want(t, "params.digest", Fail, "lacks "+drop+",")
		})
	}
	t.Run("an old file lacks all of them", func(t *testing.T) {
		h := newRig(t)
		o := h.options(h.flatDir(h.freshCRLs()))
		o.Expect, o.ExpectDigest = "", strings.Repeat("a", 64)
		h.run(o).want(t, "params.digest", Fail, "cannot be compared with the gate's")
	})
}

// With a complete file -expect compares every member, including the ones an old file left unverified.
func TestExpectComparesTheNewMembersWhenStated(t *testing.T) {
	g := newRig(t)
	g.completeParams()
	g.record = g.gateRecord()
	o := g.options(g.flatDir(g.freshCRLs()))
	r := g.run(o)
	r.want(t, "params.expect", OK, "agree")
	if r.get("params.expect.unverified") != nil {
		t.Fatalf("a complete file leaves nothing unverified:\n%s", r.dump())
	}
	rec := g.recordCopy()
	rec["w"] = "24h0m0s"
	o.Expect = g.writeJSON("expect.json", rec)
	found := false
	for _, c := range g.run(o).Checks {
		found = found || (c.ID == "params.expect" && c.Status == Fail && strings.HasPrefix(c.Message, "w:"))
	}
	if !found {
		t.Fatal("a differing w is not reported")
	}
}

func TestExpectDigestUsage(t *testing.T) {
	g := newRig(t)
	o := g.options(g.flatDir(g.freshCRLs()))
	for name, mut := range map[string]func(*Options){
		"not hex":        func(o *Options) { o.ExpectDigest = "xyz" },
		"upper case":     func(o *Options) { o.ExpectDigest = strings.Repeat("A", 64) },
		"with no-expect": func(o *Options) { o.NoExpect, o.ExpectDigest = true, strings.Repeat("a", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			x := o
			x.Expect = ""
			if name == "with no-expect" {
				x.Expect = ""
			}
			mut(&x)
			if _, err := Run(t.Context(), x); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
