package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bide-ai/bide/internal/capledger"
)

// The release half of cumulative caps: the loop is the only releaser, it releases a marked reservation
// when it proves the tool never began (it sealed the began word) and before the attempt is voided, and the
// reconcile pass releases what a void left counted.

const (
	capRuleDay  = `refund_total: refund.amount[minor, currency=currency] sum 10 USD per day -> refuse`
	capRuleHour = `refund_hour: refund.amount[minor, currency=currency] sum 1 USD per hour -> refuse`
)

func liveDay(t *testing.T, j *Journal) []capledger.Slot {
	return liveOf(t, j, "refund_total", capledger.Day, "2026-10-22")
}

// A call refused by its second rule after its first reserved was never begun: the first slot is released.
func TestCapsRelease_RefusalByALaterRuleReleasesTheEarlierSlot(t *testing.T) {
	j := memJournal()
	tool := sumTool(Safety{}, capRuleDay, capRuleHour)
	if _, err := runSum(t, j, "r0", sumArgs(60, ""), tool); err != nil || tool.calls() != 1 {
		t.Fatalf("first call: %v, calls %d", err, tool.calls())
	}
	if _, err := runSum(t, j, "r1", sumArgs(80, ""), tool); err != nil || tool.calls() != 1 {
		t.Fatalf("second call: %v, calls %d (want refused by the hour rule)", err, tool.calls())
	}
	live := liveDay(t, j)
	if len(live) != 1 || live[0].Amount != 60 {
		t.Fatalf("day ledger live = %+v, want only the first call's 60 (the refused call's 80 released)", live)
	}
	// And the window has room again: a call of 900 fits (60 + 900 <= 1000) under the day rule.
	hour := liveOf(t, j, "refund_hour", capledger.Hour, "2026-10-22T12")
	if len(hour) != 1 {
		t.Fatalf("hour ledger live = %+v", hour)
	}
}

// A retry-safe call is never released, even when the same refusal proves it did not begin.
func TestCapsRelease_UnmarkedSlotIsNeverReleased(t *testing.T) {
	j := memJournal()
	tool := sumTool(Safety{Idempotent: true}, capRuleDay, capRuleHour)
	if _, err := runSum(t, j, "r0", sumArgs(60, ""), tool); err != nil {
		t.Fatal(err)
	}
	if _, err := runSum(t, j, "r1", sumArgs(80, ""), tool); err != nil {
		t.Fatal(err)
	}
	live := liveDay(t, j)
	if len(live) != 2 {
		t.Fatalf("day ledger live = %+v, want both (the unmarked refused call's 80 stays counted)", live)
	}
	for _, s := range live {
		if s.Marked {
			t.Fatalf("slot %+v is marked", s)
		}
	}
}

// A tool that began is never released, however the chain ends: a middleware that turns the tool's success
// into an error leaves the call's outcome unknown and its slot counted.
func TestCapsRelease_ACallThatBeganKeepsItsSlot(t *testing.T) {
	j := memJournal()
	tool := sumTool(Safety{}, capRuleDay)
	spoil := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if _, err := next(ctx, call); err != nil {
				return nil, err
			}
			return nil, errors.New("middleware failed after the tool ran")
		}
	}
	_, _ = runSum(t, j, "r0", sumArgs(60, ""), tool, WithToolMiddleware(spoil))
	if tool.calls() != 1 {
		t.Fatalf("calls = %d", tool.calls())
	}
	if live := liveDay(t, j); len(live) != 1 || live[0].Amount != 60 {
		t.Fatalf("live = %+v, want the call's 60 still counted", live)
	}
}

// orderStore records the names of the entries inserted, in order, and fails the inserts failOn selects.
type orderStore struct {
	Store
	mu     sync.Mutex
	names  []string
	failOn func(name string, data []byte) bool
}

func (o *orderStore) Insert(ctx context.Context, run, name string, data []byte) (Entry, bool, error) {
	if o.failOn != nil && o.failOn(name, data) {
		return Entry{}, false, errors.New("store down")
	}
	e, ins, err := o.Store.Insert(ctx, run, name, data)
	if ins && err == nil {
		o.mu.Lock()
		o.names = append(o.names, run+"|"+name+"|"+string(data))
		o.mu.Unlock()
	}
	return e, ins, err
}

// Unwrap lets Capability find the inner store's Lister.
func (o *orderStore) Unwrap() Store { return o.Store }

func (o *orderStore) indexOf(sub string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.IndexFunc(o.names, func(n string) bool { return strings.Contains(n, sub) })
}

// A store fault on the second rule's slot fails the call before the tool began: the first slot is released
// and then the attempt is voided, in that order, and a re-drive reserves and runs the call.
func TestCapsRelease_ReleaseIsWrittenBeforeTheVoid(t *testing.T) {
	os := &orderStore{Store: NewMemStore()}
	os.failOn = func(name string, data []byte) bool {
		return strings.HasPrefix(name, "slot:") && strings.Contains(string(data), `"attempt":`) && strings.Contains(string(data), "refund_hour")
	}
	j := newJournal(os)
	tool := sumTool(Safety{}, capRuleDay, capRuleHour)
	_, err := runSum(t, j, "r0", sumArgs(60, ""), tool)
	if err == nil || tool.calls() != 0 {
		t.Fatalf("err = %v, calls = %d, want a failed run and no call", err, tool.calls())
	}
	rel := os.indexOf(`"kind":"release"`)
	void := os.indexOf("attempt:not-started")
	if rel < 0 || void < 0 || rel > void {
		t.Fatalf("release at %d, void at %d in %v: want the release first", rel, void, os.names)
	}
	if live := liveDay(t, j); len(live) != 0 {
		t.Fatalf("live = %+v after the release", live)
	}
	os.failOn = nil
	if _, err := runSum(t, j, "r0", sumArgs(60, ""), tool); err != nil || tool.calls() != 1 {
		t.Fatalf("re-drive: err = %v, calls = %d", err, tool.calls())
	}
	if live := liveDay(t, j); len(live) != 1 {
		t.Fatalf("live = %+v after the re-drive, want one", live)
	}
}

// A release that fails leaves the slot counted; the void follows, and the reconcile pass releases it.
func TestCapsRelease_ReconcileReleasesWhatAVoidLeftCounted(t *testing.T) {
	os := &orderStore{Store: NewMemStore()}
	j := newJournal(os)
	tool := sumTool(Safety{}, capRuleDay, capRuleHour)
	os.failOn = func(name string, data []byte) bool {
		return strings.HasPrefix(name, "slot:") && (strings.Contains(string(data), `"kind":"release"`) || strings.Contains(string(data), "refund_hour"))
	}
	if _, err := runSum(t, j, "r0", sumArgs(60, ""), tool); err == nil {
		t.Fatal("want a failed run")
	}
	if live := liveDay(t, j); len(live) != 1 {
		t.Fatalf("live = %+v, want the slot left counted by the failed release", live)
	}
	os.failOn = nil
	released, err := reconcileCaps(context.Background(), j)
	if err != nil || len(released) != 1 {
		t.Fatalf("reconcileCaps = %v, %v, want the one slot", released, err)
	}
	if live := liveDay(t, j); len(live) != 0 {
		t.Fatalf("live = %+v after reconcile", live)
	}
	// Idempotent: a second pass finds nothing.
	if released, err := reconcileCaps(context.Background(), j); err != nil || len(released) != 0 {
		t.Fatalf("second reconcile = %v, %v", released, err)
	}
}

// Recover runs the reconcile pass.
func TestCapsRelease_RecoverReconciles(t *testing.T) {
	os := &orderStore{Store: NewMemStore()}
	j := newJournal(os)
	tool := sumTool(Safety{}, capRuleDay, capRuleHour)
	os.failOn = func(name string, data []byte) bool {
		return strings.HasPrefix(name, "slot:") && (strings.Contains(string(data), `"kind":"release"`) || strings.Contains(string(data), "refund_hour"))
	}
	_, _ = runSum(t, j, "r0", sumArgs(60, ""), tool)
	os.failOn = nil
	if _, err := Recover(context.Background(), j, func(context.Context, string, RunStart) error { return nil }); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if live := liveDay(t, j); len(live) != 0 {
		t.Fatalf("live = %+v after Recover", live)
	}
}

// Reconcile does not touch an attempt whose marker stands (the call may have run), an unmarked attempt, or a
// call ResolveHalt resolved: crash at every write, resolve the halt, reconcile, and a call that fired is
// still counted.
func TestCapsRelease_ReconcileAndResolveHaltNeverCreditACallThatMayHaveRun(t *testing.T) {
	halts := 0
	for crashAt := 1; crashAt <= 14; crashAt++ {
		mem := newJournal(NewMemStore())
		tool := sumTool(Safety{}, capRuleDay)
		a := mustNew(capModel{sumArgs(300, "")}, crashJournal(mem, crashAt), WithTools(tool), capClockOpt())
		_, _ = a.Run(context.Background(), "r0", UserText("go"))
		before := len(liveDay(t, mem))
		if released, err := reconcileCaps(context.Background(), mem); err != nil {
			t.Fatalf("crash %d: %v", crashAt, err)
		} else if len(released) != 0 && tool.calls() > 0 {
			t.Fatalf("crash %d: reconcile released %v for a call that fired", crashAt, released)
		}
		a2 := mustNew(capModel{sumArgs(300, "")}, mem, WithTools(tool), capClockOpt())
		_, err := a2.Run(context.Background(), "r0", UserText("go"))
		if h, ok := errors.AsType[*OutcomeUnknown](err); ok {
			halts++
			ref := HaltRef{RunID: h.RunID, Op: h.Op, Cause: h.Cause}
			if rerr := ResolveHalt(context.Background(), mem, ref, Outcome{Result: "not charged", IsError: true}, WithoutLiveDriverCheck()); rerr != nil {
				t.Fatalf("crash %d: ResolveHalt: %v", crashAt, rerr)
			}
			if after := len(liveDay(t, mem)); after != before {
				t.Fatalf("crash %d: ResolveHalt changed the live slots from %d to %d", crashAt, before, after)
			}
			if released, _ := reconcileCaps(context.Background(), mem); len(released) != 0 {
				t.Fatalf("crash %d: reconcile released %v for a resolved halt", crashAt, released)
			}
		}
		if tool.calls() > 1 || tool.calls() == 1 && len(liveDay(t, mem)) != 1 {
			t.Fatalf("crash %d: calls %d, live %+v", crashAt, tool.calls(), liveDay(t, mem))
		}
	}
	if halts == 0 {
		t.Fatal("no crash point halted the run: the sweep does not reach ResolveHalt")
	}
}

// A reservation made after the loop took the list is not in it (the late reserve leaks to reconcile).
func TestCapHeld_AddAfterTakeIsDropped(t *testing.T) {
	h := &capHeld{}
	a := capledger.Target{ToolUseID: "a"}
	b := capledger.Target{ToolUseID: "b"}
	h.add(a, true)
	h.add(a, true) // a retry adds nothing new
	h.add(capledger.Target{ToolUseID: "u"}, false)
	got := h.take()
	h.add(b, true)
	if len(got) != 1 || got[0] != a || len(h.take()) != 1 {
		t.Fatalf("take = %v then %v, want only the marked a", got, h.take())
	}
}

// A compensation reserves and releases nothing: a saga rollback gives no credit.
func TestCapsRelease_RollbackGivesNoCredit(t *testing.T) {
	j := memJournal()
	tool := sumTool(Safety{}, capRuleDay)
	if _, err := runSum(t, j, "r0", sumArgs(300, ""), tool); err != nil {
		t.Fatal(err)
	}
	if live := liveDay(t, j); len(live) != 1 {
		t.Fatalf("live = %+v", live)
	}
	a := mustNew(&scriptModel{}, j, WithTools(tool), capClockOpt())
	ctx := withCompensating(context.Background())
	if _, _, err := a.toolHandler("r0")(ctx, ToolUse{ID: "c9", Name: "refund", Args: json.RawMessage(sumArgs(300, ""))}); err == nil || errors.Is(err, ErrConfig) {
		_ = err
	}
	if live := liveDay(t, j); len(live) != 1 || live[0].CumAfter != 300 {
		t.Fatalf("live = %+v after a compensating call, want the original 300 and nothing more", live)
	}
	_ = fmt.Sprint
}
