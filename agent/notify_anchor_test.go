package agent

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
)

// anchoringTrail is a NotifyTrail that records the AnchorTrails calls the notifier makes.
type anchoringTrail struct {
	*MemNotifyTrail
	mu    sync.Mutex
	calls [][]string
	err   error
}

func (a *anchoringTrail) AnchorTrails(_ context.Context, trails []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, slices.Clone(trails))
	return a.err
}

func (a *anchoringTrail) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, c := range a.calls {
		out = append(out, c...)
	}
	slices.Sort(out)
	return out
}

// The sweep asks the trail to sign the trail of every run it looked at, once each, after the
// deliveries, with sub-runs attributed to their root.
func TestNotifyPending_AsksTheTrailToAnchorTheTrailsItLookedAt(t *testing.T) {
	clk := sweepClock()
	j := memJournal()
	putGate(t, j, "r1", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	putGate(t, j, "r1>sub", ApprovalWait{ToolUseID: "c2", ToolName: "refund", Single: true})
	putGate(t, j, "r2", ApprovalWait{ToolUseID: "c3", ToolName: "refund", Single: true})
	putGate(t, j, "s>@turn/1", ApprovalWait{ToolUseID: "c4", ToolName: "pay", Single: true}) // a session's: skipped
	trail := &anchoringTrail{MemNotifyTrail: NewMemNotifyTrail()}

	if err := sweep(t, j, &fakeSink{}, trail, clk); err != nil {
		t.Fatal(err)
	}
	if got, want := trail.all(), []string{NotifyDigestTrail, NotifyOpenTrail, "notify:r1", "notify:r2"}; !slices.Equal(got, want) {
		t.Fatalf("anchored %v, want %v", got, want)
	}
	if len(trail.calls) != 1 {
		t.Fatalf("%d AnchorTrails calls, want one for the whole sweep", len(trail.calls))
	}
	// After the deliveries: the trail the call names already holds its records.
	if recs, _ := trail.Load(context.Background(), "notify:r1"); len(recs) == 0 {
		t.Fatal("the trail was empty when it was anchored")
	}
}

// A failure to anchor is reported to WithNotifyErrors and is neither the sweep's error nor a
// failed delivery.
func TestNotifyPending_AnAnchorFailureIsReportedNotReturned(t *testing.T) {
	clk := sweepClock()
	j := memJournal()
	putGate(t, j, "r", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	boom := errors.New("anchor down")
	trail := &anchoringTrail{MemNotifyTrail: NewMemNotifyTrail(), err: boom}
	var mu sync.Mutex
	var reported []error
	sink := &fakeSink{}
	err := sweep(t, j, sink, trail, clk, WithNotifyErrors(func(err error) { mu.Lock(); reported = append(reported, err); mu.Unlock() }))
	if err != nil {
		t.Fatalf("sweep = %v: an anchor failure is not a delivery failure", err)
	}
	if sink.count() != 1 {
		t.Fatalf("delivered %d times, want 1", sink.count())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 || !errors.Is(reported[0], boom) {
		t.Fatalf("reported %v, want the anchor error once", reported)
	}
}

// A trail that cannot anchor is simply not asked.
func TestNotifyPending_APlainTrailIsNotAnchored(t *testing.T) {
	j := memJournal()
	putGate(t, j, "r", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	if err := sweep(t, j, &fakeSink{}, NewMemNotifyTrail(), sweepClock()); err != nil {
		t.Fatal(err)
	}
}

// A recovery pass asks the trail to sign the trail of each run it recovered, root-attributed.
func TestRecover_AsksTheTrailToAnchorEachRunsTrail(t *testing.T) {
	ctx := context.Background()
	tool := MustFunc("refund", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithApproval(SingleApproval()))
	j := memJournal()
	if _, err := mustNew(NewScriptedModel(ToolTurn("c1", "refund", `{}`)), j, WithTools(tool)).Run(ctx, "r", UserText("go")); !errors.As(err, new(*ApprovalPending)) {
		t.Fatalf("setup Run = %v", err)
	}
	trail := &anchoringTrail{MemNotifyTrail: NewMemNotifyTrail()}
	resume := func(context.Context, string, RunStart) error {
		return &ApprovalPending{RunRef: RunRef{RunID: "r", RootRunID: "r"}, ToolUseID: "c1", ToolName: "refund"}
	}
	if _, err := Recover(ctx, j, resume, WithNotifier(&fakeSink{}, trail)); err != nil && !errors.As(err, new(*ApprovalPending)) {
		t.Fatal(err)
	}
	if got := trail.all(); !slices.Equal(got, []string{"notify:r"}) {
		t.Fatalf("the recovery pass anchored %v, want [notify:r]", got)
	}
}

// Flushing the notifier, which a process does as it stops, asks the trail to sign what the last
// deliveries wrote.
func TestFlush_AsksTheTrailToAnchorWhatItWrote(t *testing.T) {
	trail := &anchoringTrail{MemNotifyTrail: NewMemNotifyTrail()}
	e := testEngine(t, memJournal(), &fakeSink{}, trail, &notifyClock{t: sweepClock().t}, "p")
	if err := e.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(trail.calls) != 1 {
		t.Fatalf("%d AnchorTrails calls on flush, want 1", len(trail.calls))
	}
}
