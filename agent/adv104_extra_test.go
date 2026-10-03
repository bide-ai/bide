package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// blockOnce is a read-only tool whose first call signals blocked and waits for release; later
// calls return at once.
type blockOnce struct {
	once             sync.Once
	blocked, release chan struct{}
}

// Spec describes the tool to the agent (see Tool).
func (t *blockOnce) Spec() ToolSpec {
	return ToolSpec{Name: "wait", Description: "", Input: json.RawMessage(`{"type":"object"}`), Safety: Safety{ReadOnly: true}}
}

func (t *blockOnce) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	first := false
	t.once.Do(func() { first = true })
	if first {
		close(t.blocked)
		<-t.release
	}
	return json.RawMessage(`"ok"`), nil
}

// procStore is one process's handle on a shared store: a value, so each Journal over it shares
// in-flight steps with no other, and without the store's Leaser, as two processes of a store
// without leases.
type procStore struct{ Store }

// (a) Two drivers of one run each journal the late spend of their own requests: the one that
// started first must not lose its late record to the other's under one key.
func TestAdv104_TwoDriversLateSpend(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	journal := func() *Journal {
		j, err := NewJournal(procStore{mem})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	store := journal()
	tool := &blockOnce{blocked: make(chan struct{}), release: make(chan struct{})}
	lateB := Usage{InputTokens: 11}
	lateA := Usage{InputTokens: 13}
	bgB := &lateModel{u: lateB, started: make(chan struct{}), gate: make(chan struct{})}
	bgA := &lateModel{u: lateA, started: make(chan struct{}), gate: make(chan struct{})}
	// A's request ends once A's turn is recorded: its spend is late, journaled at A's end.
	fsA := &faultStore{Store: mem, commitThenErr: map[string]bool{}, failNoCommit: map[string]bool{}, failGet: map[string]bool{}, armGetOnFault: map[string]bool{}}
	fsA.afterInsert = func(name string) {
		if name == modelStep(1) {
			close(bgA.gate)
		}
	}
	jA, err := NewJournal(procStore{fsA})
	if err != nil {
		t.Fatal(err)
	}

	// Driver B: turn 0 calls the blocking tool, with a request of its own still in flight.
	mB := &scriptModel{turns: [][]Emit{toolTurnWithUsage("c1", "wait", `{}`, billed)}}
	doneB := make(chan error, 1)
	go func() {
		_, err := mustNew(mB, journal(), WithTools(tool), WithMiddleware(answerAndLeave(bgB))).Run(ctx, "r", UserText("go"))
		doneB <- err
	}()
	<-tool.blocked
	// Driver A: resumes the run while B is blocked, runs the tool (retry-safe), answers turn 1
	// with a request of its own in flight, and completes.
	mA := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(mA, jA, WithTools(tool), WithMiddleware(answerAndLeave(bgA))).Run(ctx, "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	close(bgB.gate)
	close(tool.release)
	if err := <-doneB; err != nil {
		t.Fatal(err)
	}
	res, err := mustNew(&scriptModel{}, store, WithTools(tool)).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("journal: %v", names(t, store, "r"))
	want := billed
	addUsage(&want, billed)
	addUsage(&want, lateA)
	addUsage(&want, lateB)
	if res.Spend != want {
		t.Fatalf("Spend = %+v, want %+v; journal %v", res.Spend, want, names(t, store, "r"))
	}
}

// (b) A late spend record that precedes every model record still reaches the replayed run's
// spend: Replay carries it to the first turn.
func TestAdv104_ReplayLateRecordBeforeAnyTurn(t *testing.T) {
	ctx := context.Background()
	src := memJournal()
	if _, err := src.do(ctx, "r", lateSpendStep("x"), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, DiscardedUsage: &late}, nil
	}); err != nil {
		t.Fatal(err)
	}
	msg := Message{Role: RoleAssistant, Parts: []Part{Text{Text: "done"}}}
	u := billed
	if _, err := src.do(ctx, "r", modelStep(0), func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &msg, Usage: &u, ModelTurn: &ModelTurn{Finish: FinishStop}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	rm, err := Replay(ctx, src, "r")
	if err != nil {
		t.Fatal(err)
	}
	res, err := mustNew(rm, memJournal()).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, late)
	if res.Spend != want {
		t.Fatalf("replayed Spend = %+v, want %+v", res.Spend, want)
	}
}

// rewrap is a Model decorator that forwards its inner model's events through a stream of its
// own, as a logging or metering wrapper does.
type rewrap struct{ inner Model }

func (w rewrap) Unwrap() Model { return w.inner }
func (w rewrap) Stream(ctx context.Context, req Request) (*Stream, error) {
	s, err := w.inner.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	return NewStreamFunc(ctx, func(send func(Emit) bool) {
		for ev, err := range s.Events() {
			if !send(Emit{Event: ev, Err: err}) {
				return
			}
		}
	}), nil
}

// (c) A decorator that re-wraps the replay stream keeps the replayed turn's recorded model.
func TestAdv104_ReplayThroughADecoratorKeepsModel(t *testing.T) {
	ctx := context.Background()
	src := memJournal()
	m := describedScript{&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}}
	if _, err := mustNew(m, src).Run(ctx, "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	rm, err := Replay(ctx, src, "r")
	if err != nil {
		t.Fatal(err)
	}
	dst := memJournal()
	if _, err := mustNew(rewrap{rm}, dst).Run(ctx, "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	a, b := modelRecords(t, src, "r"), modelRecords(t, dst, "r")
	if a[0].Model() == nil || b[0].Model() == nil || *a[0].Model() != *b[0].Model() {
		t.Fatalf("replayed Model = %v, original %v", b[0].Model(), a[0].Model())
	}
}

// When a model record's write reports an error and the read that should settle its fate fails
// too, the turn's answer functions wait for the run's next drive in the process: the record
// landed, so they run then, once.
func TestAdv104_LookupErrorAnswerRunsOnNextDrive(t *testing.T) {
	ctx := context.Background()
	st := newFaultStore()
	st.commitThenErr[modelStep(0)] = true
	st.armGetOnFault[modelStep(0)] = true
	j, err := NewJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	var answers int
	key := new(int)
	count := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			call.OnAnswer(key, func(context.Context, ModelResponse) { answers++ })
			return next(ctx, call)
		}
	}
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(m, j, WithMiddleware(count)).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want the write failure")
	}
	if answers != 0 {
		t.Fatalf("answers ran %d times before the record's fate was known", answers)
	}
	st.mu.Lock()
	st.failGet = map[string]bool{}
	st.mu.Unlock()
	if _, err := mustNew(&scriptModel{}, j).Run(ctx, "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	if answers != 1 {
		t.Fatalf("answers ran %d times, want once for the one recorded turn", answers)
	}
}

// gatedTurn answers after every gatedTurn sharing its gate has been called, so two drivers' calls
// of one turn are both made before either is recorded.
type gatedTurn struct {
	g    *twoGate
	text string
	u    Usage
}

type twoGate struct {
	mu   sync.Mutex
	n    int
	open chan struct{}
}

func (m *gatedTurn) Stream(context.Context, Request) (*Stream, error) {
	m.g.mu.Lock()
	if m.g.n++; m.g.n == 2 {
		close(m.g.open)
	}
	m.g.mu.Unlock()
	<-m.g.open
	ch := make(chan Emit, 2)
	ch <- Emit{Event: TextDelta{Text: m.text}}
	ch <- Emit{Event: Finish{Reason: FinishStop, Usage: m.u}}
	close(ch)
	return NewStream(ch), nil
}

// Two drivers of one run both answer its turn; one records it. The other's request was billed all
// the same: its spend is journaled as late spend, so the run's Spend counts both requests, and only
// the recorded turn's driver runs its answer functions.
func TestAdv104_LostRaceSpendIsLate(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	g := &twoGate{open: make(chan struct{})}
	uA, uB := Usage{InputTokens: 5}, Usage{InputTokens: 9}
	var wg sync.WaitGroup
	for _, m := range []*gatedTurn{{g: g, text: "a", u: uA}, {g: g, text: "b", u: uB}} {
		j, err := NewJournal(procStore{mem})
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mustNew(m, j).Run(ctx, "r", UserText("go")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	j, _ := NewJournal(procStore{mem})
	res, err := mustNew(&scriptModel{}, j).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := uA
	addUsage(&want, uB)
	if res.Spend != want {
		t.Fatalf("Spend = %+v, want both requests' %+v; journal %v", res.Spend, want, names(t, j, "r"))
	}
}

// A late spend record whose write fails without landing is kept by the process and written by the
// run's next drive in it, so the run's Spend still counts the request, once.
func TestAdv104_FailedLateSpendWriteIsWrittenNextDrive(t *testing.T) {
	ctx := context.Background()
	bg := &lateModel{u: late, started: make(chan struct{}), gate: make(chan struct{})}
	st := newFaultStore()
	st.failNoCommitPrefix = lateSpendPrefix
	st.afterInsert = func(name string) {
		if name == modelStep(0) {
			close(bg.gate)
		}
	}
	j, err := NewJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(m, j, WithMiddleware(answerAndLeave(bg))).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want the late spend write's error")
	}
	res, err := mustNew(&scriptModel{}, j).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, late)
	if res.Spend != want {
		t.Fatalf("Spend = %+v, want %+v; journal %v", res.Spend, want, names(t, j, "r"))
	}
}
