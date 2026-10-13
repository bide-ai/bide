package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func sweepClock() *notifyClock {
	return &notifyClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
}

func sweep(t *testing.T, j *Journal, sink Notifier, trail NotifyTrail, clk *notifyClock, opts ...NotifyOption) error {
	t.Helper()
	return NotifyPending(context.Background(), j, sink, trail, append([]NotifyOption{fixedNotifyClock(clk), WithNotifyHolder("sweeper")}, opts...)...)
}

// putMarker journals an attempt marker for tool call id on run, stamped ms.
func putMarker(t *testing.T, j *Journal, run, id string, ms int64) {
	t.Helper()
	if _, err := j.do(context.Background(), run, toolAttemptStep(id), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: id, AttemptedAt: ms}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func putGate(t *testing.T, j *Journal, run string, w ApprovalWait) {
	t.Helper()
	b, _ := json.Marshal(w)
	if _, _, err := j.putEntry(context.Background(), run, ApprovalPendingStep(w.ToolUseID), Record{Kind: StepValue, Result: b}); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyPending_ReportsAGateOnceAndNotAfterItIsDecided(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	j := memJournal()
	putGate(t, j, "r", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Need: 2, Approvers: []string{"a", "b", "c"}})
	trail, sink := NewMemNotifyTrail(), &fakeSink{}

	for range 3 {
		if err := sweep(t, j, sink, trail, clk); err != nil {
			t.Fatal(err)
		}
	}
	if sink.count() != 1 {
		t.Fatalf("the gate was notified %d times over 3 sweeps, want 1", sink.count())
	}
	n := sink.calls[0]
	if n.Kind != NotificationApprovalPending || n.RunID != "r" || n.Approval.ToolName != "refund" || n.Approval.Need != 2 || n.Approval.Of != 3 {
		t.Fatalf("notification = %+v / %+v", n, n.Approval)
	}

	// A gate already decided is not reported, even if its pending record is still there.
	putGate(t, j, "r2", ApprovalWait{ToolUseID: "c9", ToolName: "refund", Single: true})
	if _, _, err := j.putEntry(ctx, "r2", ToolResultStep("c9"), Record{Kind: StepToolResult, ToolUseID: "c9"}); err != nil {
		t.Fatal(err)
	}
	if err := sweep(t, j, sink, trail, clk); err != nil || sink.count() != 1 {
		t.Fatalf("a decided gate was reported: %v calls=%d", err, sink.count())
	}
	if recs, _ := trail.Load(ctx, "notify:r2"); len(recs) != 0 {
		t.Fatalf("the sweep wrote to the trail of a decided gate: %v", recKinds(recs))
	}
}

func TestNotifyPending_SkipsSessionRuns(t *testing.T) {
	clk := sweepClock()
	j := memJournal()
	putGate(t, j, "s>@turn/1", ApprovalWait{ToolUseID: "c1", ToolName: "pay", Single: true})
	sink := &fakeSink{}
	if err := sweep(t, j, sink, NewMemNotifyTrail(), clk); err != nil || sink.count() != 0 {
		t.Fatalf("a session turn's run is the session's to drive: %v, %d reported", err, sink.count())
	}
}

func TestNotifyPending_HaltsWaitForTheirAgeAndSkipWhatIsNotAHalt(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	now := clk.now()
	j := memJournal()
	putMarker(t, j, "old", "c1", now.Add(-10*time.Minute).UnixMilli())
	putMarker(t, j, "young", "c1", now.Add(-time.Minute).UnixMilli())
	putMarker(t, j, "untimed", "c1", 0)
	putMarker(t, j, "edge", "c1", now.Add(-5*time.Minute).UnixMilli()) // exactly the default age
	putMarker(t, j, "done", "c1", now.Add(-time.Hour).UnixMilli())
	if _, _, err := j.putEntry(ctx, "done", ToolResultStep("c1"), Record{Kind: StepToolResult, ToolUseID: "c1"}); err != nil {
		t.Fatal(err)
	}
	// An attempt recorded as never started is not a halt: a resume re-attempts it.
	won, marker, key, err := j.claimNext(ctx, "voided", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: now.Add(-time.Hour).UnixMilli()})
	if err != nil || !won {
		t.Fatal(won, err)
	}
	if err := j.notStarted(ctx, "voided", key, marker); err != nil {
		t.Fatal(err)
	}
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	if err := sweep(t, j, sink, trail, clk); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 2 || sink.calls[0].RunID != "edge" || sink.calls[1].RunID != "old" {
		t.Fatalf("%d halts reported, want the old one and the one exactly at the age: %+v", sink.count(), sink.calls)
	}
	n := sink.calls[1]
	h := n.Halt
	if n.RunID != "old" || n.Kind != NotificationOutcomeUnknown || h.Op.Kind != OpTool || h.Op.ID != "c1" || h.Cause != "" || !h.AttemptedAt.Equal(now.Add(-10*time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("notification = %+v halt = %+v", n, h)
	}
	// The young one is reported once it is old enough.
	clk.advance(5 * time.Minute)
	if err := sweep(t, j, sink, trail, clk); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 3 || sink.calls[2].RunID != "young" {
		t.Fatalf("calls = %d after the marker aged: %+v", sink.count(), sink.calls)
	}
	// The age is configurable.
	sink2 := &fakeSink{}
	if err := sweep(t, j, sink2, NewMemNotifyTrail(), clk, WithNotifyHaltAge(24*time.Hour)); err != nil || sink2.count() != 0 {
		t.Fatalf("with a day's grace: %v, %d reported", err, sink2.count())
	}
}

// A halt the drive already reported and the sweep finds is one event: the keys agree.
func TestNotifyPending_AndADriveShareOneKey(t *testing.T) {
	ctx := context.Background()
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
	j := mustJournal(markerAt(t, time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC).UnixMilli()))
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	a := mustNew(NewScriptedModel(), j, WithTools(charge), WithNotifier(sink, trail))
	if _, err := a.Run(ctx, "r", UserText("go")); !errors.As(err, new(*OutcomeUnknown)) {
		t.Fatalf("Run = %v", err)
	}
	if err := a.FlushNotifications(ctx); err != nil || sink.count() != 1 {
		t.Fatalf("drive: %v, %d", err, sink.count())
	}
	if err := NotifyPending(ctx, j, sink, trail, WithNotifyHaltAge(time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 1 {
		t.Fatalf("the sweep notified an event the drive had: %d calls", sink.count())
	}
	if sink.calls[0].Halt.Op.ToolName != "charge" {
		t.Fatalf("the drive's tool name = %q", sink.calls[0].Halt.Op.ToolName)
	}
}

func TestNotifyPending_ASubRunsEventBelongsToItsRootRun(t *testing.T) {
	clk := sweepClock()
	j := memJournal()
	putGate(t, j, "root>sub", ApprovalWait{ToolUseID: "c1", ToolName: "pay", Single: true})
	putMarker(t, j, "root>sub2", "c2", clk.now().Add(-time.Hour).UnixMilli())
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	if err := sweep(t, j, sink, trail, clk); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 2 || sink.calls[0].RunID != "root" || sink.calls[0].Key != ApprovalKey("root>sub", "c1", 0) {
		t.Fatalf("calls = %+v", sink.calls)
	}
	if h := sink.calls[1]; h.RunID != "root" || h.Key != HaltedKey("root>sub2", OpTool, "c2", clk.now().Add(-time.Hour).UnixMilli()) {
		t.Fatalf("a sub-run halt is keyed to the sub-run, as the drive keys it: %+v", h)
	}
	if recs, _ := trail.Load(context.Background(), "notify:root"); len(recs) == 0 {
		t.Fatal("the event is not on the root run's trail")
	}
}

// The tool's name comes from the model turn that called it.
func TestNotifyPending_NamesTheTool(t *testing.T) {
	j := mustJournal(markerAt(t, sweepClock().now().Add(-time.Hour).UnixMilli()))
	sink := &fakeSink{}
	if err := sweep(t, j, sink, NewMemNotifyTrail(), sweepClock()); err != nil || sink.count() != 1 || sink.calls[0].Halt.Op.ToolName != "charge" {
		t.Fatalf("%v %+v", err, sink.calls)
	}
}

func TestNotifyPending_AFailedDeliveryIsReportedAndTheRestStillGoOut(t *testing.T) {
	clk := sweepClock()
	j := memJournal()
	putGate(t, j, "a", ApprovalWait{ToolUseID: "c1", ToolName: "t", Single: true})
	putGate(t, j, "b", ApprovalWait{ToolUseID: "c1", ToolName: "t", Single: true})
	trail, sink := NewMemNotifyTrail(), &fakeSink{fail: 3} // all 3 attempts of the first run's round
	err := sweep(t, j, sink, trail, clk)
	if err == nil || !strings.Contains(err.Error(), "run a") {
		t.Fatalf("err = %v, want the failed run named", err)
	}
	if sink.count() != 4 || sink.calls[3].RunID != "b" {
		t.Fatalf("the second run was not delivered after the first failed: %d calls", sink.count())
	}
	// The next sweep retries the one that failed, and only that, once its claim has lapsed.
	clk.advance(3 * time.Minute)
	if err := sweep(t, j, sink, trail, clk); err != nil || sink.count() != 5 || sink.calls[4].RunID != "a" {
		t.Fatalf("second sweep: %v, calls=%d", err, sink.count())
	}
}

func TestNotifyPending_Config(t *testing.T) {
	ctx := context.Background()
	if err := NotifyPending(ctx, nil, &fakeSink{}, NewMemNotifyTrail()); !errors.Is(err, ErrConfig) {
		t.Errorf("nil journal: %v", err)
	}
	if err := NotifyPending(ctx, memJournal(), nil, NewMemNotifyTrail()); !errors.Is(err, ErrConfig) {
		t.Errorf("nil notifier: %v", err)
	}
	if err := NotifyPending(ctx, memJournal(), &fakeSink{}, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("nil trail: %v", err)
	}
	if err := NotifyPending(ctx, memJournal(), &fakeSink{}, NewMemNotifyTrail(), WithNotifyHaltAge(0)); !errors.Is(err, ErrConfig) {
		t.Errorf("zero halt age: %v", err)
	}
}

// A drive's notifier and the sweep in one process, configured with one holder name, are two
// holders: while the drive's delivery is in flight under its live claim, the sweep does not send.
func TestNotifyPending_AndADriveInOneProcessAreTwoHolders(t *testing.T) {
	ctx := context.Background()
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
	j := mustJournal(markerAt(t, time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC).UnixMilli()))
	trail := NewMemNotifyTrail()
	block, entered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	slow := notifierFunc(func(context.Context, Notification) error {
		once.Do(func() { close(entered) })
		<-block
		return nil
	})
	a := mustNew(NewScriptedModel(), j, WithTools(charge), WithNotifier(slow, trail, WithNotifyHolder("worker-1")))
	if _, err := a.Run(ctx, "r", UserText("go")); !errors.As(err, new(*OutcomeUnknown)) {
		t.Fatalf("Run = %v", err)
	}
	<-entered
	sweepSink := &fakeSink{}
	if err := NotifyPending(ctx, j, sweepSink, trail, WithNotifyHolder("worker-1"), WithNotifyHaltAge(time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if sweepSink.count() != 0 {
		t.Fatal("the sweep delivered under the drive's live claim")
	}
	close(block)
	if err := a.FlushNotifications(ctx); err != nil {
		t.Fatal(err)
	}
}
