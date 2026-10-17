package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ledger tracks side effects: which resources are currently "active" (side effect
// applied and not undone), how many times each was undone, and the undo order. The core
// saga invariant is: after an abort, NOTHING is active, and each undo ran at most once.
type ledger struct {
	mu        sync.Mutex
	active    map[string]bool
	undoCount map[string]int
	undoOrder []string
}

func newLedger() *ledger {
	return &ledger{active: map[string]bool{}, undoCount: map[string]int{}}
}

// write returns a compensated tool that marks `res` active on do and clears it on undo.
func (l *ledger) write(res string) Tool {
	return MustCompensatedFunc(res, "", func(context.Context, struct{}) (struct{}, error) {
		l.mu.Lock()
		l.active[res] = true
		l.mu.Unlock()
		return struct{}{}, nil
	},
		func(context.Context, struct{}, struct{}) error {
			l.mu.Lock()
			l.active[res] = false
			l.undoCount[res]++
			l.undoOrder = append(l.undoOrder, res)
			l.mu.Unlock()
			return nil
		})
}

func (l *ledger) activeSet() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for k, v := range l.active {
		if v {
			out = append(out, k)
		}
	}
	return out
}

// assertClean is the invariant: nothing left active, and no compensator ran more than once.
func (l *ledger) assertClean(t *testing.T) {
	t.Helper()
	if a := l.activeSet(); len(a) != 0 {
		t.Fatalf("side effects still active after abort: %v", a)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for res, n := range l.undoCount {
		if n > 1 {
			t.Fatalf("compensator for %q ran %d times, want at most 1", res, n)
		}
	}
}

func failTool(name string) Tool {
	return MustFunc(name, "", func(context.Context, struct{}) (struct{}, error) {
		return struct{}{}, errors.New(name + " failed")
	})
}

// Depth-3 tree: the deepest sub-agent fails → every level's writes reverse.
func TestSaga_DeepTreeFailureReverses(t *testing.T) {
	l := newLedger()
	store := memJournal()

	l3 := mustNew(
		&scriptModel{turns: [][]Emit{toolTurn("d1", "C", `{}`), toolTurn("d2", "boom3", `{}`)}},
		store,
		WithTools(l.write("C"), failTool("boom3")),
	)
	l2 := mustNew(
		&scriptModel{turns: [][]Emit{toolTurn("b1", "B", `{}`), toolTurn("b2", "l3", `{"task":"x"}`)}},
		store,
		WithTools(l.write("B"), MustSubAgent("l3", "", l3)),
	)
	l1 := mustNew(
		&scriptModel{turns: [][]Emit{toolTurn("a1", "A", `{}`), toolTurn("a2", "l2", `{"task":"x"}`)}},
		store,
		WithTools(l.write("A"), MustSubAgent("l2", "", l2)),
	)

	_, err := l1.Run(context.Background(), "root", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	l.assertClean(t)
	for _, r := range []string{"A", "B", "C"} {
		if l.undoCount[r] != 1 {
			t.Errorf("undo %q = %d, want 1", r, l.undoCount[r])
		}
	}
}

// Sibling sub-agents: A succeeds, B fails → A's writes (via parent recursion) and B's
// writes (self-rollback) and the parent's write all reverse, each once.
func TestSaga_SiblingSubAgentsReverse(t *testing.T) {
	l := newLedger()
	store := memJournal()

	subA := mustNew(
		&scriptModel{turns: [][]Emit{toolTurn("x1", "X", `{}`), textTurn("done")}},
		store,
		WithTools(l.write("X")),
	)
	subB := mustNew(
		&scriptModel{turns: [][]Emit{toolTurn("y1", "Y", `{}`), toolTurn("y2", "boomB", `{}`)}},
		store,
		WithTools(l.write("Y"), failTool("boomB")),
	)

	parent := mustNew(
		&scriptModel{turns: [][]Emit{
			toolTurn("p0", "P", `{}`),
			toolTurn("p1", "subA", `{"task":"x"}`),
			toolTurn("p2", "subB", `{"task":"x"}`),
		}},
		store,
		WithTools(l.write("P"), MustSubAgent("subA", "", subA), MustSubAgent("subB", "", subB)),
	)

	_, err := parent.Run(context.Background(), "root", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	l.assertClean(t)
	for _, r := range []string{"P", "X", "Y"} {
		if l.undoCount[r] != 1 {
			t.Errorf("undo %q = %d, want 1", r, l.undoCount[r])
		}
	}
}

// flakyStore fails the first N compensation steps, before their compensators run, to simulate a
// crash DURING rollback. A compensation step reads its record first (to run at most once) and
// calls the compensator only if it is not recorded, so failing that read stops the step there.
type flakyStore struct {
	*MemStore
	mu                sync.Mutex
	failCompensations int
	seen              int
}

func (f *flakyStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if strings.HasPrefix(name, "@saga/compensate/") {
		f.mu.Lock()
		f.seen++
		fail := f.seen <= f.failCompensations
		f.mu.Unlock()
		if fail {
			return Entry{}, false, errors.New("crash during rollback")
		}
	}
	return f.MemStore.Get(ctx, runID, name)
}

// Crash during rollback → resume → rollback completes, each compensator still runs once.
func TestSaga_CrashDuringRollbackResumes(t *testing.T) {
	l := newLedger()
	store := &flakyStore{MemStore: NewMemStore(), failCompensations: 1}
	j := mustJournal(store) // crash on the first compensation

	build := func() *Agent {
		return mustNew(
			&scriptModel{turns: [][]Emit{
				toolTurn("c1", "A", `{}`),
				toolTurn("c2", "Bw", `{}`),
				toolTurn("c3", "boom", `{}`),
			}},
			j,
			WithTools(l.write("A"), l.write("Bw"), failTool("boom")),
		)
	}

	// First attempt: the failing step trips the saga, then rollback itself crashes.
	_, err := build().Run(context.Background(), "root", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) || ab.CompensateErr == nil {
		t.Fatalf("first attempt err = %v, want *SagaAborted with CompensateErr", err)
	}

	// Resume: a fresh agent on the SAME store, compensation no longer failing.
	store.mu.Lock()
	store.failCompensations = 0
	store.mu.Unlock()

	_, err = build().Run(context.Background(), "root", UserText("go"), WithSaga())
	if !errors.As(err, &ab) {
		t.Fatalf("resume err = %v, want *SagaAborted", err)
	}
	l.assertClean(t) // nothing active, and — crucially — no compensator ran twice
	for _, r := range []string{"A", "Bw"} {
		if l.undoCount[r] != 1 {
			t.Errorf("undo %q = %d, want exactly 1 across crash+resume", r, l.undoCount[r])
		}
	}
}

// deepSagaReverses builds a `depth`-level nest of transactional agents — each level
// writes then delegates deeper; the deepest fails — and asserts the WHOLE tree reverses:
// every level's write undone exactly once, nothing left active.
func deepSagaReverses(t *testing.T, depth int) {
	l := newLedger()
	store := memJournal()

	var child *Agent
	for i := depth; i >= 1; i-- {
		res := fmt.Sprintf("w%d", i)
		if i == depth {
			child = mustNew(
				&scriptModel{turns: [][]Emit{
					toolTurn("wr", res, `{}`),
					toolTurn("bm", "boom", `{}`),
				}},
				store,
				WithTools(l.write(res), failTool("boom")),
			)
		} else {
			sub := MustSubAgent(fmt.Sprintf("sub%d", i+1), "", child)
			child = mustNew(
				&scriptModel{turns: [][]Emit{
					toolTurn("wr", res, `{}`),
					toolTurn("dl", sub.Spec().Name, `{"task":"x"}`),
				}},
				store,
				WithTools(l.write(res), sub),
			)
		}
	}

	_, err := child.Run(context.Background(), "root", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	l.assertClean(t)
	for i := 1; i <= depth; i++ {
		res := fmt.Sprintf("w%d", i)
		if l.undoCount[res] != 1 {
			t.Errorf("level %d (%q): undo count %d, want 1", i, res, l.undoCount[res])
		}
	}
}

func TestSaga_50LevelsDeep(t *testing.T)  { deepSagaReverses(t, 50) }
func TestSaga_100LevelsDeep(t *testing.T) { deepSagaReverses(t, 100) }

// SCALING NOTE (measured, not assumed): correctness holds at every depth that completes,
// but memory is O(depth) LIVE HEAP with a large per-level constant, because a synchronous
// agent tree keeps the whole ancestor chain alive (each parent is blocked waiting on its
// child, holding its conversation + maps). Measured: 50 <10ms; 500 ~4s; 2500 ~7.4GB.
// Running each sub-agent on its own goroutine did NOT fix this (confirmed: still 7.4GB at
// 2500) — the cost is heap live-set, not stack depth. Fine for real trees (single-digit
// depth; 100 here is far beyond realistic). A real fix would need heap profiling to shrink
// the per-level footprint; not worth it until a workload needs deep trees. See
// docs/KNOWN-LIMITATIONS.md.

// A completed write with no compensator inside the tree is surfaced, not silently lost.
func TestSaga_UncompensatedWriteSurfaced(t *testing.T) {
	l := newLedger()
	store := memJournal()

	// "danger" is a real write (Safety{}) with NO compensator.
	danger := MustFunc("danger", "", func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil })

	a := mustNew(
		&scriptModel{turns: [][]Emit{
			toolTurn("c1", "A", `{}`),
			toolTurn("c2", "danger", `{}`),
			toolTurn("c3", "boom", `{}`),
		}},
		store,
		WithTools(l.write("A"), danger, failTool("boom")),
	)

	_, err := a.Run(context.Background(), "root", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	found := false
	for _, u := range ab.Uncompensated {
		if u == "danger" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Uncompensated = %v, want it to include \"danger\"", ab.Uncompensated)
	}
}

type refundArgs struct {
	Amount int    `json:"amount"`
	Memo   string `json:"memo,omitempty"`
}

// compensateWith calls a CompensatedFunc's Compensate with recorded args and returns what undo saw.
func compensateWith(t *testing.T, args string) (refundArgs, error) {
	t.Helper()
	var saw refundArgs
	tool := MustCompensatedFunc("charge", "charge", func(context.Context, refundArgs) (string, error) { return "ok", nil },
		func(_ context.Context, in refundArgs, _ string) error { saw = in; return nil })
	err := tool.(Compensator).Compensate(context.Background(), json.RawMessage(args), json.RawMessage(`"ok"`))
	return saw, err
}

// Compensation decodes the recorded arguments as the forward call did. A record the strict
// forward decoder accepts yields the same value it gave the forward call.
func TestCompensate_StrictRecordIsTheForwardValue(t *testing.T) {
	for args, want := range map[string]refundArgs{
		`{"amount":5}`:                {Amount: 5},
		`{"amount":5,"memo":"m"}`:     {Amount: 5, Memo: "m"},
		` {"amount":0,"memo":null} `:  {Amount: 0},
		`{"memo":"x","amount":12345}`: {Amount: 12345, Memo: "x"},
	} {
		got, err := compensateWith(t, args)
		if err != nil || got != want {
			t.Errorf("Compensate(%s) saw %+v, %v; want %+v", args, got, err, want)
		}
		// The forward call accepts the same record and decodes the same value.
		var fwd refundArgs
		if err := decodeArgs(json.RawMessage(args), &fwd); err != nil || fwd != got {
			t.Errorf("forward decode of %s = %+v, %v; compensation saw %+v", args, fwd, err, got)
		}
	}
}

// A record written before tool arguments decoded strictly may hold arguments only encoding/json
// accepts (a case variant, an unknown or duplicate name, a missing or null required field). The
// forward call of that time decoded them with encoding/json, so compensation decodes them the same
// way and undoes the value the call acted on, rather than refusing to compensate.
func TestCompensate_OlderLooseRecordIsTheValueItsCallDecoded(t *testing.T) {
	for args, want := range map[string]refundArgs{
		`{"AMOUNT":7}`:              {Amount: 7},
		`{"amount":7,"extra":true}`: {Amount: 7},
		`{"amount":1,"amount":7}`:   {Amount: 7},
		`{"memo":"m"}`:              {Memo: "m"},
		`{"amount":null}`:           {},
		``:                          {},
	} {
		got, err := compensateWith(t, args)
		if err != nil || got != want {
			t.Errorf("Compensate(%q) saw %+v, %v; want %+v", args, got, err, want)
		}
	}
}

// Arguments no decoder accepts are ErrProtocol: compensation never guesses a value.
func TestCompensate_UndecodableRecordIsAnError(t *testing.T) {
	for _, args := range []string{`{"amount":"five"}`, `[1]`, `{"amount":5} {}`} {
		if got, err := compensateWith(t, args); !errors.Is(err, ErrProtocol) {
			t.Errorf("Compensate(%s) saw %+v, %v; want ErrProtocol", args, got, err)
		}
	}
}

type chargeArgs struct {
	Amount int `json:"amount"`
}

// scaleCharge is a tool middleware that rewrites a charge's amount (say, dollars to cents) before
// the tool runs: the tool charges 100 times what the model asked for.
func scaleCharge(next ToolHandler) ToolHandler {
	return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
		if call.Use.Name == "charge" {
			var in chargeArgs
			if err := json.Unmarshal(call.Use.Args, &in); err == nil {
				call.Use.Args, _ = json.Marshal(chargeArgs{Amount: in.Amount * 100})
			}
		}
		return next(ctx, call)
	}
}

// rewrittenChargeSaga runs a saga that charges (through scaleCharge) and then fails, so the charge is
// compensated. It returns what was charged and what was refunded.
func rewrittenChargeSaga(t *testing.T, store *Journal, safety Safety, mw ...ToolMiddleware) (charged, refunded int, err error) {
	t.Helper()
	charge := MustCompensatedFunc("charge", "charge the card", func(_ context.Context, in chargeArgs) (string, error) { charged = in.Amount; return "ok", nil },
		func(_ context.Context, in chargeArgs, _ string) error { refunded = in.Amount; return nil }, WithSafety(safety))
	fail := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		return "", errors.New("no seats")
	})
	m := NewScriptedModel(ToolTurn("c1", "charge", `{"amount":5}`), ToolTurn("b1", "book", `{}`), TextTurn("done"))
	_, err = mustNew(m, store, WithTools(charge, fail), WithToolMiddleware(mw...)).Run(context.Background(), "r", UserText("trip"), WithSaga())
	return charged, refunded, err
}

// Compensation undoes what the tool did: the arguments the tool accepted, after tool middleware.
// It used to take the model's arguments from the journal, so a middleware that rewrote a charge
// from 5 to 500 charged 500 and refunded 5.
func TestSaga_CompensatesTheArgumentsTheToolAccepted(t *testing.T) {
	for _, safety := range []Safety{{}, {Idempotent: true}} {
		charged, refunded, err := rewrittenChargeSaga(t, memJournal(), safety, scaleCharge)
		var aborted *SagaAborted
		if !errors.As(err, &aborted) || aborted.CompensateErr != nil {
			t.Fatalf("safety %+v: saga Run = %v, want a clean *SagaAborted", safety, err)
		}
		if charged != 500 || refunded != 500 {
			t.Errorf("safety %+v: charged %d, refunded %d; want the refund to undo the 500 charged", safety, charged, refunded)
		}
	}
}

// The accepted arguments are journaled only when they matter: in a saga, for a compensable call,
// when a middleware changed them. Without a rewrite the journal is as before, and compensation
// reads the model's arguments, which are the ones the tool accepted.
func TestSaga_AcceptedArgumentsAreJournaledOnlyWhenRewritten(t *testing.T) {
	j := memJournal()
	charged, refunded, _ := rewrittenChargeSaga(t, j, Safety{})
	if charged != 5 || refunded != 5 {
		t.Fatalf("charged %d, refunded %d; want 5 and 5", charged, refunded)
	}
	recs, _ := j.History(context.Background(), "r")
	for _, r := range recs {
		if r.Name == sagaArgsStep("c1") {
			t.Fatalf("a call whose arguments no middleware changed journaled them: %+v", r)
		}
	}

	// A Run (not a saga) never compensates, so it journals no arguments either.
	var ran atomic.Int32
	charge := MustCompensatedFunc("charge", "charge the card", func(context.Context, chargeArgs) (string, error) { ran.Add(1); return "ok", nil },
		func(context.Context, chargeArgs, string) error { return nil })
	j = memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{"amount":5}`), TextTurn("done"))
	if _, err := mustNew(m, j, WithTools(charge), WithToolMiddleware(scaleCharge)).Run(context.Background(), "r", UserText("go")); err != nil || ran.Load() != 1 {
		t.Fatalf("Run = %v after %d charges", err, ran.Load())
	}
	recs, _ = j.History(context.Background(), "r")
	for _, r := range recs {
		if r.Name == sagaArgsStep("c1") {
			t.Fatalf("a Run journaled a call's arguments: %+v", r)
		}
	}
}

// The accepted arguments are journaled before the side effect fires, so a charge whose outcome
// became unknown (the resume halts on it) is, once an operator resolves the halt and the saga
// later aborts, refunded by what the tool was given, not by what the model asked for.
func TestSaga_ResolvedUnknownOutcomeCompensatesTheAcceptedArguments(t *testing.T) {
	var charged, refunded atomic.Int32
	store := memJournal()
	build := func() *Agent {
		charge := MustCompensatedFunc("charge", "charge the card", func(_ context.Context, in chargeArgs) (string, error) {
			charged.Store(int32(in.Amount))
			return "", ErrToolOutcomeUnknown // the connection dropped after the request went out
		},
			func(_ context.Context, in chargeArgs, _ string) error { refunded.Store(int32(in.Amount)); return nil })
		fail := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
			return "", errors.New("no seats")
		})
		m := NewScriptedModel(ToolTurn("c1", "charge", `{"amount":5}`), ToolTurn("b1", "book", `{}`), TextTurn("done"))
		return mustNew(m, store, WithTools(charge, fail), WithToolMiddleware(scaleCharge))
	}
	if _, err := build().Run(context.Background(), "r", UserText("trip"), WithSaga()); !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("saga Run = %v, want ErrToolOutcomeUnknown", err)
	}
	_, err := build().Run(context.Background(), "r", UserText("trip"), WithSaga()) // the resume halts on the charge
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "c1" {
		t.Fatalf("resumed saga Run = %v, want a OutcomeUnknown on the charge", err)
	}
	if err := ResolveHalt(context.Background(), store, HaltRef{RunID: halt.RunID, Op: OpRef{Kind: OpTool, ID: halt.Op.ID}, Cause: HaltCrashed}, Outcome{Result: "charged (operator-confirmed)", IsError: false}); err != nil {
		t.Fatalf("ResolveHalt: %v", err)
	}
	_, err = build().Run(context.Background(), "r", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil {
		t.Fatalf("resumed saga Run = %v, want a clean *SagaAborted", err)
	}
	if charged.Load() != 500 || refunded.Load() != 500 {
		t.Fatalf("charged %d, refunded %d; want the refund to undo the 500 charged", charged.Load(), refunded.Load())
	}
}

// A journal written before accepted arguments were journaled has none: compensation falls back to
// the model's arguments, as it always did.
func TestSaga_JournalWithoutAcceptedArgumentsUsesTheModelArguments(t *testing.T) {
	// No middleware: nothing is journaled, which is exactly what an older journal looks like.
	charged, refunded, err := rewrittenChargeSaga(t, memJournal(), Safety{})
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || charged != 5 || refunded != 5 {
		t.Fatalf("saga Run = %v; charged %d, refunded %d; want 5 and 5", err, charged, refunded)
	}
}

// A retry-safe compensable call that a sibling's failure cut off has no recorded result, so the
// rollback runs it again to learn the result it compensates. That run goes through the tool
// middleware like the first: it used to call the tool directly with the model's arguments, so
// the charge the live call made at 500 was repeated at 5 and refunded at 5.
func TestSaga_RollbackRerunGoesThroughToolMiddleware(t *testing.T) {
	var calls atomic.Int32
	var charges, refunded []int
	var mu sync.Mutex
	record := func(list *[]int, v int) { mu.Lock(); *list = append(*list, v); mu.Unlock() }
	// book fails only once the live charge has started: a call the failure reaches before it
	// starts never starts, and would leave the rollback's re-run the first call.
	chargeStarted := make(chan struct{})
	charge := MustCompensatedFunc("charge", "charge the card", func(ctx context.Context, in chargeArgs) (string, error) {
		record(&charges, in.Amount)
		if calls.Add(1) == 1 {
			close(chargeStarted)
			<-ctx.Done() // the live call is cut off waiting for its response
			return "", ctx.Err()
		}
		return "ok", nil
	},
		func(_ context.Context, in chargeArgs, _ string) error { record(&refunded, in.Amount); return nil }, WithSafety(Safety{Idempotent: true}))
	book := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		<-chargeStarted
		return "", errors.New("no seats")
	})
	m := &sagaTurns{turns: [][][3]string{{{"c1", "charge", `{"amount":5}`}, {"b1", "book", `{}`}}}}
	_, err := mustNew(m, memJournal(), WithTools(charge, book), WithToolMiddleware(scaleCharge)).Run(context.Background(), "r", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil {
		t.Fatalf("saga Run = %v, want a clean *SagaAborted", err)
	}
	if len(charges) != 2 || charges[0] != 500 || charges[1] != 500 || len(refunded) != 1 || refunded[0] != 500 {
		t.Fatalf("charges %v, refunds %v; want the re-run to charge 500 like the live call, and a refund of 500", charges, refunded)
	}
}

// Only a compensable call journals its accepted arguments: a rewritten call to a tool with no
// compensator has nothing to undo them with.
func TestSaga_NonCompensableCallJournalsNoArguments(t *testing.T) {
	charge := MustFunc("charge", "charge the card", func(context.Context, chargeArgs) (string, error) { return "ok", nil })
	fail := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		return "", errors.New("no seats")
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{"amount":5}`), ToolTurn("b1", "book", `{}`), TextTurn("done"))
	_, _ = mustNew(m, store, WithTools(charge, fail), WithToolMiddleware(scaleCharge)).Run(context.Background(), "r", UserText("trip"), WithSaga())
	recs, _ := store.History(context.Background(), "r")
	for _, r := range recs {
		if r.Name == sagaArgsStep("c1") {
			t.Fatalf("a call with no compensator journaled its arguments: %+v", r)
		}
	}
}

// failArgsStore fails the write of the accepted-arguments record.
type failArgsStore struct{ *MemStore }

func (s failArgsStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == sagaArgsStep("c1") {
		return Entry{}, false, errors.New("disk full")
	}
	return s.MemStore.Insert(ctx, runID, name, data)
}

// A compensable call whose accepted arguments cannot be journaled does not run: compensation
// could not undo what it would do.
func TestSaga_UnjournaledArgumentsStopTheCall(t *testing.T) {
	charged, _, err := rewrittenChargeSaga(t, mustJournal(failArgsStore{NewMemStore()}), Safety{}, scaleCharge)
	if charged != 0 || !errors.Is(err, ErrStorage) {
		t.Fatalf("charged %d, saga Run = %v; want no charge and ErrStorage", charged, err)
	}
}

// A retry-safe compensable call cut off inside a middleware, before the tool ran, journaled no
// arguments; the rollback's re-run, through the middleware and in the saga, journals them, so the
// refund undoes the 500 the re-run charged.
func TestSaga_RollbackRerunJournalsTheAcceptedArguments(t *testing.T) {
	var charges, refunds atomic.Int32
	charge := MustCompensatedFunc("charge", "charge the card", func(_ context.Context, in chargeArgs) (string, error) {
		charges.Store(int32(in.Amount))
		return "ok", nil
	},
		func(_ context.Context, in chargeArgs, _ string) error { refunds.Store(int32(in.Amount)); return nil }, WithSafety(Safety{Idempotent: true}))
	// book fails only once the live charge has started: a call the failure reaches before it
	// starts never starts, and would leave the rollback's re-run the one that stalls.
	chargeStarted := make(chan struct{})
	book := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		<-chargeStarted
		return "", errors.New("no seats")
	})
	var live atomic.Bool
	stall := func(next ToolHandler) ToolHandler { // the live call stalls in the middleware, before the tool
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name == "charge" && live.CompareAndSwap(false, true) {
				close(chargeStarted)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return next(ctx, call)
		}
	}
	m := &sagaTurns{turns: [][][3]string{{{"c1", "charge", `{"amount":5}`}, {"b1", "book", `{}`}}}}
	_, err := mustNew(m, memJournal(), WithTools(charge, book), WithToolMiddleware(stall, scaleCharge)).Run(context.Background(), "r", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil {
		t.Fatalf("saga Run = %v, want a clean *SagaAborted", err)
	}
	if charges.Load() != 500 || refunds.Load() != 500 {
		t.Fatalf("charged %d, refunded %d; want the refund to undo the 500 the re-run charged", charges.Load(), refunds.Load())
	}
}

// failArgsReadStore fails every read that would return the accepted-arguments record (a Get of
// it, or a Load that reaches it): the read the rollback makes after a re-run journaled it.
type failArgsReadStore struct{ *MemStore }

func (s failArgsReadStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	e, ok, err := s.MemStore.Get(ctx, runID, name)
	if ok && name == sagaArgsStep("c1") {
		return Entry{}, false, errors.New("read failed")
	}
	return e, ok, err
}

func (s failArgsReadStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		for e, err := range s.MemStore.Load(ctx, runID, after) {
			if err == nil && e.Name == sagaArgsStep("c1") {
				yield(Entry{}, errors.New("read failed"))
				return
			}
			if !yield(e, err) {
				return
			}
		}
	}
}

// When the rollback cannot read back the arguments its re-run accepted, it stops with that error
// rather than compensate with the model's arguments.
func TestSaga_RollbackRerunStopsWhenItCannotReadTheArguments(t *testing.T) {
	var refunds atomic.Int32
	charge := MustCompensatedFunc("charge", "charge the card", func(context.Context, chargeArgs) (string, error) { return "ok", nil },
		func(_ context.Context, in chargeArgs, _ string) error { refunds.Store(int32(in.Amount)); return nil }, WithSafety(Safety{Idempotent: true}))
	chargeStarted := make(chan struct{}) // book fails once the live charge has started
	book := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		<-chargeStarted
		return "", errors.New("no seats")
	})
	var live atomic.Bool
	stall := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name == "charge" && live.CompareAndSwap(false, true) {
				close(chargeStarted)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return next(ctx, call)
		}
	}
	m := &sagaTurns{turns: [][][3]string{{{"c1", "charge", `{"amount":5}`}, {"b1", "book", `{}`}}}}
	_, err := mustNew(
		m,
		mustJournal(failArgsReadStore{NewMemStore()}),
		WithTools(charge, book),
		WithToolMiddleware(stall, scaleCharge),
	).Run(context.Background(), "r", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr == nil || refunds.Load() != 0 {
		t.Fatalf("saga Run = %v, refunded %d; want the rollback stopped with the read error and no refund", err, refunds.Load())
	}
}

// sagaJournal is a saga run's journal in which the model called c1, followed by recs.
func sagaJournal(recs ...Record) []Record {
	asst := &Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "charge", Args: json.RawMessage(`{}`)}}}
	return append([]Record{{Name: "@llm/0", Kind: StepModel, Message: asst}}, recs...)
}

func tallyRecord(t ApprovalTally) Record {
	b, _ := json.Marshal(t)
	return Record{Name: ApprovalTallyStep("c1"), Kind: StepValue, Result: b}
}

// What aborts a saga: a recorded step failure, and a failure an operator recorded for a step a
// crash cut off (ResolveHalt with isError). Not a human's denial, 1-of-1 or m-of-n, which the
// model reacts to; not a success.
func TestSagaFailure(t *testing.T) {
	failed := Record{Name: "c1", Kind: StepToolResult, ToolUseID: "c1", IsError: true, Result: json.RawMessage(`"card declined"`)}
	quorum := ApprovalTally{Need: 2, Approvers: []string{"ops", "finance"}}
	deniedQuorum, passedQuorum := quorum, quorum
	deniedQuorum.Denied = 1
	passedQuorum.Approved = 2
	for _, tc := range []struct {
		name      string
		recs      []Record
		wantCause string
		want      bool
	}{
		{"recorded failure", sagaJournal(Record{Name: "c1", Kind: StepSagaFail, ToolUseID: "c1", Result: json.RawMessage(`"boom"`)}), "boom", true},
		{"reconciled failure", sagaJournal(Record{Name: "attempt:c1", Kind: StepAttempt, ToolUseID: "c1"}, failed), "card declined", true},
		{"reconciled failure recorded as JSON", sagaJournal(Record{Name: "c1", Kind: StepToolResult, ToolUseID: "c1", IsError: true, Result: json.RawMessage(`{"code":402}`)}), `{"code":402}`, true},
		{"reconciled failure after an approval", sagaJournal(Record{Name: "approval:c1", Kind: StepApproval, ToolUseID: "c1", Approved: true}, failed), "card declined", true},
		{"reconciled failure after a passed quorum", sagaJournal(tallyRecord(passedQuorum), failed), "card declined", true},
		{"reconciled failure after a quorum passed over a denial", sagaJournal(
			Record{Name: "approval:c1:risk", Kind: StepApproval, ToolUseID: "c1", ApproverSignature: &ApproverSignature{Approver: "risk"}}, tallyRecord(passedQuorum), failed), "card declined", true},
		{"denial", sagaJournal(Record{Name: "approval:c1", Kind: StepApproval, ToolUseID: "c1"}, failed), "", false},
		{"quorum denial", sagaJournal(tallyRecord(deniedQuorum), failed), "", false},
		{"success", sagaJournal(Record{Name: "c1", Kind: StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{}`)}), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause, got, err := sagaFailure("r", tc.recs)
			if err != nil || got != tc.want || cause != tc.wantCause {
				t.Fatalf("sagaFailure = %q, %v; want %q, %v", cause, got, tc.wantCause, tc.want)
			}
		})
	}
}

// Uncompensated lists every write the rollback did not undo, for whatever reason: one with no
// compensator, one whose outcome is unknown (the rollback halted on it, compensator or not), or one
// whose tool is gone. The message says that, not that each lacked a compensator.
func TestSagaAborted_MessageDoesNotBlameAMissingCompensator(t *testing.T) {
	e := &SagaAborted{RunID: "r", Cause: errors.New("boom"), Uncompensated: []string{"charge"},
		CompensateErr: toolHalt("r", "r", "c1", "charge", time.Time{}, HaltCrashed)}
	msg := e.Error()
	if strings.Contains(msg, "no compensator") || !strings.Contains(msg, "charge") || !strings.Contains(msg, "INCOMPLETE") {
		t.Fatalf("Error() = %q; want the uncompensated writes listed without blaming a missing compensator", msg)
	}
}

// A saga rollback that recurses into a wrapped sub-agent binds the wrapper's context first. When the
// wrapper cannot (the grant it narrowed to is gone), the delegation is reported uncompensated and
// the compensation inside it never runs: it must not run under authority the delegation did not
// grant.
func TestSaga_RollbackStopsWhenAWrapperCannotBindItsContext(t *testing.T) {
	ctx := context.Background()
	var compensated atomic.Int32
	pay := MustCompensatedFunc("pay", "", func(context.Context, struct{}) (string, error) { return "paid", nil },
		func(context.Context, struct{}, string) error { compensated.Add(1); return nil })
	book := MustFunc("book", "", func(context.Context, struct{}) (string, error) { return "", errors.New("no seats") })

	run := func(t *testing.T, bindErr error) (error, int32) {
		compensated.Store(0)
		subModel := &sagaTurns{turns: [][][3]string{{{"p1", "pay", `{}`}}}}
		sub := mustNew(subModel, memJournal(), WithTools(pay))
		inner := MustSubAgent("research", "d", sub)
		wrapped := covBinder{covWrapper{inner: inner, bind: func(ctx context.Context, id string) (context.Context, error) {
			return ctx, bindErr
		}}}
		parent := &sagaTurns{turns: [][][3]string{{{"c1", "research", `{"task":"look"}`}}, {{"c2", "book", `{}`}}}}
		a := mustNew(parent, memJournal(), WithTools(wrapped, book))
		_, err := a.Run(ctx, "r", UserText("go"), WithSaga())
		return err, compensated.Load()
	}

	t.Run("binds", func(t *testing.T) {
		err, comps := run(t, nil)
		var sa *SagaAborted
		if !errors.As(err, &sa) || sa.CompensateErr != nil || comps != 1 {
			t.Fatalf("Run = %v, %d compensations; want a clean abort that compensated the payment once", err, comps)
		}
	})
	t.Run("cannot bind", func(t *testing.T) {
		err, comps := run(t, errors.New("grant gone"))
		var sa *SagaAborted
		if !errors.As(err, &sa) {
			t.Fatalf("Run = %v; want *SagaAborted", err)
		}
		if sa.CompensateErr == nil || !strings.Contains(sa.CompensateErr.Error(), "grant gone") {
			t.Errorf("CompensateErr = %v; want the bind failure", sa.CompensateErr)
		}
		if !contains(sa.Uncompensated, "research") {
			t.Errorf("Uncompensated = %v; want the delegation named", sa.Uncompensated)
		}
		if comps != 0 {
			t.Errorf("%d compensations ran under a context that could not be bound", comps)
		}
	})
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// A saga whose history cannot be read is not driven: the error is the store's, and no model call is
// made on a run whose state is unknown.
func TestSaga_UnreadableHistoryIsNotDriven(t *testing.T) {
	m := &countModel{inner: &scriptModel{turns: [][]Emit{textTurn("x")}}}
	a := mustNew(m, mustJournal(loadFails{NewMemStore()}))
	if _, err := a.Run(context.Background(), "r", UserText("go"), WithSaga()); !errors.Is(err, ErrStorage) {
		t.Fatalf("Run = %v; want ErrStorage", err)
	}
	if m.calls.Load() != 0 {
		t.Fatal("the model was called for a saga whose history could not be read")
	}
}
