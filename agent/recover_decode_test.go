package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// prCountingStore counts the Gets of run:start through it. It unwraps to the store it wraps, so
// Recover finds that store's Lister and Leaser.
type prCountingStore struct {
	Store
	startGets atomic.Int64
}

func (s *prCountingStore) Unwrap() Store { return s.Store }

func (s *prCountingStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if name == runStartStep {
		s.startGets.Add(1)
	}
	return s.Store.Get(ctx, runID, name)
}

// prJournal returns a journal over a fresh MemStore that counts its run:start Gets.
func prJournal(t *testing.T) (*Journal, *prCountingStore) {
	t.Helper()
	cs := &prCountingStore{Store: NewMemStore()}
	j, err := NewJournal(cs)
	if err != nil {
		t.Fatal(err)
	}
	return j, cs
}

// prPut records a value step name in run id.
func prPut(t *testing.T, j *Journal, id, name string, result string) {
	t.Helper()
	rec := Record{Kind: StepValue}
	if result != "" {
		rec.Result = []byte(result)
	}
	if _, err := j.put(context.Background(), id, name, rec); err != nil {
		t.Fatalf("put %s/%s: %v", id, name, err)
	}
}

// prStart records st as run id's run:start.
func prStart(t *testing.T, j *Journal, id string, st RunStart) {
	t.Helper()
	b, err := marshalJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	prPut(t, j, id, runStartStep, string(b))
}

// Rule 6: Recover excludes the runs holding run:complete, run:aborted or run:cancelled, and not a
// saga holding only its rollback request (run:cancel-requested), which it hands to the resumer
// with its run:start.
func TestP14Rule06_CancelRequestedSagaIsRecovered(t *testing.T) {
	ctx := context.Background()
	j, _ := prJournal(t)
	prStart(t, j, "requested", RunStart{Input: UserText("trip"), Saga: true})
	prPut(t, j, "requested", runCancelRequestedStep, `{"reason":"stop"}`)
	for _, end := range endOfRunMarkers {
		prStart(t, j, "ended-"+end, RunStart{Input: UserText("x"), Saga: true})
		prPut(t, j, "ended-"+end, end, "")
	}
	var got []string
	var starts []RunStart
	n, err := Recover(ctx, j, func(_ context.Context, id string, st RunStart) error {
		got, starts = append(got, id), append(starts, st)
		return nil
	})
	if err != nil || n != 1 || len(got) != 1 || got[0] != "requested" {
		t.Fatalf("Recover = %d, %v, resumed %v; want only the saga holding its rollback request", n, err, got)
	}
	if !starts[0].Saga || starts[0].Input.Text() != "trip" {
		t.Fatalf("resumer got start %+v; want the run's run:start (a saga answering \"trip\")", starts[0])
	}
}

// Rule 14: recovery reads run:start under the lease for every run it visits; a run with none is
// skipped (not resumed, not counted) and reported (ErrNotStarted) once per process, however many
// passes visit it.
func TestP14Rule14_NotStartedSkippedAndReportedOnce(t *testing.T) {
	ctx := context.Background()
	j, cs := prJournal(t)
	prPut(t, j, "mistyped", signalStep("go"), "")
	resumed := 0
	resume := func(context.Context, string, RunStart) error { resumed++; return nil }
	n, err := Recover(ctx, j, resume)
	if n != 0 || resumed != 0 || !errors.Is(err, ErrNotStarted) {
		t.Fatalf("first pass = %d, %v (resumed %d); want 0 and ErrNotStarted", n, err, resumed)
	}
	n, err = Recover(ctx, j, resume)
	if n != 0 || resumed != 0 || err != nil {
		t.Fatalf("second pass = %d, %v (resumed %d); want 0 and no second report", n, err, resumed)
	}
	if g := cs.startGets.Load(); g != 2 {
		t.Fatalf("run:start read %d times over two passes; want once per pass", g)
	}
}

// Rule 15 (model 10, finding L7, findings/not-started-remembered): what the process remembers is
// the report, not the skip. A pass finds the run before its first drive wrote run:start (a worker
// took its lease first); the first drive then writes run:start and dies; the next pass recovers it.
func TestP14Rule15_StartedAfterSkipIsRecovered(t *testing.T) {
	ctx := context.Background()
	j, _ := prJournal(t)
	prPut(t, j, "r", signalStep("early"), "") // the run exists, with no run:start yet
	var resumed []RunStart
	resume := func(_ context.Context, _ string, st RunStart) error { resumed = append(resumed, st); return nil }
	if n, err := Recover(ctx, j, resume); n != 0 || !errors.Is(err, ErrNotStarted) {
		t.Fatalf("pass before the first drive = %d, %v; want the run skipped and reported", n, err)
	}
	prStart(t, j, "r", RunStart{Input: UserText("late")}) // the first drive starts the run, then dies
	n, err := Recover(ctx, j, resume)
	if err != nil || n != 1 || len(resumed) != 1 || resumed[0].Input.Text() != "late" {
		t.Fatalf("pass after the first drive = %d, %v, resumed %+v; want the run recovered with its start", n, err, resumed)
	}
}

// RecoverLoop reports a not-started run to WithRecoverErrors once, however many passes read it.
func TestP14Rule14_RecoverLoopReportsNotStartedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j, cs := prJournal(t)
		prPut(t, j, "mistyped", signalStep("go"), "")
		var mu sync.Mutex
		var reports []error
		var resumed atomic.Int64
		stop := runLoop(t, j, func(context.Context, string, RunStart) error {
			resumed.Add(1)
			return nil
		}, WithRecoverInterval(10*time.Millisecond), WithRecoverErrors(func(err error) {
			mu.Lock()
			reports = append(reports, err)
			mu.Unlock()
		}))
		for i := 0; cs.startGets.Load() < 4 && i < 100; i++ {
			time.Sleep(10 * time.Millisecond) // synctest: fake time, advances only when every goroutine waits
		}
		if err := stop(); !errors.Is(err, context.Canceled) {
			t.Fatalf("RecoverLoop = %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if g := cs.startGets.Load(); g < 4 {
			t.Fatalf("run:start read %d times in 100 intervals; want a read every pass", g)
		}
		if len(reports) != 1 || !errors.Is(reports[0], ErrNotStarted) || resumed.Load() != 0 {
			t.Fatalf("reports = %v, resumed %d; want one ErrNotStarted over %d passes and no resume", reports, resumed.Load(), cs.startGets.Load())
		}
	})
}

// A run no Resumer drives (ErrNotResumable) is reported once per process, is not a failure on
// later passes, and is not counted as recovered.
func TestP14Recover_NotResumableReportedOnce(t *testing.T) {
	ctx := context.Background()
	j, _ := prJournal(t)
	prStart(t, j, "flow", RunStart{Input: UserText("{}"), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}})
	calls := 0
	resume := func(context.Context, string, RunStart) error {
		calls++
		return fmt.Errorf("not mine: %w", ErrNotResumable)
	}
	n, err := Recover(ctx, j, resume)
	if n != 0 || !errors.Is(err, ErrNotResumable) {
		t.Fatalf("first pass = %d, %v; want 0 and ErrNotResumable", n, err)
	}
	n, err = Recover(ctx, j, resume)
	if n != 0 || err != nil || calls != 2 {
		t.Fatalf("second pass = %d, %v (calls %d); want 0, no second report, the resumer asked again", n, err, calls)
	}
}

// ResumeAny hands a run to each Resumer in order until one does not return ErrNotResumable.
func TestP14ResumeAny_Order(t *testing.T) {
	ctx := context.Background()
	var order []string
	r := func(name string, err error) Resumer {
		return func(context.Context, string, RunStart) error { order = append(order, name); return err }
	}
	boom := errors.New("boom")
	if err := ResumeAny(r("a", ErrNotResumable), r("b", fmt.Errorf("x: %w", ErrNotResumable)), r("c", boom), r("d", nil))(ctx, "r", RunStart{}); !errors.Is(err, boom) {
		t.Fatalf("ResumeAny = %v; want c's error", err)
	}
	if fmt.Sprint(order) != "[a b c]" {
		t.Fatalf("order = %v; want [a b c]", order)
	}
	order = nil
	if err := ResumeAny(r("a", nil), r("b", nil))(ctx, "r", RunStart{}); err != nil || fmt.Sprint(order) != "[a]" {
		t.Fatalf("ResumeAny = %v, order %v; want nil after a", err, order)
	}
	if err := ResumeAny(r("a", ErrNotResumable))(ctx, "r", RunStart{}); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("ResumeAny with none willing = %v; want ErrNotResumable", err)
	}
	if err := ResumeAny()(ctx, "r", RunStart{}); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("ResumeAny() = %v; want ErrNotResumable", err)
	}
	if err := ResumeAny(r("a", ErrNotResumable), nil)(ctx, "r", RunStart{}); !errors.Is(err, ErrConfig) {
		t.Fatalf("ResumeAny with a nil Resumer = %v; want ErrConfig", err)
	}
}

// A nil Resumer is ErrConfig for Recover and RecoverLoop.
func TestP14Recover_NilResumer(t *testing.T) {
	j, _ := prJournal(t)
	if _, err := Recover(context.Background(), j, nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("Recover(nil) = %v; want ErrConfig", err)
	}
	if err := RecoverLoop(context.Background(), j, nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("RecoverLoop(nil) = %v; want ErrConfig", err)
	}
}

// strictStart is the full decoding of a stored run:start record: the record through
// decodeStored, then its result as a RunStart. ok is false for a record that is not a value
// record.
func strictStart(t *testing.T, b []byte) (RunStart, bool, error) {
	t.Helper()
	r, err := decodeStored("run", runStartStep, b)
	if err != nil {
		return RunStart{}, false, err
	}
	if r.Kind != StepValue {
		return RunStart{}, false, nil
	}
	var st RunStart
	if err := json.Unmarshal(r.Result, &st); err != nil {
		return RunStart{}, false, err
	}
	return st, true, nil
}

// testSalt is a salt of SaltSize bytes, as the journal gives every record it records.
var testSalt = []byte("0123456789abcdef0123456789abcdef")

// A recovery pass decodes run:start in one pass, reading only the record's name and kind and the
// start (decodeStartEntry). For every start a drive journals, that is the start the full decoding
// reads; anything else is left to the full decoding.
func TestP14_RecoveryStartDecodeMatchesFullDecode(t *testing.T) {
	turn := 3
	maxTurns, budget := 7, 900
	prompt := "be brief"
	starts := map[string]RunStart{
		"plain":   {Input: UserText("go")},
		"empty":   {},
		"escaped": {Input: UserText("a \"quote\", a \\ and  ")},
		"image":   {Input: Message{Role: RoleUser, Parts: []Part{Text{Text: "look"}, ImageData("image/png", []byte{1, 2, 3})}}},
		"saga":    {Input: UserText("go"), Saga: true},
		"session": {Input: UserText("hi"), Kind: RunKindSessionTurn, Session: &SessionRef{ID: "s1", Turn: &turn}},
		"flow":    {Input: UserText(`{"a":1}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}},
		"typed":   {Input: UserText("q"), Typed: &TypedStart{Mode: OutputNative, SchemaDigest: "d", Schema: json.RawMessage(`{"type":"object"}`)}},
		"options": {Input: UserText("go"), Settings: RunSettings{MaxTurns: &maxTurns, TokenBudget: &budget, SystemPrompt: &prompt},
			Principal: &Principal{OnBehalfOf: "u", AuthorityRef: "a"}, Tools: []string{"x", "y"},
			Ext: map[string]json.RawMessage{"audit.grant": json.RawMessage(`"g"`)}},
	}
	for name, want := range starts {
		t.Run(name, func(t *testing.T) {
			res, err := marshalJournal(want)
			if err != nil {
				t.Fatal(err)
			}
			// A journaled record carries a salt (JournalEntry); without one it takes the full decoding.
			b, err := EncodeRecord(Record{Name: runStartStep, Kind: StepValue, Result: res, salt: testSalt})
			if err != nil {
				t.Fatal(err)
			}
			strict, ok, err := strictStart(t, b)
			if err != nil || !ok {
				t.Fatalf("full decoding = %v, %v", ok, err)
			}
			got, ok := decodeStartEntry(b)
			if !ok {
				t.Fatalf("decodeStartEntry refused %s", b)
			}
			if !reflect.DeepEqual(got, strict) {
				t.Fatalf("decodeStartEntry = %#v\nthe full decoding = %#v", got, strict)
			}
		})
	}

	// What the one-pass decoding does not take, the full decoding reports.
	tomb := []byte(`{"redacted":{"leaf_hash":"abababababababababababababababababababababababababababababababab","at_ms":1}}`)
	other, _ := EncodeRecord(Record{Name: runStartStep, Kind: StepToolResult, Result: json.RawMessage(`{"input":"go"}`)})
	renamed, _ := EncodeRecord(Record{Name: "run:other", Kind: StepValue, Result: json.RawMessage(`{"input":"go"}`)})
	badInput, _ := EncodeRecord(Record{Name: runStartStep, Kind: StepValue, Result: json.RawMessage(`{"input":5}`)})
	noResult, _ := EncodeRecord(Record{Name: runStartStep, Kind: StepValue})
	nullResult := []byte(`{"name":"run:start","kind":"value","result":null}`)
	noInput := []byte(`{"name":"run:start","kind":"value","result":{"saga":true}}`)
	escapedName := []byte(`{"name":"run\u003astart","kind":"value","result":{"input":"go"}}`)
	escapedKind := []byte(`{"name":"run:start","kind":"valu\u0065","result":{"input":"go"}}`)
	renamedLast := []byte(`{"name":"run:start","kind":"value","result":{"input":"go"},"name":"run:other"}`)
	for name, b := range map[string][]byte{
		"tombstone": tomb, "other kind": other, "other name": renamed, "bad input": badInput,
		"no result": noResult, "not json": []byte(`{"name":`), "null result": nullResult, "no input": noInput,
		"escaped name": escapedName, "escaped kind": escapedKind, "renamed by a later member": renamedLast,
	} {
		t.Run(name, func(t *testing.T) {
			if st, ok := decodeStartEntry(b); ok {
				t.Fatalf("decodeStartEntry took %s as %#v; the full decoding must read it", b, st)
			}
		})
	}
}

// A record the one-pass decoding leaves to the full decoding reads as the full decoding reads it:
// a name or kind spelled with escapes is still run:start's, and a result with no input is a start
// with no input.
func TestStartUnderLeaseFallsBackToTheFullDecoding(t *testing.T) {
	ctx := context.Background()
	for name, b := range map[string][]byte{
		"escaped name": []byte(`{"name":"run\u003astart","kind":"value","result":{"input":"go","saga":true}}`),
		"escaped kind": []byte(`{"name":"run:start","kind":"valu\u0065","result":{"input":"go"}}`),
		"no input":     []byte(`{"name":"run:start","kind":"value","result":{"saga":true}}`),
	} {
		t.Run(name, func(t *testing.T) {
			m := NewMemStore()
			j, err := NewJournal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err := j.ensureHeader(ctx, "r"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := m.Insert(ctx, "r", runStartStep, b); err != nil {
				t.Fatal(err)
			}
			rec, err := decodeStored("r", runStartStep, b)
			if err != nil {
				t.Fatal(err)
			}
			var want RunStart
			if err := json.Unmarshal(rec.Result, &want); err != nil {
				t.Fatal(err)
			}
			got, ok, err := startUnderLease(ctx, j, "r")
			if err != nil || !ok || !reflect.DeepEqual(got, want) {
				t.Fatalf("startUnderLease = %#v, %v, %v; want %#v (the full decoding)", got, ok, err, want)
			}
		})
	}
}

// decodePlainStart reads only the records it is sure of, and reads them as the full decoding does:
// a plain text start this version writes (or one with no kind, as earlier versions wrote it) whose
// input needs no escape, with any other input or setting left to the JSON decoding.
func TestDecodePlainStartMatchesFullDecode(t *testing.T) {
	entry := func(result string) []byte {
		b, err := JournalEntry(runStartStep, Record{Kind: StepValue, Result: json.RawMessage(result)})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	start := func(s RunStart) []byte {
		r, err := marshalJournal(s)
		if err != nil {
			t.Fatal(err)
		}
		return entry(string(r))
	}
	full := func(b []byte) RunStart {
		rec, err := decodeStored("r", runStartStep, b)
		if err != nil {
			t.Fatal(err)
		}
		var st RunStart
		if err := json.Unmarshal(rec.Result, &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	taken := map[string][]byte{
		"plain":        start(RunStart{Input: UserText("go"), Kind: RunKindAgent}),
		"empty":        start(RunStart{Input: UserText(""), Kind: RunKindAgent}),
		"unicode":      start(RunStart{Input: UserText("caf\u00e9 \u4e16\u754c <b> & 'x'"), Kind: RunKindAgent}),
		"legacy":       entry(`{"input":"go"}`),
		"legacy space": entry(`{"input":"a b"}`),
	}
	for name, b := range taken {
		t.Run("taken/"+name, func(t *testing.T) {
			got, ok := decodePlainStart(b)
			if !ok {
				t.Fatalf("decodePlainStart left %s to the JSON decoding", b)
			}
			if want := full(b); !reflect.DeepEqual(got, want) {
				t.Fatalf("decodePlainStart = %#v, the full decoding = %#v", got, want)
			}
		})
	}
	left := map[string][]byte{
		"quote":        start(RunStart{Input: UserText(`a "b"`), Kind: RunKindAgent}),
		"backslash":    start(RunStart{Input: UserText(`a \ b`), Kind: RunKindAgent}),
		"control":      start(RunStart{Input: UserText("a\tb\n"), Kind: RunKindAgent}),
		"line sep":     start(RunStart{Input: UserText("a\u2028b"), Kind: RunKindAgent}),
		"saga":         start(RunStart{Input: UserText("go"), Kind: RunKindAgent, Saga: true}),
		"session":      start(RunStart{Input: UserText("go"), Kind: RunKindSessionTurn}),
		"image":        start(RunStart{Input: Message{Role: RoleUser, Parts: []Part{Text{Text: "go"}, Image{URL: "u"}}}, Kind: RunKindAgent}),
		"escaped":      entry(`{"input":"g\u006f"}`),
		"invalid utf8": []byte(plainStartPrefix + "\xff" + plainStartKind + `AAAA"}`),
		"raw control":  []byte(plainStartPrefix + "a\x01b" + plainStartKind + `AAAA"}`),
		"bad salt":     []byte(plainStartPrefix + "go" + plainStartKind + `AA AA"}`),
		"no salt":      []byte(plainStartPrefix + "go" + plainStartKind + `"}`),
		"trailing":     []byte(plainStartPrefix + "go" + plainStartKind + `AAAA"},`),
		"extra member": []byte(plainStartPrefix + "go" + plainStartKind + `AAAA","x":1}`),
	}
	for name, b := range left {
		t.Run("left/"+name, func(t *testing.T) {
			if st, ok := decodePlainStart(b); ok {
				t.Fatalf("decodePlainStart took %s as %#v", b, st)
			}
		})
	}
}

var errCrashed = errors.New("the process died")

// markerHookStore is a MemStore that acts as soon as it has recorded a new attempt marker: it
// cancels the driver's context, as a shutdown or a lost lease landing at that moment would, and
// when crash is set it also fails every later write, as a process that died right there would.
type markerHookStore struct {
	*MemStore
	cancel func()
	crash  bool
	dead   atomic.Bool
}

func (s *markerHookStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if s.dead.Load() {
		return Entry{}, false, errCrashed
	}
	e, inserted, err := s.MemStore.Insert(ctx, runID, name, data)
	if rec, derr := DecodeRecord(data); err == nil && inserted && derr == nil && rec.Kind == StepAttempt { // a marker this call wrote, not one it read
		s.cancel()
		if s.crash {
			s.dead.Store(true)
		}
	}
	return e, inserted, err
}

// A tool call whose run is cancelled after it won its attempt claim but before the tool was
// called provably did not start. The run records that, so a resume calls the tool instead of
// halting for a human over an effect that never fired.
func TestTool_CancelledBeforeItStartsIsReattempted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	var calls atomic.Int32
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "charged", nil
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	_, err := mustNew(m, j, WithTools(charge)).Run(ctx, "r1", UserText("pay"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run err = %v, want context.Canceled", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("the tool was called %d times after its run was cancelled, want 0", n)
	}

	resume := &greedyModel{script: [][]Emit{textTurn("done")}}
	res, err := mustNew(resume, mustJournal(store.MemStore), WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	if err != nil {
		t.Fatalf("resume err = %v, want the run to call the tool that never started", err)
	}
	out := res.Message
	if out.Text() != "done" || calls.Load() != 1 {
		t.Fatalf("resume = %q with %d charges, want \"done\" and exactly 1", out.Text(), calls.Load())
	}
}

// A process that dies in the same window leaves no record that the call did not start, so the
// resume halts: the claim alone cannot say whether the effect fired.
func TestTool_CrashBeforeItStartsStillHalts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel, crash: true}
	j := mustJournal(store)
	var calls atomic.Int32
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "charged", nil
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	if _, err := mustNew(m, j, WithTools(charge)).Run(ctx, "r1", UserText("pay")); err == nil {
		t.Fatal("the run whose process died succeeded")
	}
	resume := &greedyModel{script: [][]Emit{textTurn("done")}}
	_, err := mustNew(resume, mustJournal(store.MemStore), WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "c1" {
		t.Fatalf("resume err = %v after %d charges, want *OutcomeUnknown for c1", err, calls.Load())
	}
}

// Step keeps the same rule: cancelled after its claim and before fn, it records that fn did not
// start, and the next attempt runs fn.
func TestStep_CancelledBeforeItStartsIsReattempted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	var entered, ran atomic.Int32
	reserve := reserveFn(&entered, &ran)
	if _, err := j.Step(ctx, "r1", "reserve", reserve); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled step err = %v, want context.Canceled", err)
	}
	if n := entered.Load(); n != 0 {
		t.Fatalf("fn was called %d times after the step was cancelled, want 0", n)
	}
	got, err := mustJournal(store.MemStore).Step(context.Background(), "r1", "reserve", reserve)
	if err != nil || got != "reserved" || ran.Load() != 1 {
		t.Fatalf("second attempt = (%q, %v) with fn run %d times, want (\"reserved\", nil) and exactly 1", got, err, ran.Load())
	}
}

// A retry-safe Step that finds an earlier attempt marker (the step was a side effect then) halts
// on it, unless that attempt is recorded as not started: then there is nothing to halt for.
func TestStep_RetrySafeAfterNotStartedRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	var entered, ran atomic.Int32
	reserve := reserveFn(&entered, &ran)
	_, _ = j.Step(ctx, "r1", "reserve", reserve)
	got, err := mustJournal(store.MemStore).Step(context.Background(), "r1", "reserve", reserve, WithSafety(Safety{Idempotent: true}))
	if err != nil || got != "reserved" || ran.Load() != 1 {
		t.Fatalf("retry-safe step after an attempt that never started = (%q, %v), fn run %d times; want (\"reserved\", nil) and exactly 1", got, err, ran.Load())
	}
}

// Drivers that re-attempt a step recorded as not started claim the new attempt exclusively: fn
// runs once however many of them race.
func TestStep_ReattemptIsClaimedOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	var entered, ran atomic.Int32
	reserve := reserveFn(&entered, &ran)
	_, _ = j.Step(ctx, "r1", "reserve", reserve)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := mustJournal(store.MemStore).Step(context.Background(), "r1", "reserve", reserve)
			var halt *OutcomeUnknown
			if err != nil && !errors.As(err, &halt) {
				t.Errorf("a racing re-attempt failed with %v, want the result or a OutcomeUnknown", err)
			}
		}()
	}
	wg.Wait()
	if n := ran.Load(); n != 1 {
		t.Fatalf("fn ran %d times across racing re-attempts, want exactly 1", n)
	}
	if got, err := mustJournal(store.MemStore).Step(context.Background(), "r1", "reserve", reserve); err != nil || got != "reserved" {
		t.Fatalf("the step after the race = (%q, %v), want its recorded result", got, err)
	}
}

// reserveFn is a step's side effect that, like a well-behaved client, does nothing once its
// context is cancelled. entered counts every call, ran every reservation made.
func reserveFn(entered, ran *atomic.Int32) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		entered.Add(1)
		if err := ctx.Err(); err != nil {
			return "", err
		}
		ran.Add(1)
		return "reserved", nil
	}
}

// Only the driver that claimed an attempt can record that it did not start: a not-started record
// that carries another claim, or is not a StepNotStarted record, does not void the marker.
func TestNotStarted_OnlyTheClaimantVoidsAnAttempt(t *testing.T) {
	for name, forged := range map[string]Record{
		"another claim": {Kind: StepNotStarted, ToolUseID: "reserve", claim: "someone-else"},
		"another kind":  {Kind: StepValue, ToolUseID: "reserve"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := memJournal()
			won, marker, key, err := claimNextAttempt(ctx, store, "r1", stepAttemptStep("reserve"),
				Record{Kind: StepAttempt, ToolUseID: "reserve", AttemptedAt: time.Now().UnixMilli()})
			if err != nil || !won {
				t.Fatalf("setup claim = (%v, %v)", won, err)
			}
			rec := forged
			if name == "another kind" {
				rec.claim = marker.claim
			}
			if _, err := store.do(ctx, "r1", notStartedStep(key, marker.claim), func(context.Context) (Record, error) { return rec, nil }); err != nil {
				t.Fatal(err)
			}
			var entered, ran atomic.Int32
			_, err = store.Step(ctx, "r1", "reserve", reserveFn(&entered, &ran))
			var halt *OutcomeUnknown
			if !errors.As(err, &halt) || entered.Load() != 0 {
				t.Fatalf("Step = %v with fn called %d times, want a OutcomeUnknown: the attempt was never recorded as not started by its claimant", err, entered.Load())
			}
		})
	}
}

// ctxStore is a MemStore whose every method fails once its context is cancelled, before reading
// anything, as a SQL store's does, and that cancels the driver's context once an attempt marker is
// recorded.
type ctxStore struct {
	*MemStore
	cancel func()
}

func (s *ctxStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	e, inserted, err := s.MemStore.Insert(ctx, runID, name, data)
	if rec, derr := DecodeRecord(e.Data); err == nil && derr == nil && rec.Kind == StepAttempt {
		s.cancel()
	}
	return e, inserted, err
}

func (s *ctxStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	return s.MemStore.Get(ctx, runID, name)
}

func (s *ctxStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if err := ctx.Err(); err != nil {
		return func(yield func(Entry, error) bool) { yield(Entry{}, err) }
	}
	return s.MemStore.Load(ctx, runID, after)
}

// On a store that refuses a cancelled context, the step's own write fails before fn is called,
// and the not-started record is still written: it does not depend on the cancelled context.
func TestStep_NotStartedIsRecordedOnAStoreThatChecksContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := mustJournal(&ctxStore{MemStore: NewMemStore(), cancel: cancel})
	var entered, ran atomic.Int32
	reserve := reserveFn(&entered, &ran)
	if _, err := store.Step(ctx, "r1", "reserve", reserve); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled step err = %v, want context.Canceled", err)
	}
	got, err := store.Step(context.Background(), "r1", "reserve", reserve)
	if err != nil || got != "reserved" || ran.Load() != 1 {
		t.Fatalf("second attempt = (%q, %v) with fn run %d times, want (\"reserved\", nil) and exactly 1", got, err, ran.Load())
	}
}

// A saga's rollback compensates the calls that may have taken effect. A call cancelled by a
// sibling's failure after its claim and before it was called changed nothing, so the rollback
// neither halts on it nor compensates it.
func TestSaga_NotStartedCallIsNotCompensated(t *testing.T) {
	var charged, refunded atomic.Int32
	pay := MustCompensatedFunc("pay", "charge the card", func(context.Context, struct{}) (string, error) { charged.Add(1); return "paid", nil },
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil })
	book := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		return "", errors.New("no seats")
	})
	store := mustJournal(&holdClaimStore{MemStore: NewMemStore(), toolUseID: "p1"})
	m := &sagaTurns{turns: [][][3]string{{{"p1", "pay", `{}`}, {"b1", "book", `{}`}}}}
	_, err := mustNew(m, store, WithTools(pay, book)).Run(context.Background(), "r1", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	if aborted.CompensateErr != nil || charged.Load() != 0 || refunded.Load() != 0 {
		t.Fatalf("SagaAborted = %+v, charged %d, refunded %d; want a clean rollback of a charge that never started", aborted, charged.Load(), refunded.Load())
	}
}

// holdClaimStore is a MemStore that, once the attempt marker of the call toolUseID is recorded,
// holds that call until its context (the tool group's) is cancelled, so a sibling's failure lands
// after the claim and before the call.
type holdClaimStore struct {
	*MemStore
	toolUseID string
}

func (s *holdClaimStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	e, inserted, err := s.MemStore.Insert(ctx, runID, name, data)
	if rec, derr := DecodeRecord(e.Data); err == nil && derr == nil && rec.Kind == StepAttempt && rec.ToolUseID == s.toolUseID {
		<-ctx.Done()
	}
	return e, inserted, err
}

// ResolveHalt's minimum age runs from the attempt that may have fired the effect, not from an
// earlier attempt recorded as never started.
func TestResolveHalt_AgeRunsFromTheLiveAttempt(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	now := time.Now()
	base := toolAttemptStep("c1")
	won, first, key, err := claimNextAttempt(ctx, store, "r1", base,
		Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: now.Add(-2 * time.Hour).UnixMilli()})
	if err != nil || !won {
		t.Fatalf("first claim = (%v, %v)", won, err)
	}
	if err := recordNotStarted(ctx, store, "r1", key, first); err != nil {
		t.Fatal(err)
	}
	if won, _, _, err := claimNextAttempt(ctx, store, "r1", base,
		Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: now.UnixMilli()}); err != nil || !won {
		t.Fatalf("re-attempt claim = (%v, %v)", won, err)
	}
	err = ResolveHalt(ctx, store, HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "charged", IsError: false}, WithMinHaltAge(time.Hour), WithClock(func() time.Time { return now }))
	var young *HaltTooYoung
	if !errors.As(err, &young) {
		t.Fatalf("ResolveHalt = %v, want *HaltTooYoung: the live attempt is moments old", err)
	}
}

// A re-attempt's marker halts a resume like a first attempt's: a process that dies after claiming
// the re-attempt leaves its outcome unknown.
func TestTool_CrashInAReattemptHalts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	var calls atomic.Int32
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "charged", nil
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	_, _ = mustNew(m, j, WithTools(charge)).Run(ctx, "r1", UserText("pay")) // cancelled before the call: recorded as not started

	ctx2, cancel2 := context.WithCancel(context.Background())
	crashing := mustJournal(&markerHookStore{MemStore: store.MemStore, cancel: cancel2, crash: true})
	resume := &greedyModel{script: [][]Emit{textTurn("done")}}
	if _, err := mustNew(resume, crashing, WithTools(charge)).Run(ctx2, "r1", UserText("pay")); err == nil {
		t.Fatal("the re-attempt whose process died succeeded")
	}
	_, err := mustNew(
		&greedyModel{script: [][]Emit{textTurn("done")}},
		mustJournal(store.MemStore),
		WithTools(charge),
	).Run(context.Background(), "r1", UserText("pay"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "c1" || calls.Load() != 0 {
		t.Fatalf("resume after a crash in the re-attempt = %v with %d charges, want *OutcomeUnknown for c1 and no charge", err, calls.Load())
	}
}

// A call recorded as not started emits neither ToolStarted nor ToolCompleted: a stream consumer
// sees ToolStarted only for a call that actually starts, and each is paired with its
// ToolCompleted once the call is re-attempted.
func TestStream_NotStartedCallEmitsNoToolStarted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		return "charged", nil
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	evs, _, err := collect(mustNew(m, j, WithTools(charge)).Stream(ctx, "r1", UserText("pay")))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled stream err = %v, want context.Canceled", err)
	}
	for _, e := range evs {
		switch e.(type) {
		case ToolStarted, ToolCompleted:
			t.Fatalf("a call that never started emitted %T (events %v)", e, kinds(evs))
		}
	}

	evs, _, err = collect(mustNew(
		&greedyModel{script: [][]Emit{textTurn("done")}},
		mustJournal(store.MemStore),
		WithTools(charge),
	).Stream(context.Background(), "r1", UserText("pay")))
	if err != nil {
		t.Fatalf("resumed stream err = %v", err)
	}
	var started, completed int
	for _, e := range evs {
		switch e.(type) {
		case ToolStarted:
			started++
		case ToolCompleted:
			completed++
		}
	}
	if started != 1 || completed != 1 {
		t.Fatalf("the re-attempt emitted %d ToolStarted and %d ToolCompleted, want one of each (events %v)", started, completed, kinds(evs))
	}
}

// A saga call cancelled before it started records no accepted arguments (it accepted none); its
// re-attempt goes through tool middleware like any call, journals the arguments it accepted, and
// the rollback undoes those.
func TestSaga_NotStartedReattemptCompensatesTheAcceptedArguments(t *testing.T) {
	var charged, refunded atomic.Int32
	charge := MustCompensatedFunc("charge", "charge the card", func(_ context.Context, in chargeArgs) (string, error) {
		charged.Add(int32(in.Amount))
		return "ok", nil
	},
		func(_ context.Context, in chargeArgs, _ string) error { refunded.Add(int32(in.Amount)); return nil })
	fail := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		return "", errors.New("no seats")
	})
	model := func() Model {
		return NewScriptedModel(ToolTurn("c1", "charge", `{"amount":5}`), ToolTurn("b1", "book", `{}`), TextTurn("done"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	if _, err := mustNew(model(), j, WithTools(charge, fail), WithToolMiddleware(scaleCharge)).Run(ctx, "r", UserText("trip"), WithSaga()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled saga err = %v, want context.Canceled", err)
	}
	if charged.Load() != 0 {
		t.Fatalf("charged %d before the call started, want 0", charged.Load())
	}
	recs, _ := j.History(context.Background(), "r")
	for _, r := range recs {
		if r.Name == sagaArgsStep("c1") {
			t.Fatalf("a call that never started journaled accepted arguments: %+v", r)
		}
	}

	_, err := mustNew(
		model(),
		mustJournal(store.MemStore),
		WithTools(charge, fail),
		WithToolMiddleware(scaleCharge),
	).Run(context.Background(), "r", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil {
		t.Fatalf("resumed saga = %v, want a clean *SagaAborted", err)
	}
	if charged.Load() != 500 || refunded.Load() != 500 {
		t.Fatalf("charged %d, refunded %d; want the re-attempt's 500 charged and refunded", charged.Load(), refunded.Load())
	}
}
