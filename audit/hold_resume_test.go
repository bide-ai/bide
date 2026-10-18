package audit_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

type liftFails struct {
	agent.Holder
	fails int
}

func (l *liftFails) LiftHold(ctx context.Context, id string) (bool, error) {
	if l.fails > 0 {
		l.fails--
		return false, errors.New("store down")
	}
	return l.Holder.LiftHold(ctx, id)
}

// A placement whose record could not be written leaves a hold in force and unrecorded. Placing it
// again must be able to record it; otherwise HOLD_NOT_IN_TRAIL can only be cleared by lifting the hold.
func TestLegalHold_UnrecordedPlacementIsRecordedByPlacingAgain(t *testing.T) {
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
func TestLegalHold_ResumedLiftLeavesNoUnmatchedRecord(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	if err := r.placeHold("h1", "gov"); err != nil {
		t.Fatal(err)
	}
	flaky := &liftFails{Holder: r.store, fails: 1}
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
func TestLegalHold_PassSpanningAHoldPlacementIsNoted(t *testing.T) {
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
	if !strings.Contains(kinds(rep), "REDACTED_UNDER_HOLD") && !slices.ContainsFunc(rep.Notes, func(n string) bool { return strings.Contains(n, "began before hold h1") }) {
		t.Fatalf("a pass that began before the hold and replaced records after it was placed is not reported: %s", kinds(rep))
	}
}

// Forged, replayed and wrong-key hold records are reported and change nothing: a hold stays in force
// by the trail, and a hold only a forged record claims is not in force.
func TestLegalHold_ForgedHoldRecordsChangeNothing(t *testing.T) {
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

// A hold the trail shows in force and the store lacks (lifted by hand) can be recorded as lifted.
func TestLegalHold_LiftRecordsAHoldTheStoreLacks(t *testing.T) {
	r := newTrailRig(t)
	if err := r.placeHold("h1", "gov"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.LiftHold(context.Background(), "h1"); err != nil {
		t.Fatal(err)
	}
	if rep, _ := r.verifyHolds(true); !strings.Contains(kinds(rep), "HOLD_NOT_IN_STORE") {
		t.Fatalf("setup: %s", kinds(rep))
	}
	if err := r.liftHold("h1"); err != nil {
		t.Fatalf("recording the lift of a hold the store lacks: %v", err)
	}
	if rep, err := r.verifyHolds(true); err != nil || len(rep.Holds) != 0 {
		t.Fatalf("after the lift: %+v %v", rep, err)
	}
}

// Placing again never overwrites a different hold, and a hold placed outside the tool is not lifted
// without a record.
func TestLegalHold_ResumeRefusesWhatItCannotMatch(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	if err := r.store.PlaceHold(ctx, agent.Hold{ID: "h1", Matter: "other matter", Runs: []string{"gov"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.placeHold("h1", "gov"); !errors.Is(err, agent.ErrHoldExists) {
		t.Fatalf("placing over a different store hold = %v, want ErrHoldExists", err)
	}
	if err := r.liftHold("h1"); err == nil || !strings.Contains(err.Error(), "never recorded") {
		t.Fatalf("lifting a hold the trail never recorded = %v", err)
	}
	if hs, _ := r.store.Holds(ctx); len(hs) != 1 {
		t.Fatalf("the hold left the store: %+v", hs)
	}
	if err := r.liftHold("nope"); err == nil {
		t.Fatal("lifted a hold nobody holds")
	}
	// Placing twice through the tool is refused.
	r2 := newTrailRig(t)
	if err := r2.placeHold("h2", "gov"); err != nil {
		t.Fatal(err)
	}
	if err := r2.placeHold("h2", "gov"); !errors.Is(err, agent.ErrHoldExists) {
		t.Fatalf("placing an active hold again = %v", err)
	}
}

// Without a store to compare with, the report says so.
func TestLegalHold_FileModeSaysHoldsWereNotCompared(t *testing.T) {
	r := newTrailRig(t)
	if err := r.placeHold("h1", "gov"); err != nil {
		t.Fatal(err)
	}
	rep, _ := r.verifyHolds(false)
	if !slices.ContainsFunc(rep.Notes, func(n string) bool { return strings.Contains(n, "not compared with a store") }) {
		t.Fatalf("notes = %q", rep.Notes)
	}
	rep, _ = r.verifyHolds(true)
	if slices.ContainsFunc(rep.Notes, func(n string) bool { return strings.Contains(n, "not compared with a store") }) {
		t.Fatalf("notes with a store = %q", rep.Notes)
	}
}
