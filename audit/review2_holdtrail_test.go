package audit_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

type r2LiftFails struct {
	agent.Holder
	fails int
}

func (l *r2LiftFails) LiftHold(ctx context.Context, id string) (bool, error) {
	if l.fails > 0 {
		l.fails--
		return false, errors.New("store down")
	}
	return l.Holder.LiftHold(ctx, id)
}

// A placement whose record could not be written leaves a hold in force and unrecorded. Placing it
// again must be able to record it; otherwise HOLD_NOT_IN_TRAIL can only be cleared by lifting the hold.
func TestReview2_UnrecordedPlacementCanBeRecordedByPlacingAgain(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	h := agent.Hold{ID: "h1", Matter: "m", Runs: []string{"gov"}}
	if err := audit.PlaceLegalHold(ctx, failingTrail{r.trail}, r.store, r.signer, "test", nil, h, "r", "t", "e", time.Now()); err == nil {
		t.Fatal("expected the recording to fail")
	}
	if err := audit.PlaceLegalHold(ctx, r.trail, r.store, r.signer, "test", nil, h, "r", "t", "e", time.Now()); err != nil {
		t.Fatalf("placing again to record the hold: %v", err)
	}
	if rep, err := r.verifyHolds(true); err != nil {
		t.Fatalf("%+v %v", rep.Findings, err)
	}
}

// A lift whose store step failed after the record: running the lift again must finish it without a
// second lift record (which the audit would report as HOLD_UNMATCHED for good).
func TestReview2_ResumedLiftLeavesNoUnmatchedRecord(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	if err := r.placeHold("h1", "gov"); err != nil {
		t.Fatal(err)
	}
	flaky := &r2LiftFails{Holder: r.store, fails: 1}
	if err := audit.LiftLegalHold(ctx, r.trail, flaky, r.signer, "test", nil, "h1", "r", "t", "e", time.Now()); err == nil {
		t.Fatal("expected the store lift to fail")
	}
	if err := audit.LiftLegalHold(ctx, r.trail, flaky, r.signer, "test", nil, "h1", "r", "t", "e", time.Now()); err != nil {
		t.Fatalf("resuming the lift: %v", err)
	}
	rep, err := r.verifyHolds(true)
	if err != nil {
		t.Fatalf("a resumed lift is reported: %+v", rep.Findings)
	}
}

// A pass that began before a hold was placed and ended after it, having replaced records, may have
// erased under the hold (with the store's hold removed and put back by hand in between); the trail
// cannot say when each record went, so the audit must not call it clean.
func TestReview2_PassSpanningAHoldPlacementIsNotClean(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	begin := audit.RedactionRecord{Deployment: "test", Phase: audit.RedactionBegin, PlanDigest: "p", RunID: "gov", Reason: "r", Ticket: "t", Executor: "e", AtMs: 1_700_000_000_000, HeadSize: 1}
	for _, p := range r.plan.Records {
		begin.Records = append(begin.Records, audit.RedactedRef{Position: p.Position, Name: p.Name, Kind: string(p.Kind), Length: p.Length, LeafHash: p.LeafHash})
	}
	if err := audit.AppendRedactionRecord(ctx, r.trail, begin, r.signer); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Redact(ctx, r.j, r.plan, time.UnixMilli(1_700_000_000_000)); err != nil { // erased (before the hold, here)
		t.Fatal(err)
	}
	if err := r.placeHold("h1", "gov"); err != nil {
		t.Fatal(err)
	}
	end := begin
	end.Phase, end.Records, end.Redacted, end.AtMs = audit.RedactionEnd, nil, len(r.plan.Records), 1_700_000_020_000
	if err := audit.AppendRedactionRecord(ctx, r.trail, end, r.signer); err != nil {
		t.Fatal(err)
	}
	rep, err := r.verifyHolds(true)
	t.Logf("%s %v", kinds(rep), err)
	if !strings.Contains(kinds(rep), "REDACTED_UNDER_HOLD") && len(rep.Notes) == 0 {
		t.Fatalf("a pass that began before the hold and replaced records after it was placed is not reported: %s", kinds(rep))
	}
}

// Forged, replayed and wrong-key hold records are reported and change nothing: a hold stays in force
// by the trail, and a hold only a forged record claims is not in force.
func TestReview2_ForgedHoldRecordsChangeNothing(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	if err := r.placeHold("h1", "gov"); err != nil {
		t.Fatal(err)
	}
	mallory := p11Signers(t)["ecdsa-p256"]
	lift := audit.RedactionRecord{Deployment: "test", Phase: audit.RedactionHoldLifted, HoldID: "h1", Reason: "x", Ticket: "x", Executor: "x", AtMs: 1}
	if err := audit.AppendRedactionRecord(ctx, r.trail, lift, mallory); err != nil { // another key lifts h1
		t.Fatal(err)
	}
	if err := r.trail.Append(ctx, audit.RedactionTrailName, 2, unsignedLeaf(t, lift)); err != nil { // unsigned lift
		t.Fatal(err)
	}
	leaves, _ := r.trail.Load(ctx, audit.RedactionTrailName)
	place := audit.RedactionRecord{Deployment: "test", Phase: audit.RedactionHoldPlaced, HoldID: "ghost", HoldMatter: "m", HoldRuns: []string{"gov"}, Reason: "x", Ticket: "x", Executor: "x", AtMs: 1}
	if err := audit.AppendRedactionRecord(ctx, r.trail, place, mallory); err != nil { // a hold only a forged record claims
		t.Fatal(err)
	}
	if err := r.trail.Append(ctx, audit.RedactionTrailName, 4, leaves[0]); err != nil { // replay of the genuine placement
		t.Fatal(err)
	}
	hs, _ := r.store.Holds(ctx)
	all, _ := r.trail.Load(ctx, audit.RedactionTrailName)
	rep, err := audit.VerifyRedactions(audit.RedactionVerifyInput{Deployment: "test", Trail: all, Verifier: r.v, Unanchored: true, Journals: r.journals(), StoreHolds: &hs})
	k := kinds(rep)
	t.Logf("%s | holds by trail: %+v", k, rep.Holds)
	if err == nil || len(rep.Holds) != 1 || rep.Holds[0].ID != "h1" || strings.Contains(k, "HOLD_NOT_IN_STORE") || strings.Contains(k, "HOLD_UNMATCHED") {
		t.Fatalf("forged hold records changed the holds: %s %+v", k, rep.Holds)
	}
}

// HOLD_DIFFERS covers the runs and the tree flag; a hold whose matter was edited in the store is not
// compared, and neither is a partial overlap reported as anything but DIFFERS.
func TestReview2_HoldDiffersCases(t *testing.T) {
	ctx := context.Background()
	for name, edit := range map[string]func(h agent.Hold) agent.Hold{
		"matter":  func(h agent.Hold) agent.Hold { h.Matter = "other matter"; return h },
		"tree":    func(h agent.Hold) agent.Hold { h.Tree = !h.Tree; return h },
		"subset":  func(h agent.Hold) agent.Hold { h.Runs = h.Runs[:1]; return h },
		"overlap": func(h agent.Hold) agent.Hold { h.Runs = []string{h.Runs[0], "extra"}; return h },
	} {
		t.Run(name, func(t *testing.T) {
			r := newTrailRig(t)
			if err := r.placeHold("h1", "gov", "gov2"); err != nil {
				t.Fatal(err)
			}
			hs, _ := r.store.Holds(ctx)
			r.store.LiftHold(ctx, "h1")
			if err := r.store.PlaceHold(ctx, edit(hs[0])); err != nil {
				t.Fatal(err)
			}
			rep, err := r.verifyHolds(true)
			t.Logf("%s: %s %v", name, kinds(rep), err)
			if err == nil || !strings.Contains(kinds(rep), "HOLD_DIFFERS") {
				t.Errorf("an edited hold (%s) is not reported as HOLD_DIFFERS: %s", name, kinds(rep))
			}
		})
	}
}
