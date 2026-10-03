package agent

import (
	"context"
	"errors"
	"iter"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

// answerModel answers directly on its first turn (no tool calls), so a run completes.
type answerModel struct{}

func (answerModel) Stream(_ context.Context, _ Request) (*Stream, error) {
	ch := make(chan Emit, 2)
	ch <- Emit{Event: TextDelta{Text: "done"}}
	ch <- Emit{Event: Finish{Reason: "stop"}}
	close(ch)
	return NewStream(ch), nil
}

// approvalModel calls a tool that requires human approval, so the run pauses durably.
type approvalModel struct{}

func (approvalModel) Stream(_ context.Context, req Request) (*Stream, error) {
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "pay", Name: "charge", ArgsFragment: []byte(`{}`)}}
	ch <- Emit{Event: Finish{Reason: "tool_use"}}
	close(ch)
	return NewStream(ch), nil
}

// TestRecover_SkipsCompletedRedrivesIncomplete drives two runs to completion and leaves
// one paused for approval, then asserts Recover re-drives only the incomplete run.
func TestRecover_SkipsCompletedRedrivesIncomplete(t *testing.T) {
	ctx := context.Background()
	store := memJournal()

	// Two runs that finish (their journals get a completion marker).
	done := mustNew(answerModel{}, store)
	if _, err := done.Run(ctx, "done1", "hi"); err != nil {
		t.Fatalf("done1: %v", err)
	}
	if _, err := done.Run(ctx, "done2", "hi"); err != nil {
		t.Fatalf("done2: %v", err)
	}
	// One run that pauses for human approval (never reaches the terminal marker).
	charge := Func("charge", "charge a card", Safety{},
		func(context.Context, struct{}) (string, error) { return "charged", nil }, WithApproval(SingleApproval()))
	paused := mustNew(approvalModel{}, store, WithTools(charge))
	_, err := paused.Run(ctx, "paused1", "hi")
	var pa *PendingApproval
	if !errors.As(err, &pa) {
		t.Fatalf("paused1 should await approval, got %v", err)
	}

	// IsComplete distinguishes the two classes directly.
	for _, id := range []string{"done1", "done2"} {
		if ok, err := IsComplete(ctx, store, id); err != nil || !ok {
			t.Fatalf("IsComplete(%s) = %v, %v; want true, nil", id, ok, err)
		}
	}
	if ok, err := IsComplete(ctx, store, "paused1"); err != nil || ok {
		t.Fatalf("IsComplete(paused1) = %v, %v; want false, nil", ok, err)
	}

	// Recover: the resume callback records which runIDs it was asked to re-drive.
	var asked []string
	n, err := Recover(ctx, store, func(ctx context.Context, runID string, _ RunStart) error {
		asked = append(asked, runID)
		_, err := paused.Run(ctx, runID, "hi") // still paused -> a pause error, treated as success
		return err
	})
	if err != nil {
		t.Fatalf("Recover returned an error for a still-paused run: %v", err)
	}
	if n != 1 {
		t.Fatalf("Recover re-drove %d runs, want 1 (only the incomplete one)", n)
	}
	sort.Strings(asked)
	if len(asked) != 1 || asked[0] != "paused1" {
		t.Fatalf("Recover asked to resume %v, want [paused1] only", asked)
	}
}

// TestRecover_StillPausedCountsAsRecovered confirms a re-driven run that is still waiting
// is a SUCCESS (no error joined) and is counted as recovered.
func TestRecover_StillPausedCountsAsRecovered(t *testing.T) {
	ctx := context.Background()
	store := memJournal()

	charge := Func("charge", "charge a card", Safety{},
		func(context.Context, struct{}) (string, error) { return "charged", nil }, WithApproval(SingleApproval()))
	a := mustNew(approvalModel{}, store, WithTools(charge))
	if _, err := a.Run(ctx, "p", "hi"); !IsPause(err) {
		t.Fatalf("run should pause, got %v", err)
	}

	n, err := Recover(ctx, store, func(ctx context.Context, runID string, _ RunStart) error {
		_, err := a.Run(ctx, runID, "hi") // re-drives, still pauses for approval
		return err
	})
	if err != nil {
		t.Fatalf("a still-paused resume must not surface as an error, got %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered = %d, want 1", n)
	}
}

// TestRecover_NeedsLister confirms Recover reports a config error for a store that cannot
// enumerate its runs.
func TestRecover_NeedsLister(t *testing.T) {
	_, err := Recover(context.Background(), mustJournal(noListStore{}), func(context.Context, string, RunStart) error { return nil })
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig for a non-Lister store", err)
	}
}

// noListStore is a Store that does NOT implement Lister (or Leaser). It stores nothing: every
// Insert reports stored, and every read finds nothing.
type noListStore struct{}

func (noListStore) Insert(_ context.Context, _, name string, data []byte) (Entry, bool, error) {
	return Entry{Seq: 1, Name: name, Data: data}, true, nil
}
func (noListStore) Get(context.Context, string, string) (Entry, bool, error) {
	return Entry{}, false, nil
}
func (noListStore) Load(context.Context, string, int64) iter.Seq2[Entry, error] {
	return func(func(Entry, error) bool) {}
}

// TestRecover_WakerRebuild is Part 2: after a "crash" (fresh MemWaker, timers lost),
// Recover with a Waker-bound resume re-registers the sleeping run's wake, so advancing the
// clock and firing the waker resumes it to completion. The timer set is rebuilt purely by
// re-driving the run: Sleep sees the Waker on ctx and re-registers its journaled wake.
func TestRecover_WakerRebuild(t *testing.T) {
	var clk int64 = 1000
	now := func() time.Time { return time.Unix(atomic.LoadInt64(&clk), 0) }
	store := memJournal()
	a := mustNew(sleepModel{}, store, WithTools(waitTool()))

	// The run sleeps for an hour and pauses durably. Its wake time is journaled.
	if _, err := a.Run(ContextWithClock(context.Background(), now), "sleeper", "go"); !errorsIsSleeping(err) {
		t.Fatalf("run should sleep, got %v", err)
	}

	// Crash: a brand-new MemWaker has an EMPTY timer set (the in-memory timers are lost).
	var w *MemWaker
	w = NewMemWaker(func(ctx context.Context, runID string) error {
		_, err := a.Run(ContextWithWaker(ContextWithClock(ctx, now), w), runID, "go")
		return err
	})

	// Recover rebuilds the timer set: it re-drives the incomplete run with a Waker-bound
	// resume, and Sleep re-registers the journaled wake on the fresh waker.
	n, err := Recover(context.Background(), store, func(ctx context.Context, runID string, _ RunStart) error {
		_, err := a.Run(ContextWithWaker(ContextWithClock(ctx, now), w), runID, "go")
		return err
	})
	if err != nil {
		t.Fatalf("Recover: %v (a still-sleeping run is not a failure)", err)
	}
	if n != 1 {
		t.Fatalf("recovered = %d, want 1", n)
	}

	// Before the wake time, nothing fires (proving the re-registered timer honors it).
	if fired, err := w.Fire(context.Background(), now()); fired != 0 || err != nil {
		t.Fatalf("nothing should fire before the wake time, fired %d err %v", fired, err)
	}

	// Advance past the wake time and fire: the rebuilt timer resumes the run to completion.
	atomic.StoreInt64(&clk, 1000+3600)
	fired, err := w.Fire(context.Background(), now())
	if err != nil || fired != 1 {
		t.Fatalf("the rebuilt waker should resume exactly one run, fired %d err %v", fired, err)
	}
	if ok, err := IsComplete(context.Background(), store, "sleeper"); err != nil || !ok {
		t.Fatalf("run should be complete after the rebuilt timer fired, IsComplete = %v, %v", ok, err)
	}
}

// TestRetriableOnResume: only ReadOnly and Idempotent make a call retry-safe.
func TestRetriableOnResume(t *testing.T) {
	cases := []struct {
		name string
		s    Safety
		want bool
	}{
		{"read-only", Safety{ReadOnly: true}, true},
		{"idempotent", Safety{Idempotent: true}, true},
		{"bare", Safety{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.retriableOnResume(); got != tc.want {
				t.Fatalf("retriableOnResume() = %v, want %v", got, tc.want)
			}
		})
	}
}
