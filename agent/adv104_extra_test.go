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

func (*blockOnce) Name() string                { return "wait" }
func (*blockOnce) Description() string         { return "" }
func (*blockOnce) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (*blockOnce) Safety() Safety              { return Safety{ReadOnly: true} }
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
		_, err := New(mB, journal(), tool).Use(answerAndLeave(bgB)).RunResult(ctx, "r", "go")
		doneB <- err
	}()
	<-tool.blocked
	// Driver A: resumes the run while B is blocked, runs the tool (retry-safe), answers turn 1
	// with a request of its own in flight, and completes.
	mA := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := New(mA, jA, tool).Use(answerAndLeave(bgA)).RunResult(ctx, "r", "go"); err != nil {
		t.Fatal(err)
	}
	close(bgB.gate)
	close(tool.release)
	if err := <-doneB; err != nil {
		t.Fatal(err)
	}
	res, err := New(&scriptModel{}, store, tool).RunResult(ctx, "r", "go")
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
	src := NewMemStore()
	if _, err := src.Do(ctx, "r", lateSpendStep(0), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, DiscardedUsage: &late}, nil
	}); err != nil {
		t.Fatal(err)
	}
	msg := Message{Role: RoleAssistant, Parts: []Part{Text{Text: "done"}}}
	u := billed
	if _, err := src.Do(ctx, "r", modelStep(0), func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &msg, Usage: &u, Finish: FinishStop}, nil
	}); err != nil {
		t.Fatal(err)
	}
	rm, err := Replay(ctx, src, "r")
	if err != nil {
		t.Fatal(err)
	}
	res, err := New(rm, NewMemStore()).RunResult(ctx, "r", "go")
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
	src := NewMemStore()
	m := describedScript{&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}}
	if _, err := New(m, src).Run(ctx, "r", "go"); err != nil {
		t.Fatal(err)
	}
	rm, err := Replay(ctx, src, "r")
	if err != nil {
		t.Fatal(err)
	}
	dst := NewMemStore()
	if _, err := New(rewrap{rm}, dst).Run(ctx, "r", "go"); err != nil {
		t.Fatal(err)
	}
	a, b := modelRecords(t, src, "r"), modelRecords(t, dst, "r")
	if a[0].Model == nil || b[0].Model == nil || *a[0].Model != *b[0].Model {
		t.Fatalf("replayed Model = %v, original %v", b[0].Model, a[0].Model)
	}
}
