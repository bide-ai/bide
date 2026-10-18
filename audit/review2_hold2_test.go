package audit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

type afterHolds struct {
	agent.Holder
	once func()
}

func (h *afterHolds) Holds(ctx context.Context) ([]agent.Hold, error) {
	if h.once != nil {
		f := h.once
		h.once = nil
		f()
	}
	return h.Holder.Holds(ctx)
}

// Two operators place the same hold at once: the second read the trail before the first recorded.
func TestReview2_ConcurrentSamePlacementLeavesNoFinding(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	h := agent.Hold{ID: "h1", Matter: "m", Runs: []string{"gov"}}
	b := &afterHolds{Holder: r.store}
	b.once = func() {
		if err := audit.PlaceLegalHold(ctx, r.trail, r.store, r.signer, "test", nil, h, "r", "t", "A", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	err := audit.PlaceLegalHold(ctx, r.trail, b, r.signer, "test", nil, h, "r", "t", "B", time.Now())
	rep, verr := r.verifyHolds(true)
	t.Logf("B: %v; audit: %s %v", err, kinds(rep), verr)
	if verr != nil {
		t.Fatalf("a concurrent identical placement leaves findings: %s", kinds(rep))
	}
}

// Two operators lift the same hold at once.
func TestReview2_ConcurrentSameLiftLeavesNoFinding(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	if err := r.placeHold("h1", "gov"); err != nil {
		t.Fatal(err)
	}
	b := &afterHolds{Holder: r.store}
	b.once = func() {
		if err := audit.LiftLegalHold(ctx, r.trail, r.store, r.signer, "test", nil, "h1", "r", "t", "A", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	err := audit.LiftLegalHold(ctx, r.trail, b, r.signer, "test", nil, "h1", "r", "t", "B", time.Now())
	rep, verr := r.verifyHolds(true)
	t.Logf("B: %v; audit: %s %v", err, kinds(rep), verr)
	if verr != nil {
		t.Fatalf("a concurrent lift leaves findings: %s", kinds(rep))
	}
	if err == nil {
		t.Error("the second lift reports success though the first one did it and recorded it")
	}
}

func TestReview2_SpanNoteAndFileModeNote(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	begin := audit.RedactionRecord{Deployment: "test", Phase: audit.RedactionBegin, PlanDigest: "p", RunID: "gov", Reason: "r", Ticket: "t", Executor: "e", AtMs: 1_700_000_000_000, HeadSize: 1}
	for _, p := range r.plan.Records {
		begin.Records = append(begin.Records, audit.RedactedRef{Position: p.Position, Name: p.Name, Kind: string(p.Kind), Length: p.Length, LeafHash: p.LeafHash})
	}
	if err := audit.AppendRedactionRecord(ctx, r.trail, begin, r.signer); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Redact(ctx, r.j, r.plan, time.UnixMilli(1_700_000_000_000)); err != nil {
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
	if err != nil || !strings.Contains(strings.Join(rep.Notes, "|"), "began before hold") {
		t.Fatalf("span note: %v %+v", err, rep)
	}
	leaves, _ := r.trail.Load(ctx, audit.RedactionTrailName)
	rep, _ = audit.VerifyRedactions(audit.RedactionVerifyInput{Deployment: "test", Trail: leaves, Verifier: r.v, Unanchored: true, Journals: r.journals()})
	if !strings.Contains(strings.Join(rep.Notes, "|"), "not compared with a store") {
		t.Fatalf("file-mode note: %+v", rep.Notes)
	}
}
