package agent

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
	var logs strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	failures := capReleaseFailures.Load()
	if _, err := runSum(t, j, "r0", sumArgs(60, ""), tool); err == nil {
		t.Fatal("want a failed run")
	}
	if live := liveDay(t, j); len(live) != 1 {
		t.Fatalf("live = %+v, want the slot left counted by the failed release", live)
	}
	// The failed release is not silent: counted, and logged with the run, call and rule but no amount.
	if capReleaseFailures.Load() != failures+1 {
		t.Fatalf("release failures = %d, want one more than %d", capReleaseFailures.Load(), failures)
	}
	if out := logs.String(); !strings.Contains(out, "could not be released") || !strings.Contains(out, "refund_total") || !strings.Contains(out, "c1") || strings.Contains(out, "amount") || regexp.MustCompile(`\b60\b`).MatchString(out) {
		t.Fatalf("log = %q, want the rule and call without the amount", out)
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

// capHeld keeps one entry per target and the flag it was first added with: marked is a function of the
// attempt, so a second add of the same target cannot flip it.
func TestCapHeld_DedupeKeepsTheFirstMarkedFlag(t *testing.T) {
	h := &capHeld{}
	m := capledger.Target{ToolUseID: "m"}
	u := capledger.Target{ToolUseID: "u"}
	h.add(m, true)
	h.add(m, false) // ignored: the target is held already, marked
	h.add(u, false)
	h.add(u, true) // ignored: held already, unmarked
	got := h.take()
	if len(got) != 1 || got[0] != m {
		t.Fatalf("take = %v, want only the marked m", got)
	}
}

// The marker key of a slot's attempt id is everything before the last '#', so a key that holds '#' is read
// whole, and an unmarked attempt (no marker key) gives none.
func TestCapMarkerKey(t *testing.T) {
	for _, tc := range []struct {
		att, key string
		ok       bool
	}{
		{"attempt:tool:abc#refund_total", "attempt:tool:abc", true},
		{"attempt:tool:a#b#c#refund_total", "attempt:tool:a#b#c", true},
		{"#refund_total", "", false},
		{"refund_total", "", false},
		{"", "", false},
	} {
		if key, ok := capMarkerKey(tc.att); key != tc.key || ok != tc.ok {
			t.Errorf("capMarkerKey(%q) = %q, %v; want %q, %v", tc.att, key, ok, tc.key, tc.ok)
		}
	}
}

// leakOne leaves one marked slot counted in the ledger of the window now falls in: the second rule's slot and
// every release fail, so the call fails before the tool began, its release fails, and the void follows.
func leakOne(t *testing.T, os *orderStore, j *Journal, runID string, now time.Time, opts ...Option) {
	leakRules(t, os, j, runID, now, []string{capRuleDay, capRuleHour}, opts...)
}

// leakRules is leakOne for the given rules (the last one is the one whose slot cannot be written).
func leakRules(t *testing.T, os *orderStore, j *Journal, runID string, now time.Time, lines []string, opts ...Option) {
	t.Helper()
	os.failOn = func(name string, data []byte) bool {
		return strings.HasPrefix(name, "slot:") && (strings.Contains(string(data), `"kind":"release"`) || strings.Contains(string(data), "refund_hour"))
	}
	defer func() { os.failOn = nil }()
	tool := sumTool(Safety{}, lines...)
	a := mustNew(capModel{sumArgs(60, "")}, j, append([]Option{WithTools(tool), WithClock(func() time.Time { return now })}, opts...)...)
	if _, err := a.Run(context.Background(), runID, UserText("go")); err == nil {
		t.Fatal("want a failed run")
	}
}

// Reconcile follows the open windows: a day window that ended more than a day ago is not read, and one that
// is open is. (A leaked slot in a closed window no longer limits any reservation a correct clock makes.)
func TestCapsRelease_ReconcileSkipsClosedWindows(t *testing.T) {
	os := &orderStore{Store: NewMemStore()}
	j := newJournal(os)
	closed := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	open := time.Now().UTC()
	leakOne(t, os, j, "old", closed, WithCapNamespace("old")) // its own namespace: the clock bound refuses a window years earlier
	leakOne(t, os, j, "new", open)
	released, err := reconcileCaps(context.Background(), j)
	if err != nil || len(released) != 1 {
		t.Fatalf("reconcileCaps = %v, %v, want only the open window's slot", released, err)
	}
	l := newCapLedger(j, time.Now, 0)
	rule := capledger.Rule{Namespace: defaultCapNamespace, Name: "refund_total", Currency: "USD", Kind: capledger.Day}
	oldRule := rule
	oldRule.Namespace = "old"
	oldLive, _ := l.Live(context.Background(), l.LedgerRun(oldRule, "2020-01-01", capledger.KeyDigest("")))
	newLive, _ := l.Live(context.Background(), l.LedgerRun(rule, open.Format("2006-01-02"), capledger.KeyDigest("")))
	if len(oldLive) != 1 || len(newLive) != 0 {
		t.Fatalf("closed window live = %d, open window live = %d, want 1 and 0", len(oldLive), len(newLive))
	}
}

func TestCapWindowOpen(t *testing.T) {
	ctx := context.Background()
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	store := memJournal()
	if _, err := store.put(ctx, "ended-root", runCompleteStep, Record{Kind: StepValue, Result: json.RawMessage(`1`)}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("r", 200) // its encoded form is a digest
	for _, tc := range []struct {
		name   string
		kind   capledger.Kind
		window string
		now    string
		want   bool
	}{
		{"day, 47h59m after its start+24h... open", capledger.Day, "2026-10-20", "2026-10-21T23:59:59Z", true},
		{"day, closed 48h after its start", capledger.Day, "2026-10-20", "2026-10-22T00:00:00Z", false},
		{"hour, just inside two hours", capledger.Hour, "2026-10-22T10", "2026-10-22T11:59:59Z", true},
		{"hour, just outside two hours", capledger.Hour, "2026-10-22T10", "2026-10-22T12:00:00Z", false},
		{"hour, one hour in", capledger.Hour, "2026-10-22T10", "2026-10-22T11:30:00Z", true},
		{"run window, root still running", capledger.Run, "run-1", "2026-10-22T12:00:00Z", true},
		{"run window, root ended", capledger.Run, "ended-root", "2026-10-22T12:00:00Z", false},
		{"run window named like a date, still running, long after", capledger.Run, "2026-07-15", "2026-10-22T12:00:00Z", true},
		{"run window with a root whose encoded id is a digest", capledger.Run, long, "2026-10-22T12:00:00Z", true},
		{"unknown kind", capledger.Kind("week"), "x", "2026-10-22T12:00:00Z", true},
		{"unparseable day", capledger.Day, "not-a-day", "2026-10-22T12:00:00Z", true},
	} {
		if got, err := capWindowOpen(ctx, store, tc.kind, tc.window, at(tc.now)); err != nil || got != tc.want {
			t.Errorf("%s: open = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

// getFailStore fails the reads of the names it selects.
type getFailStore struct {
	Store
	fail func(run, name string) bool
	gets map[string]int
	mu   sync.Mutex
}

func (g *getFailStore) Get(ctx context.Context, run, name string) (Entry, bool, error) {
	g.mu.Lock()
	if g.gets == nil {
		g.gets = map[string]int{}
	}
	g.gets[run+"|"+name]++
	g.mu.Unlock()
	if g.fail != nil && g.fail(run, name) {
		return Entry{}, false, errors.New("store down")
	}
	return g.Store.Get(ctx, run, name)
}

func (g *getFailStore) Unwrap() Store { return g.Store }

func (g *getFailStore) count(run string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for k, v := range g.gets {
		if strings.HasPrefix(k, run+"|") {
			n += v
		}
	}
	return n
}

// A store error while reading whether a root run ended surfaces from the reconcile pass; it is not read as
// "closed" and skipped.
func TestCapWindowOpen_StoreErrorSurfaces(t *testing.T) {
	gs := &getFailStore{Store: NewMemStore()}
	gs.fail = func(run, name string) bool { return run == "boom" && name == runCompleteStep }
	open, err := capWindowOpen(context.Background(), newJournal(gs), capledger.Run, "boom", time.Now())
	if err == nil || open {
		t.Fatalf("open = %v, err = %v, want the error", open, err)
	}
}

// runLeakRules are the rules of the run-window leak tests: a per-run total and an hour rule whose slot cannot be written.
var runLeakRules = []string{`refund_run: refund.amount[minor, currency=currency] sum 10 USD per run -> refuse`, capRuleHour}

func runLedger(l *capledger.Ledger, root string) string {
	return l.LedgerRun(capledger.Rule{Namespace: defaultCapNamespace, Name: "refund_run", Currency: "USD", Kind: capledger.Run}, root, capledger.KeyDigest(""))
}

// Reconcile of a run window: a root run still running (even one named like a date, and one whose id is long
// enough to be digested in the ledger's name) has its voided slot released; one whose root completed is left.
func TestCapsRelease_ReconcilesRunWindowsOfRunningRoots(t *testing.T) {
	long := strings.Repeat("x", 120)
	for name, root := range map[string]string{"plain": "run-plain", "date-named": "2026-07-15", "long": long} {
		t.Run(name, func(t *testing.T) {
			os := &orderStore{Store: NewMemStore()}
			j := newJournal(os)
			leakRules(t, os, j, root, time.Now().UTC(), runLeakRules)
			l := newCapLedger(j, time.Now, 0)
			ledger := runLedger(l, root)
			if live, err := l.Live(context.Background(), ledger); err != nil || len(live) != 1 {
				t.Fatalf("live = %+v, %v; want the leak", live, err)
			}
			released, err := reconcileCaps(context.Background(), j)
			if err != nil || len(released) != 1 {
				t.Fatalf("reconcileCaps = %v, %v, want the run window's slot", released, err)
			}
			if live, _ := l.Live(context.Background(), ledger); len(live) != 0 {
				t.Fatalf("live = %+v after reconcile", live)
			}
		})
	}
}

func TestCapsRelease_ReconcileSkipsARunWindowWhoseRootEnded(t *testing.T) {
	os := &orderStore{Store: NewMemStore()}
	j := newJournal(os)
	leakRules(t, os, j, "done-root", time.Now().UTC(), runLeakRules)
	if _, err := j.put(context.Background(), "done-root", runCompleteStep, Record{Kind: StepValue, Result: json.RawMessage(`1`)}); err != nil {
		t.Fatal(err)
	}
	released, err := reconcileCaps(context.Background(), j)
	l := newCapLedger(j, time.Now, 0)
	if live, _ := l.Live(context.Background(), runLedger(l, "done-root")); err != nil || len(released) != 0 || len(live) != 1 {
		t.Fatalf("released %v, err %v, live %+v: want the ended run's window left alone", released, err, live)
	}
}

// A store error reading the root's markers surfaces from reconcileCaps and releases nothing.
func TestCapsRelease_ReconcileSurfacesAStoreErrorOnARunWindow(t *testing.T) {
	os := &orderStore{Store: NewMemStore()}
	j := newJournal(os)
	leakRules(t, os, j, "root-e", time.Now().UTC(), runLeakRules)
	gs := &getFailStore{Store: os, fail: func(run, name string) bool { return run == "root-e" && name == runCompleteStep }}
	jf := newJournal(gs)
	released, err := reconcileCaps(context.Background(), jf)
	l := newCapLedger(j, time.Now, 0)
	if live, _ := l.Live(context.Background(), runLedger(l, "root-e")); err == nil || len(released) != 0 || len(live) != 1 {
		t.Fatalf("released %v, err %v, live %+v: want the error surfaced and the slot untouched", released, err, live)
	}
}

// An ended root is remembered: later passes do not read its markers again, and the memory is bounded.
func TestCapsRelease_EndedRootsAreRememberedAndBounded(t *testing.T) {
	gs := &getFailStore{Store: NewMemStore()}
	j := newJournal(gs)
	if _, err := j.put(context.Background(), "ended-once", runCompleteStep, Record{Kind: StepValue, Result: json.RawMessage(`1`)}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got, err := capWindowOpen(context.Background(), j, capledger.Run, "ended-once", time.Now()); err != nil || got {
			t.Fatalf("open = %v, %v", got, err)
		}
	}
	if n := gs.count("ended-once"); n > 1 {
		t.Fatalf("%d reads of the ended root's markers over three passes, want at most the first pass's 1", n)
	}
	c := &endedCache{max: 2, set: map[string]*list.Element{}, order: list.New()}
	c.add("a")
	c.add("b")
	c.has("a") // a is now the most recent
	c.add("c") // drops b
	if !c.has("a") || c.has("b") || !c.has("c") || c.order.Len() != 2 {
		t.Fatalf("cache holds a=%v b=%v c=%v (len %d), want the least recently used (b) dropped", c.has("a"), c.has("b"), c.has("c"), c.order.Len())
	}
}

// RecoverLoop reconciles too (its passes list the cap runs as Recover does).
func TestCapsRelease_RecoverLoopReconciles(t *testing.T) {
	os := &orderStore{Store: NewMemStore()}
	j := newJournal(os)
	leakOne(t, os, j, "r0", time.Now().UTC())
	l := newCapLedger(j, time.Now, 0)
	live := func() int {
		got, _ := l.Live(context.Background(), l.LedgerRun(capledger.Rule{Namespace: defaultCapNamespace, Name: "refund_total", Currency: "USD", Kind: capledger.Day}, time.Now().UTC().Format("2006-01-02"), capledger.KeyDigest("")))
		return len(got)
	}
	if live() != 1 {
		t.Fatalf("live = %d before the loop, want the leak", live())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RecoverLoop(ctx, j, func(context.Context, string, RunStart) error { return nil }, WithRecoverInterval(10*time.Millisecond))
	}()
	deadline := time.Now().Add(10 * time.Second)
	for live() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if live() != 0 {
		t.Fatalf("live = %d after RecoverLoop passes, want the leak released", live())
	}
}

// take returns the marked targets only, in the order they were added, and never an unmarked one.
func TestCapHeld_TakeReturnsOnlyMarkedInOrder(t *testing.T) {
	h := &capHeld{}
	ts := []capledger.Target{{ToolUseID: "a"}, {ToolUseID: "b"}, {ToolUseID: "c"}, {ToolUseID: "d"}}
	for i, tg := range ts {
		h.add(tg, i%2 == 0)
	}
	got := h.take()
	if len(got) != 2 || got[0] != ts[0] || got[1] != ts[2] {
		t.Fatalf("take = %v, want a then c", got)
	}
}
