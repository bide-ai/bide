package agent

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"
)

// Every notifier option with a range refuses a value outside it, at New, with ErrConfig: a notifier
// that would deliver on a zero timeout or retry forever never runs half-configured.
func TestNotifierOptions_RefuseOutOfRangeValues(t *testing.T) {
	for name, opt := range map[string]NotifyOption{
		"deadline zero":            WithNotifyDeadline(0),
		"claim ttl negative":       WithNotifyClaimTTL(-time.Second),
		"retry base zero":          WithNotifyRetry(0, time.Second),
		"retry max below base":     WithNotifyRetry(time.Minute, time.Second),
		"failing after zero":       WithNotifyFailingAfter(0),
		"recovery failing after 0": WithNotifyRecoveryFailingAfter(0),
		"recovery failing neg":     WithNotifyRecoveryFailingAfter(-time.Minute),
		"halt age negative":        WithNotifyHaltAge(-time.Second),
		"aborted within zero":      WithNotifyAbortedWithin(0),
		"holder empty":             WithNotifyHolder(""),
		"overdue notifier nil":     WithOverdueNotifier(nil),
	} {
		_, err := New(&scriptModel{}, memJournal(), WithNotifier(&fakeSink{}, NewMemNotifyTrail(), opt))
		if !errors.Is(err, ErrConfig) {
			t.Errorf("%s: New = %v; want ErrConfig", name, err)
		}
	}
	for name, opt := range map[string]NotifyOption{
		"retry equal bounds": WithNotifyRetry(time.Second, time.Second),
		"failing after one":  WithNotifyFailingAfter(1),
		"recovery 1m":        WithNotifyRecoveryFailingAfter(time.Minute),
		"holder":             WithNotifyHolder("worker-1"),
		"overdue notifier":   WithOverdueNotifier(&fakeSink{}),
	} {
		if _, err := New(&scriptModel{}, memJournal(), WithNotifier(&fakeSink{}, NewMemNotifyTrail(), opt)); err != nil {
			t.Errorf("%s: New = %v", name, err)
		}
	}
}

// WithNotifier itself refuses a nil sink or trail, and an agent with no notifier has nothing to flush.
func TestNotifier_NilPartsAndFlushWithoutNotifier(t *testing.T) {
	if _, err := New(&scriptModel{}, memJournal(), WithNotifier(nil, NewMemNotifyTrail())); !errors.Is(err, ErrConfig) {
		t.Errorf("nil sink = %v; want ErrConfig", err)
	}
	if _, err := New(&scriptModel{}, memJournal(), WithNotifier(&fakeSink{}, nil)); !errors.Is(err, ErrConfig) {
		t.Errorf("nil trail = %v; want ErrConfig", err)
	}
	a := mustNew(&scriptModel{}, memJournal())
	if err := a.FlushNotifications(context.Background()); err != nil {
		t.Errorf("FlushNotifications without a notifier = %v", err)
	}
}

// NotifyPending refuses a nil journal, sink or trail, and a store that cannot list runs, rather
// than sweeping nothing.
func TestNotifyPending_Refusals(t *testing.T) {
	ctx := context.Background()
	sink, trail := &fakeSink{}, NewMemNotifyTrail()
	if err := NotifyPending(ctx, nil, sink, trail); !errors.Is(err, ErrConfig) {
		t.Errorf("nil journal = %v; want ErrConfig", err)
	}
	if err := NotifyPending(ctx, memJournal(), nil, trail); !errors.Is(err, ErrConfig) {
		t.Errorf("nil sink = %v; want ErrConfig", err)
	}
	if err := NotifyPending(ctx, memJournal(), sink, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("nil trail = %v; want ErrConfig", err)
	}
	if err := NotifyPending(ctx, mustJournal(plainStore{NewMemStore()}), sink, trail); !errors.Is(err, ErrConfig) {
		t.Errorf("a store with no Lister = %v; want ErrConfig", err)
	}
}

// covRunFault is a MemStore whose Load fails for one run, and whose Runs yields an error after its
// runs when runsErr is set.
type covRunFault struct {
	*MemStore
	badRun  string
	runsErr bool
}

func (s covRunFault) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if runID == s.badRun {
		return func(yield func(Entry, error) bool) { yield(Entry{}, errCovBlip) }
	}
	return s.MemStore.Load(ctx, runID, after)
}

func (s covRunFault) Runs(ctx context.Context, f RunFilter) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		for id, err := range s.MemStore.Runs(ctx, f) {
			if !yield(id, err) {
				return
			}
		}
		if s.runsErr {
			yield("", errCovBlip)
		}
	}
}

// A sweep reports a run it cannot read and goes on: the other runs' gates are still notified, and
// the failure comes back joined (a listing failure as ErrStorage), so one bad run never hides the
// rest.
func TestNotifyPending_ContinuesPastAnUnreadableRun(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	s := covRunFault{MemStore: NewMemStore(), badRun: "a-bad", runsErr: true}
	j := mustJournal(s)
	putGate(t, j, "z-good", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	// The bad run exists (its header is in the store), but its entries cannot be loaded.
	if _, _, err := s.MemStore.Insert(ctx, "a-bad", headerStep, headerEntry()); err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{}
	err := sweep(t, j, sink, NewMemNotifyTrail(), clk)
	if err == nil || !errors.Is(err, ErrStorage) {
		t.Fatalf("sweep = %v; want the joined failures including ErrStorage", err)
	}
	if sink.count() != 1 || sink.calls[0].RunID != "z-good" {
		t.Fatalf("the readable run was not notified: %+v", sink.calls)
	}
}

// A sweep whose context is cancelled stops and says so, rather than reporting success.
func TestNotifyPending_StopsWhenCancelled(t *testing.T) {
	j := memJournal()
	putGate(t, j, "r", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink := &fakeSink{}
	err := NotifyPending(ctx, j, sink, NewMemNotifyTrail(), fixedNotifyClock(sweepClock()), WithNotifyHolder("sweeper"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sweep on a cancelled context = %v; want context.Canceled", err)
	}
	if sink.count() != 0 {
		t.Fatalf("a cancelled sweep notified %d times", sink.count())
	}
}
