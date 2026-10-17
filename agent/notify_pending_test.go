package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	clk.advance(4*time.Minute + 10*time.Second) // past the claim and the skew margin, before the deadline
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

func putStart(t *testing.T, j *Journal, run string, at time.Time) {
	t.Helper()
	b, _ := json.Marshal(RunStart{StartedAt: at.UTC().Format(time.RFC3339Nano)})
	if _, _, err := j.putEntry(context.Background(), run, runStartStep, Record{Kind: StepValue, Result: b}); err != nil {
		t.Fatal(err)
	}
}

func putAbort(t *testing.T, j *Journal, run string) {
	t.Helper()
	if _, _, err := j.putEntry(context.Background(), run, runAbortedStep, Record{Kind: StepValue, Result: []byte(`"rolled back"`)}); err != nil {
		t.Fatal(err)
	}
}

// The sweep raises run_aborted only for a recent abort, by the run's start (the abort records no
// time), so enabling it does not page for every run that ever aborted.
func TestNotifyPending_AbortedRunsAreBoundedByAge(t *testing.T) {
	clk := sweepClock()
	j := memJournal()
	putStart(t, j, "recent", clk.now().Add(-2*time.Hour))
	putAbort(t, j, "recent")
	putStart(t, j, "old", clk.now().Add(-48*time.Hour))
	putAbort(t, j, "old")
	putAbort(t, j, "untimed") // no run:start: its age is unknown
	sink := &fakeSink{}
	if err := sweep(t, j, sink, NewMemNotifyTrail(), clk); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 1 || sink.calls[0].RunID != "recent" || sink.calls[0].Kind != NotificationRunAborted {
		t.Fatalf("default window: %+v", sink.calls)
	}
	wide := &fakeSink{}
	if err := sweep(t, j, wide, NewMemNotifyTrail(), clk, WithNotifyAbortedWithin(72*time.Hour)); err != nil || wide.count() != 2 {
		t.Fatalf("a 72 hour window: %v, %d notifications, want recent and old", err, wide.count())
	}
	if err := sweep(t, j, &fakeSink{}, NewMemNotifyTrail(), clk, WithNotifyAbortedWithin(0)); !errors.Is(err, ErrConfig) {
		t.Fatalf("a zero window: %v", err)
	}
}

// failGetStore fails Get while armed.
type failGetStore struct {
	Store
	armed atomic.Bool
}

func (f *failGetStore) Unwrap() Store { return f.Store }
func (f *failGetStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if f.armed.Load() {
		return Entry{}, false, errors.New("store blip")
	}
	return f.Store.Get(ctx, runID, name)
}

// If the attempt marker of an untimed halt cannot be read, the event is not queued under a guessed
// key (which could be the previous attempt's, and so dropped as already acknowledged); the next
// drive or sweep raises it.
func TestNotifier_AHaltWhoseMarkerCannotBeReadHasNoKeyUntilItCan(t *testing.T) {
	st := &failGetStore{Store: markerAt(t, 0)}
	j := mustJournal(st)
	e := testEngine(t, j, &fakeSink{}, NewMemNotifyTrail(), &notifyClock{t: time.Now()}, "p")
	halt := &OutcomeUnknown{RunRef: RunRef{RunID: "r", RootRunID: "r"}, Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
	ev, ok := e.eventFromErr("r", halt) // no read of the journal here: a drive never waits on the store
	if !ok || ev.keyFn == nil {
		t.Fatalf("the halt was not queued with a lazy key: %+v", ev)
	}
	st.armed.Store(true)
	if k, err := ev.keyFn(context.Background()); err == nil {
		t.Fatalf("a key %q was made from a marker that could not be read", k)
	}
	st.armed.Store(false)
	if k, err := ev.keyFn(context.Background()); err != nil || k == "" {
		t.Fatalf("key = %q, %v once the marker could be read", k, err)
	}
}

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
	if got, want := trail.all(), []string{NotifyDigestTrail, OpenSegment(clk.now()), OpenIndex(clk.now().Year()), "notify:r1", "notify:r2"}; !slices.Equal(got, want) {
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
	// The run's own trail and the global ones (the first pass is due for them).
	if got := trail.all(); !slices.Contains(got, "notify:r") || !slices.Contains(got, NotifyDigestTrail) || !slices.Contains(got, OpenSegment(time.Now())) {
		t.Fatalf("the recovery pass anchored %v, want notify:r and the global trails", got)
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

// A recovery pass signs the global trails again once a minute, not once per run it visits.
func TestRecover_SignsTheGlobalTrailsOncePerInterval(t *testing.T) {
	clk := &notifyClock{t: sweepClock().t}
	trail := &anchoringTrail{MemNotifyTrail: NewMemNotifyTrail()}
	e := testEngine(t, memJournal(), &fakeSink{}, trail, clk, "p")
	for _, run := range []string{"a", "b", "c"} {
		e.afterRecover(context.Background(), run, true, true, nil)
	}
	count := func() int {
		n := 0
		for _, c := range trail.all() {
			if c == NotifyDigestTrail {
				n++
			}
		}
		return n
	}
	if count() != 1 {
		t.Fatalf("the digest trail was named %d times by 3 runs in one interval, want 1", count())
	}
	clk.advance(2 * time.Minute)
	e.afterRecover(context.Background(), "d", true, true, nil)
	if count() != 2 {
		t.Fatalf("after the interval the digest trail was named %d times, want 2", count())
	}
}

// Just after New Year the newest segments are in last year's index, and are still signed.
func TestGlobalTrails_FollowTheIndexAcrossNewYear(t *testing.T) {
	ctx := context.Background()
	trail := &anchoringTrail{MemNotifyTrail: NewMemNotifyTrail()}
	clk := &notifyClock{t: time.Date(2027, 1, 3, 12, 0, 0, 0, time.UTC)}
	e := testEngine(t, memJournal(), &fakeSink{}, trail, clk, "p")
	last := OpenSegment(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
	if err := trail.Append(ctx, OpenIndex(2026), 0, NotifyRecord{Kind: NotifySegment, At: clk.now(), Next: last}); err != nil {
		t.Fatal(err)
	}
	got := e.globalTrails(ctx)
	for _, want := range []string{OpenIndex(2026), last, OpenSegment(clk.now()), NotifyDigestTrail} {
		if !slices.Contains(got, want) {
			t.Errorf("globalTrails = %v, lacks %s", got, want)
		}
	}
}

func journalEventByKey(evs []JournalNotifyEvent, key string) *JournalNotifyEvent {
	for i := range evs {
		if evs[i].Key == key {
			return &evs[i]
		}
	}
	return nil
}

// A gate the run paused at is an event whether or not it was decided since; Current says which.
func TestJournalNotifyEvents_GatesAndDecisions(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	at := time.Date(2026, 10, 13, 9, 0, 0, 123, time.UTC)
	putGate(t, j, "r", ApprovalWait{ToolUseID: "open", ToolName: "refund", Single: true, RecordedAt: at.Format(time.RFC3339Nano)})
	putGate(t, j, "r", ApprovalWait{ToolUseID: "done", ToolName: "refund", Single: true})
	if _, _, err := j.putEntry(ctx, "r", ToolResultStep("done"), Record{Kind: StepToolResult, ToolUseID: "done"}); err != nil {
		t.Fatal(err)
	}
	recs, err := j.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	evs := JournalNotifyEvents("r", recs)
	if len(evs) != 2 {
		t.Fatalf("%d events, want 2: %+v", len(evs), evs)
	}
	open, done := journalEventByKey(evs, ApprovalKey("r", "open", 0)), journalEventByKey(evs, ApprovalKey("r", "done", 0))
	if open == nil || !open.Current || open.Kind != NotificationApprovalPending || open.RunID != "r" || !open.At.Equal(at) {
		t.Fatalf("open gate = %+v", open)
	}
	if done == nil || done.Current {
		t.Fatalf("a decided gate must be returned, not current: %+v", done)
	}
}

// A live attempt marker with no result is a halt at its marker time; a result, a not-started record
// or an untimed marker is not one. A sub-run's event belongs to the root run's trail.
func TestJournalNotifyEvents_HaltsAndSubRuns(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	ms := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC).UnixMilli()
	putMarker(t, j, "r>sub", "c1", ms)
	putMarker(t, j, "done", "c1", ms)
	if _, _, err := j.putEntry(ctx, "done", ToolResultStep("c1"), Record{Kind: StepToolResult, ToolUseID: "c1"}); err != nil {
		t.Fatal(err)
	}
	putMarker(t, j, "untimed", "c1", 0)
	for run, want := range map[string]int{"r>sub": 1, "done": 0, "untimed": 0} {
		recs, _ := j.History(ctx, run)
		evs := JournalNotifyEvents(run, recs)
		if len(evs) != want {
			t.Fatalf("%s: %d events, want %d: %+v", run, len(evs), want, evs)
		}
		if want == 1 {
			e := evs[0]
			if e.Key != HaltedKey("r>sub", OpTool, "c1", ms) || e.Kind != NotificationOutcomeUnknown || e.RunID != "r" || !e.Current || e.At.UnixMilli() != ms {
				t.Fatalf("halt = %+v", e)
			}
		}
	}
}

// An aborted run is an event of its root run, with the abort time when the journal has one.
func TestJournalNotifyEvents_Abort(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	if _, _, err := j.putEntry(ctx, "r", runAbortedStep, Record{Kind: StepValue, Result: []byte(`{"aborted_at":"2026-10-13T09:00:00Z"}`)}); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(ctx, "r")
	evs := JournalNotifyEvents("r", recs)
	if len(evs) != 1 || evs[0].Key != AbortedKey("r") || evs[0].Kind != NotificationRunAborted || evs[0].At.IsZero() {
		t.Fatalf("events = %+v", evs)
	}
}

// The keys JournalNotifyEvents returns are the keys the notifier's sweep writes, for a gate, a halt
// and an abort, so an auditor comparing the two finds a missing record and nothing else.
func TestJournalNotifyEvents_KeysMatchTheSweep(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	j := memJournal()
	putGate(t, j, "g", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	putMarker(t, j, "h", "c1", clk.now().Add(-time.Hour).UnixMilli())
	if _, _, err := j.putEntry(ctx, "a", runAbortedStep, Record{Kind: StepValue, Result: []byte(`{"aborted_at":"` + clk.now().Add(-time.Minute).Format(time.RFC3339Nano) + `"}`)}); err != nil {
		t.Fatal(err)
	}
	trail := NewMemNotifyTrail()
	if err := sweep(t, j, &fakeSink{}, trail, clk); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"g", "h", "a"} {
		recs, _ := j.History(ctx, run)
		evs := JournalNotifyEvents(run, recs)
		if len(evs) != 1 {
			t.Fatalf("%s: %+v", run, evs)
		}
		st := FoldNotify(mustLoad(t, trail, NotifyTrailName(evs[0].RunID)))
		if st.Events[evs[0].Key] == nil {
			t.Fatalf("%s: the sweep wrote no event under key %q (trail has %v)", run, evs[0].Key, st.Keys)
		}
	}
}

func mustLoad(t *testing.T, tr NotifyTrail, name string) []NotifyRecord {
	t.Helper()
	recs, err := tr.Load(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// A durable step's attempt marker with no result is a halt of kind step, keyed by the step's name.
func TestJournalNotifyEvents_StepHalt(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	ms := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC).UnixMilli()
	if _, err := j.do(ctx, "r", stepAttemptStep("pay"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, AttemptedAt: ms}, nil
	}); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(ctx, "r")
	evs := JournalNotifyEvents("r", recs)
	if len(evs) != 1 || evs[0].Key != HaltedKey("r", OpStep, "pay", ms) || evs[0].Kind != NotificationOutcomeUnknown {
		t.Fatalf("events = %+v, want the step halt %q", evs, HaltedKey("r", OpStep, "pay", ms))
	}
}

// A sub-run's gate is keyed by the sub-run, as the notifier keys it, and belongs to the root's trail;
// a sub-run's abort is not an event of its own (the root run reports its end).
func TestJournalNotifyEvents_SubRunGateAndAbort(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	sub := SubRunID("r", "c0")
	putGate(t, j, sub, ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	if _, _, err := j.putEntry(ctx, sub, runAbortedStep, Record{Kind: StepValue, Result: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(ctx, sub)
	evs := JournalNotifyEvents(sub, recs)
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want only the gate", evs)
	}
	if e := evs[0]; e.Key != ApprovalKey(sub, "c1", 0) || e.RunID != "r" || e.Kind != NotificationApprovalPending {
		t.Fatalf("gate = %+v", e)
	}
}

// A plan flow's run (its run:start records kind flow) is its own root even when its ID holds '>',
// as the notifier files its events; any other run with '>' is its tree root's.
func TestJournalNotifyEvents_AFlowRunIsItsOwnRoot(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	flow, plain := "f>x", "p>x"
	for _, run := range []string{flow, plain} {
		kind := `{"kind":"flow"}`
		if run == plain {
			kind = `{"kind":"agent"}`
		}
		if _, _, err := j.putEntry(ctx, run, runStartStep, Record{Kind: StepValue, Result: []byte(kind)}); err != nil {
			t.Fatal(err)
		}
		putGate(t, j, run, ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
		if _, _, err := j.putEntry(ctx, run, runAbortedStep, Record{Kind: StepValue, Result: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	for run, want := range map[string]string{flow: flow, plain: "p"} {
		recs, _ := j.History(ctx, run)
		evs := JournalNotifyEvents(run, recs)
		if len(evs) < 1 || evs[0].RunID != want {
			t.Fatalf("%s: events %+v, want root %q", run, evs, want)
		}
		// An abort is the root run's event, so the flow reports its own and a sub-run's is not one.
		aborts := 0
		for _, e := range evs {
			if e.Kind == NotificationRunAborted {
				aborts++
			}
		}
		if (run == flow) != (aborts == 1) {
			t.Fatalf("%s: %d abort events", run, aborts)
		}
	}
}

// The recheck of deferred events in a flush shares one 2s budget: with the trail hung and many
// events deferred to a peer, a flush with no deadline returns in about 2s, not 2s per event.
func TestFlushRecheckSharesOneBudget(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail := &blockingLoadTrail{MemNotifyTrail: NewMemNotifyTrail()}
	j := memJournal()
	down := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}
	peer := testEngine(t, j, down, trail, clk, "recover")
	e := testEngine(t, j, down, trail, clk, "drive")
	const n = 5
	for i := range n {
		ev := haltEvent()
		ev.runID = fmt.Sprintf("r%d", i)
		ev.key = HaltedKey(ev.runID, OpTool, "c1", 100)
		_, _ = peer.round(ctx, ev) // the peer holds a live claim
		e.w.submit(ev)
	}
	if !pollUntil(2*time.Second, func() bool { return e.w.settled() }) {
		t.Fatal("setup: the rounds did not defer")
	}
	trail.block.Store(true)
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- e.flush(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("flush reported nothing for events it could not recheck")
		}
	case <-time.After(5 * time.Second):
		trail.block.Store(false)
		t.Fatalf("flush with %d deferred events and a hung trail did not return in 5s (each recheck has its own 2s)", n)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("flush took %v, want about 2s", d)
	}
}

// A holder whose own round is slow does not send the overdue notice while a peer's notice claim on
// the notice key is live: the single duplicate of a slow holder and a peer is zero.
func TestNotifyRound_AHolderSkipsTheNoticeWhilePeersNoticeClaimIsLive(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail, j, ev := NewMemNotifyTrail(), memJournal(), haltEvent()
	down, overdue := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}, &fakeSink{fail: 1, err: ErrNotifyPermanent}
	opts := []NotifyOption{WithOverdueNotifier(overdue), WithNotifyClaimTTL(time.Hour)}
	holder := testEngine(t, j, down, trail, clk, "holder", opts...)
	_, _ = holder.round(ctx, ev) // claims before the deadline, then its round is slow
	clk.advance(6 * time.Minute)
	_, _ = testEngine(t, j, down, trail, clk, "peer", opts...).round(ctx, ev) // overdue record, then the peer's notice claim; its send fails
	if overdue.count() != 1 {
		t.Fatalf("setup: %d notice attempts by the peer, want 1", overdue.count())
	}
	clk.advance(10 * time.Second) // inside the peer's notice claim; the holder's own claim is still live
	_, _ = holder.round(ctx, ev)
	if overdue.count() != 1 {
		t.Fatalf("the holder sent the notice inside a peer's live notice claim: %d attempts, want 1", overdue.count())
	}
	clk.advance(20 * time.Minute) // the notice claim has lapsed; the holder's claim lives an hour: the notice is owed again
	_, _ = holder.round(ctx, ev)
	if overdue.count() != 2 {
		t.Fatalf("%d notice attempts after the peer's claim lapsed, want the holder's retry", overdue.count())
	}
}

// Composition of the re-ask note with the notifier's catch-up scan. A re-ask event delivered to approvers must not be cleared
// by the catch-up scan while the gate is still waiting on the re-ask: eventFromKey rebuilds the
// event from its trail key without Reask, so current() asks the first-ask question (is there a
// tally?) and the weak tally the gate does not act on makes it "decided".
func TestReask_ScanDoesNotClearAStandingReask(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	err := weakThenStrong(t, store)
	clk := resolvedClock()
	sink, trail := &fakeSink{}, NewMemNotifyTrail()
	e := testEngine(t, store, sink, trail, clk, "p1")
	ev, ok := e.eventFromErr("r1", err)
	if !ok || ev.approval == nil || ev.approval.Reask == "" {
		t.Fatalf("no re-ask event from %v", err)
	}
	if res, rerr := e.round(ctx, ev); rerr != nil || !res.done {
		t.Fatal(res, rerr)
	}
	clk.advance(1)
	scanAndFlush(t, e, "r1")
	if got := resolvedCalls(sink); len(got) != 0 {
		t.Fatalf("an all-clear (%s) was sent for a re-ask the gate is still waiting on", got[0].Reason)
	}
	for _, r := range trailOf(t, trail, "r1", ev.key) {
		if r.Kind == NotifyCleared {
			t.Fatalf("the re-ask event %s was cleared (%q) while its gate still waits", ev.key, r.Reason)
		}
	}
}

// Tests of a re-ask (ApprovalReaskStep) in the notifier and the journal readers: the key names the
// ask, routing and escalation see the policy the gate asks under, and a note the journal does not
// prove is not read.

// weakNeedThenStrong pauses r1's call c1 under a 1-of-3 policy, records the tally its one approval
// passes, and drives it again under a 2-of-3 policy that excludes the requester: the gate re-asks, and
// the re-ask note's policy differs from the first note's in what it needs.
func weakNeedThenStrong(t *testing.T, store *Journal) error {
	t.Helper()
	vf := fakeVerifiers(abc...)
	var charged int
	weak := &ApprovalPolicy{Need: 1, Approvers: abc}
	_, _ = sodRun(store, "r1", true, weak, vf, &charged, Identity{OnBehalfOf: "alice"})
	approveAs(t, store, "r1", "c1", "alice", true)
	if _, final, err := tallyGate(context.Background(), store, "r1", subjectOf(t, store, "r1", "c1"), weak, vf, ""); err != nil || !final {
		t.Fatalf("the old deployment's tally: %v %v", final, err)
	}
	_, err := sodRun(store, "r1", false, sodPolicy(2), vf, &charged, Identity{})
	if charged != 0 {
		t.Fatal("the call ran")
	}
	return err
}

func reaskByName(t *testing.T, store *Journal) map[string]Record {
	t.Helper()
	return valuesByName(mustHistory(t, store, "r1"))
}

// The key of a re-ask names which ask it is, whether or not it is addressed to one receiver, and the
// event rebuilt from it asks the re-ask's question.
func TestReask_EventFromKeyKeepsTheReason(t *testing.T) {
	for _, tc := range []struct {
		key, want string
	}{
		{ApprovalReaskKey("r1", "c1", true), PendingSoDReapproval},
		{ApprovalReaskKey("r1", "c1", false), PendingReapproval},
		{ToKey("risk", ApprovalReaskKey("r1", "c1", true)), PendingSoDReapproval},
		{ApprovalKey("r1", "c1", 0), ""},
		{ApprovalKey("r1", "c1", 2), ""},
	} {
		ev, ok := eventFromKey(&NotifyEvent{Key: tc.key, Kind: string(NotificationApprovalPending), RunID: "r1"})
		if !ok || ev.approval == nil || ev.approval.Reask != tc.want || ev.approval.ToolUseID != "c1" {
			t.Fatalf("eventFromKey(%q) = %+v (%+v), %v; want Reask %q", tc.key, ev, ev.approval, ok, tc.want)
		}
	}
	// The same through the operation fields a trail record carries.
	ev, ok := eventFromKey(&NotifyEvent{Key: ApprovalReaskKey("r1", "c1", false), Kind: string(NotificationApprovalPending), RunID: "r1", OpRun: "r1", OpKind: "gate", OpID: "c1"})
	if !ok || ev.approval.Reask != PendingReapproval {
		t.Fatalf("event with operation fields = %+v (%v)", ev.approval, ok)
	}
}

// A re-ask is routed by the approvers of the policy it asks under, to a key of its own per receiver,
// and the catch-up scan of those keys leaves it standing.
func TestReask_RoutedByApproversAndNotClearedByTheScan(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	err := weakNeedThenStrong(t, store)
	clk := resolvedClock()
	def, risk, fin := &fakeSink{}, &fakeSink{}, &fakeSink{}
	trail := NewMemNotifyTrail()
	e := testEngine(t, store, def, trail, clk, "p1",
		WithNotifyReceiver("risk", risk), WithNotifyReceiver("finance", fin),
		WithNotifyApproverRoute("alice", "risk"), WithNotifyApproverRoute("bob", "finance"), WithNotifyApproverRoute("carol", "finance"))
	ev, ok := e.eventFromErr("r1", err)
	if !ok || ev.approval.Reask != PendingSoDReapproval {
		t.Fatalf("no re-ask event from %v", err)
	}
	if err := e.roundRouted(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if risk.count() != 1 || fin.count() != 1 || def.count() != 0 {
		t.Fatalf("deliveries: risk=%d finance=%d default=%d", risk.count(), fin.count(), def.count())
	}
	for _, s := range []*fakeSink{risk, fin} {
		a := s.calls[0].Approval
		if a == nil || a.Reask != PendingSoDReapproval || a.Need != 2 {
			t.Fatalf("notification approval = %+v", a)
		}
	}
	clk.advance(1)
	scanAndFlush(t, e, "r1")
	if got := resolvedCalls(risk); len(got) != 0 || len(resolvedCalls(fin)) != 0 {
		t.Fatalf("an all-clear was sent for a re-ask the gate still waits on")
	}
	// Decided by its re-tally: now each receiver is told.
	approveAs(t, store, "r1", "c1", "carol", true)
	var charged int
	if _, err := sodRun(store, "r1", false, sodPolicy(2), fakeVerifiers(abc...), &charged, Identity{}); err == nil {
		t.Fatal("one approval passed a 2-of-3 gate")
	}
	approveAs(t, store, "r1", "c1", "bob", true)
	if _, err := sodRun(store, "r1", false, sodPolicy(2), fakeVerifiers(abc...), &charged, Identity{}); err != nil {
		t.Fatal(err)
	}
	clk.advance(1)
	scanAndFlush(t, e, "r1")
	if len(resolvedCalls(risk)) != 1 || len(resolvedCalls(fin)) != 1 {
		t.Fatalf("all-clears: risk=%d finance=%d; want one each", len(resolvedCalls(risk)), len(resolvedCalls(fin)))
	}
}

// An escalation of a re-ask is rebuilt from the trail and shows the policy the gate asks under, not
// the first ask's.
func TestReask_FillDetailUsesTheReaskNote(t *testing.T) {
	store := memJournal()
	if err := weakNeedThenStrong(t, store); err == nil {
		t.Fatal("no re-ask")
	}
	e := &notifyEngine{cfg: defaultNotifyConfig(), j: store}
	first := notifyEvent{runID: "r1", opRun: "r1", approval: &NotifyApproval{ToolUseID: "c1"}}
	e.fillDetail(context.Background(), &first)
	if first.approval.Need != 1 {
		t.Fatalf("first ask need = %d, want 1", first.approval.Need)
	}
	re := notifyEvent{runID: "r1", opRun: "r1", approval: &NotifyApproval{ToolUseID: "c1", Reask: PendingSoDReapproval}}
	e.fillDetail(context.Background(), &re)
	if re.approval.Need != 2 || re.approval.ToolName != "charge" {
		t.Fatalf("re-ask detail = %+v, want need 2", re.approval)
	}
}

// The journal's events include the re-ask under its own key, current while the gate waits, and no
// second copy of the first ask.
func TestReask_JournalNotifyEvents(t *testing.T) {
	store := memJournal()
	if err := weakThenStrong(t, store); err == nil {
		t.Fatal("no re-ask")
	}
	evs := JournalNotifyEvents("r1", mustHistory(t, store, "r1"))
	got := map[string]JournalNotifyEvent{}
	for _, ev := range evs {
		if _, dup := got[ev.Key]; dup {
			t.Fatalf("event %s twice", ev.Key)
		}
		got[ev.Key] = ev
	}
	first, re := got[ApprovalKey("r1", "c1", 0)], got[ApprovalReaskKey("r1", "c1", true)]
	if len(got) != 2 || first.Current || !re.Current || !slices.Equal(re.Approvers, abc) || re.At.IsZero() {
		t.Fatalf("events = %+v", got)
	}
	approveAs(t, store, "r1", "c1", "carol", true)
	var charged int
	if _, err := sodRun(store, "r1", false, sodPolicy(2), fakeVerifiers(abc...), &charged, Identity{}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range JournalNotifyEvents("r1", mustHistory(t, store, "r1")) {
		if ev.Current {
			t.Fatalf("%s still current after the gate passed", ev.Key)
		}
		if ev.Key == ApprovalReaskKey("r1", "c1", true) && ev.ResolvedAt.IsZero() {
			t.Fatalf("the re-ask has no resolved time, though carol's decision resolved it")
		}
	}
}

// A journal writer without approver keys can insert a note under a re-ask note's name before the
// gate does. No reader treats it as the gate's ask unless a recorded tally the gate would ask again
// over is there and the note names it.
func TestReask_ForgedNotesAreNotRead(t *testing.T) {
	forge := func(sod bool, mutate func(*ApprovalWait)) *Journal {
		store := memJournal()
		vf := fakeVerifiers(abc...)
		var charged int
		_, _ = sodRun(store, "r1", true, sodPolicy(2), vf, &charged, Identity{OnBehalfOf: "alice"})
		w := ApprovalWait{ToolUseID: "c1", ToolName: "charge", Need: 1, Approvers: []string{"mallory"}, RecordedAt: "2026-10-13T09:00:00.000Z",
			ExcludePrincipal: sod, Reask: PendingReapproval, Supersedes: ApprovalTallyStep("c1")}
		if sod {
			w.Reask = PendingSoDReapproval
		}
		if mutate != nil {
			mutate(&w)
		}
		b, _ := json.Marshal(w)
		writeRaw(t, store, "r1", ApprovalReaskStep("c1", sod), Record{Kind: StepValue, Result: b})
		return store
	}
	tally := func(t *testing.T, store *Journal, tl ApprovalTally) {
		t.Helper()
		b, _ := json.Marshal(tl)
		writeRaw(t, store, "r1", ApprovalTallyStep("c1"), Record{Kind: StepValue, Result: b})
	}
	current := ApprovalTally{Need: 2, Approvers: abc, Approved: 2, ApprovedBy: []string{"bob", "carol"}, ExcludePrincipal: true, Requesters: []string{"alice"}, DecisionFormat: DecisionFormat}
	cases := map[string]func(t *testing.T) *Journal{
		"sod note for another call": func(t *testing.T) *Journal {
			s := forge(true, func(w *ApprovalWait) { w.ToolUseID = "c2" })
			weak := current
			weak.ExcludePrincipal = false
			tally(t, s, weak)
			return s
		},
		"sod note with no reason and no tally": func(t *testing.T) *Journal { return forge(true, func(w *ApprovalWait) { w.Reask = "" }) },
		"sod note with no reason": func(t *testing.T) *Journal {
			s := forge(true, func(w *ApprovalWait) { w.Reask = "" })
			weak := current
			weak.ExcludePrincipal = false
			tally(t, s, weak)
			return s
		},
		"sod note with no tally":    func(t *testing.T) *Journal { return forge(true, nil) },
		"format note with no tally": func(t *testing.T) *Journal { return forge(false, nil) },
		"sod note over a tally that excluded the requester": func(t *testing.T) *Journal {
			s := forge(true, nil)
			tally(t, s, current)
			return s
		},
		"format note over a tally in the current format": func(t *testing.T) *Journal {
			s := forge(false, nil)
			tally(t, s, current)
			return s
		},
		"sod note naming no tally": func(t *testing.T) *Journal {
			s := forge(true, func(w *ApprovalWait) { w.Supersedes = "approval-tally:other" })
			weak := current
			weak.ExcludePrincipal = false
			tally(t, s, weak)
			return s
		},
		"sod note without the exclusion": func(t *testing.T) *Journal {
			s := forge(true, func(w *ApprovalWait) { w.ExcludePrincipal = false })
			weak := current
			weak.ExcludePrincipal = false
			tally(t, s, weak)
			return s
		},
		"note with the other reason": func(t *testing.T) *Journal {
			s := forge(true, func(w *ApprovalWait) { w.Reask = PendingReapproval })
			weak := current
			weak.ExcludePrincipal = false
			tally(t, s, weak)
			return s
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			store := mk(t)
			recs := mustHistory(t, store, "r1")
			byName := valuesByName(recs)
			r, ok := LatestPauseNote(byName, "c1")
			if !ok || r.Name != ApprovalPendingStep("c1") {
				t.Fatalf("LatestPauseNote = %q (%v), want the first note", r.Name, ok)
			}
			e := &notifyEngine{cfg: defaultNotifyConfig(), j: store}
			evs := e.pendingEvents("r1", recs)
			// At most the first ask (a recorded tally decides it): never a second event for the note.
			if len(evs) > 1 || len(evs) == 1 && (evs[0].key != ApprovalKey("r1", "c1", 0) || evs[0].approval.Reask != "") {
				t.Fatalf("pending events = %+v; want at most the first ask", evs)
			}
			// fillDetail of a re-ask event does not take the forged note's policy.
			re := notifyEvent{runID: "r1", opRun: "r1", approval: &NotifyApproval{ToolUseID: "c1", Reask: PendingSoDReapproval}}
			e.fillDetail(context.Background(), &re)
			if re.approval.ToolName != "" || re.approval.Need != 0 {
				t.Fatalf("fillDetail took a forged note: %+v", re.approval)
			}
			for _, ev := range JournalNotifyEvents("r1", recs) {
				if ev.Key != ApprovalKey("r1", "c1", 0) {
					t.Fatalf("journal event %s from a forged note", ev.Key)
				}
			}
		})
	}
}

// The note the gate wrote is read: the control of the forged cases above.
func TestReask_GenuineNotesAreRead(t *testing.T) {
	store := memJournal()
	if err := weakThenStrong(t, store); err == nil {
		t.Fatal("no re-ask")
	}
	byName := reaskByName(t, store)
	r, ok := LatestPauseNote(byName, "c1")
	if !ok || r.Name != ApprovalReaskStep("c1", true) {
		t.Fatalf("LatestPauseNote = %q (%v), want the sod note", r.Name, ok)
	}
}

// ValidReaskNote follows the chain of tallies the gate follows: a separation-of-duties re-ask over a
// tally that predates the format is valid only over the format re-tally, once that is recorded.
func TestReask_ValidNoteFollowsTheChainOfTallies(t *testing.T) {
	rec := func(v any) Record { b, _ := json.Marshal(v); return Record{Kind: StepValue, Result: b} }
	old := ApprovalTally{Need: 2, Approvers: abc, Approved: 2, ApprovedBy: []string{"bob", "carol"}}
	retally := ApprovalTally{Need: 2, Approvers: abc, Approved: 2, ApprovedBy: []string{"bob", "carol"}, DecisionFormat: DecisionFormat, Supersedes: ApprovalTallyStep("c1")}
	byName := map[string]Record{ApprovalTallyStep("c1"): rec(old), ApprovalRetallyStep("c1"): rec(retally)}
	sodNote := func(over string) ApprovalWait {
		return ApprovalWait{ToolUseID: "c1", ExcludePrincipal: true, Reask: PendingSoDReapproval, Supersedes: over}
	}
	if !ValidReaskNote(byName, "c1", ApprovalReaskStep("c1", true), sodNote(ApprovalRetallyStep("c1"))) {
		t.Fatal("a sod note over the format re-tally is not valid")
	}
	if ValidReaskNote(byName, "c1", ApprovalReaskStep("c1", true), sodNote(ApprovalTallyStep("c1"))) {
		t.Fatal("a sod note over the tally the format re-tally replaced is valid")
	}
	delete(byName, ApprovalRetallyStep("c1"))
	if ValidReaskNote(byName, "c1", ApprovalReaskStep("c1", true), sodNote(ApprovalTallyStep("c1"))) {
		t.Fatal("a sod note is valid before the gate has asked for the format")
	}
	v3 := ApprovalWait{ToolUseID: "c1", Reask: PendingReapproval, Supersedes: ApprovalTallyStep("c1")}
	if !ValidReaskNote(byName, "c1", ApprovalReaskStep("c1", false), v3) {
		t.Fatal("a format note over a tally that predates the format is not valid")
	}
}
