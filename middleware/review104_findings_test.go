package middleware_test

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// signal is a middleware that closes done[model] once next returns for a call to that model.
func signal(target agent.Model, done chan struct{}) agent.Middleware {
	return func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			r, err := next(ctx, call)
			if call.Model == target {
				close(done)
			}
			return r, err
		}
	}
}

// F1: Cost inside Hedge counts a losing target's response as an answer when the loser completes.
// Hedge's doc: "Cost and the run's token budget count each target's usage as spend, the
// winner's as the answer's."
func TestF1_CostInsideHedge_AnswerCountsLoser(t *testing.T) {
	gate := make(chan struct{})
	primary := &gateModel{name: "p", u: billed, gate: gate}
	backup := &gateModel{name: "b", u: billed}
	var meter middleware.CostMeter
	loserDone := make(chan struct{})
	resp, err := agent.CallModel(context.Background(), primary, agent.Request{Messages: []agent.Message{agent.UserText("q")}},
		middleware.Hedge(0, backup), signal(primary, loserDone), middleware.Cost(&meter, middleware.Rates{InputPer1M: 1e6}))
	if err != nil || resp.Message.Text() != "b-partial!" {
		t.Fatalf("resp=%q err=%v", resp.Message.Text(), err)
	}
	close(gate)
	<-loserDone
	s := meter.Snapshot()
	if s.Spend != (agent.Usage{InputTokens: 200, OutputTokens: 40}) {
		t.Fatalf("spend %+v", s.Spend)
	}
	if s.Answer != billed {
		t.Fatalf("Answer = %+v, want only the winner's %+v (the loser's response was never returned by the call)", s.Answer, billed)
	}
}

// F2: Replay does not reproduce the journaled Model of a turn: a replayed run's journal differs
// from the original's.
func TestF2_ReplayLosesModelInfo(t *testing.T) {
	ctx := context.Background()
	info := agent.ModelInfo{Provider: "acme", Model: "m-1"}
	orig := agent.NewMemStore()
	m := describedModel{&gateModel{name: "p", u: billed}, info}
	sys := agent.Func("noop", "d", agent.Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { return "", nil })
	if _, err := agent.New(m, orig, sys).Run(ctx, "r", "q"); err != nil {
		t.Fatal(err)
	}
	rep, err := agent.Replay(ctx, orig, "r")
	if err != nil {
		t.Fatal(err)
	}
	dst := agent.NewMemStore()
	if _, err := agent.New(rep, dst, sys).Run(ctx, "r", "q"); err != nil {
		t.Fatal(err)
	}
	a, _ := orig.History(ctx, "r")
	b, _ := dst.History(ctx, "r")
	pick := func(rs []agent.Record) agent.Record {
		for _, r := range rs {
			if r.Kind == agent.StepModel {
				return r
			}
		}
		return agent.Record{}
	}
	ra, rb := pick(a), pick(b)
	if ra.Finish != rb.Finish || ra.RawFinish != rb.RawFinish || ra.PromptDigest != rb.PromptDigest || ra.ToolsDigest != rb.ToolsDigest {
		t.Fatalf("finish/digest mismatch: %+v vs %+v", ra, rb)
	}
	if !reflect.DeepEqual(ra.Model, rb.Model) {
		t.Fatalf("replayed record Model = %v, original %+v", rb.Model, *ra.Model)
	}
}

// F3: a hedge loser that ends after the run's last turn is billed but never reaches Result.Spend
// or the journal. Here the loser honours the cancellation Hedge sends it, but reports its usage
// only after the winner's response has been returned: the run must wait for it before completing.
func TestF3_HedgeLoserSpendLostOnFinalTurn(t *testing.T) {
	gate := make(chan struct{})
	primary := &gateModel{name: "p", u: billed, gate: gate, honorCtx: true}
	backup := &gateModel{name: "b", u: billed}
	winnerDone := make(chan struct{})
	go func() { <-winnerDone; close(gate) }() // the loser ends once the winner has been returned
	a := agent.New(primary, agent.NewMemStore()).Use(middleware.Hedge(0, backup), signal(backup, winnerDone))
	res, err := a.RunResult(context.Background(), "r", "q")
	if err != nil {
		t.Fatal(err)
	}
	// re-enter the finished run: Spend is read from the journal
	res2, err := a.RunResult(context.Background(), "r", "q")
	if err != nil {
		t.Fatal(err)
	}
	want := agent.Usage{InputTokens: 200, OutputTokens: 40}
	if res.Spend != want || res2.Spend != want {
		t.Fatalf("Spend = %+v / re-entered %+v, want both requests %+v", res.Spend, res2.Spend, want)
	}
}

// failOnce fails the first Insert of the named step.
type failOnce struct {
	agent.Store
	name   string
	failed bool
}

func (s *failOnce) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if name == s.name && !s.failed {
		s.failed = true
		return agent.Entry{}, false, errors.New("disk full")
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// F4: a model turn whose record fails to write was billed, but no @spend record is written for it
// (the meter was already taken inside the step fn), so the resumed run's Spend misses it.
func TestF4_ModelRecordWriteFailureLosesSpend(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	fs := &failOnce{Store: mem, name: "@llm/0"}
	j, err := agent.NewJournal(fs)
	if err != nil {
		t.Fatal(err)
	}
	m := &gateModel{name: "p", u: billed}
	a := agent.New(m, j)
	if _, err := a.RunResult(ctx, "r", "q"); err == nil {
		t.Fatal("want storage error")
	}
	res, err := a.RunResult(ctx, "r", "q")
	if err != nil {
		t.Fatal(err)
	}
	if m.calls.Load() != 2 {
		t.Fatalf("calls %d", m.calls.Load())
	}
	want := agent.Usage{InputTokens: 200, OutputTokens: 40}
	if res.Spend != want {
		t.Fatalf("Spend = %+v after resume, want both billed requests %+v", res.Spend, want)
	}
}

// F5: a middleware that stores a ModelCall can send it after the run has returned: the request
// is sent and billed, but no run records it.
func TestF5_StoredCallSendsAfterRun(t *testing.T) {
	var stored agent.ModelCall
	var next0 agent.ModelHandler
	keep := func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			stored, next0 = call, next
			return next(ctx, call)
		}
	}
	m := &gateModel{name: "p", u: billed}
	if _, err := agent.New(m, agent.NewMemStore()).Use(keep).Run(context.Background(), "r", "q"); err != nil {
		t.Fatal(err)
	}
	_, err := next0(context.Background(), stored)
	if err == nil || m.calls.Load() != 1 {
		t.Fatalf("a call of a finished turn was sent after the run returned: err=%v, model calls=%d", err, m.calls.Load())
	}
}

// F6: attempt numbering, as documented on ModelCall.Attempt: a request is numbered when it reaches
// the model handler, before its Before hooks, so every hook sees its number; a request a Before
// hook refused keeps its number, and the next request sent is numbered after it.
func TestF6_AttemptGapOnBeforeRefusal(t *testing.T) {
	refused := false
	var sentAttempts []int
	refuse := func(next agent.ModelHandler) agent.ModelHandler {
		h := agent.ModelCallHook{Before: func(context.Context, agent.ModelCall) error {
			if !refused {
				refused = true
				return errors.New("not yet")
			}
			return nil
		}}
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			return next(ctx, call.AddHook(h))
		}
	}
	logm := func(next agent.ModelHandler) agent.ModelHandler {
		h := agent.ModelCallHook{Before: func(_ context.Context, c agent.ModelCall) error {
			sentAttempts = append(sentAttempts, c.Attempt())
			return nil
		}}
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			return next(ctx, call.AddHook(h))
		}
	}
	m := &gateModel{name: "p", u: billed}
	_, err := agent.CallModel(context.Background(), m, agent.Request{Messages: []agent.Message{agent.UserText("q")}},
		middleware.Retry(1, middleware.WithBackoff(0, 0)), refuse, logm)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sentAttempts, []int{2}) {
		t.Fatalf("the request sent after a refused one is numbered %v, want [2]", sentAttempts)
	}
}

// Stream consumer check: after the last TurnRestarted of a turn, the text the consumer assembled
// equals the AssistantTurn's message; no model event after AssistantTurn of that turn.
func streamCheck(t *testing.T, as *agent.AgentStream) (restarts int) {
	t.Helper()
	var text strings.Builder
	ended := false
	for ev := range as.Events() {
		switch e := ev.(type) {
		case agent.TurnRestarted:
			restarts++
			text.Reset()
		case agent.ModelEvent:
			if ended {
				t.Errorf("model event after AssistantTurn: %#v", e.Event)
			}
			if d, ok := e.Event.(agent.TextDelta); ok {
				text.WriteString(d.Text)
			}
		case agent.AssistantTurn:
			ended = true
			if text.String() != e.Message.Text() {
				t.Errorf("consumer assembled %q, journal %q", text.String(), e.Message.Text())
			}
		}
	}
	if _, err := as.Final(); err != nil {
		t.Fatal(err)
	}
	return restarts
}

func TestS1_HedgeStreamWinnerNotClaimer(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{})
	primary := &gateModel{name: "p", u: billed, gate: gate, honorCtx: true, started: started}
	backup := &gateModel{name: "b", u: billed}
	// Delay the backup until the primary has streamed.
	wait := func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			if call.Model == agent.Model(backup) {
				<-started
			}
			return next(ctx, call)
		}
	}
	loserDone := make(chan struct{})
	a := agent.New(primary, agent.NewMemStore()).Use(middleware.Hedge(0, backup), signal(primary, loserDone), wait)
	if n := streamCheck(t, a.Stream(context.Background(), "r", "q")); n > 1 {
		t.Fatalf("restarts %d", n)
	}
	close(gate)
	<-loserDone
}

// Goroutines: cancelling a streaming hedged run mid-stream leaves no goroutine behind once the
// models have honored the cancellation.
func TestG1_CancelMidStreamNoLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		gate := make(chan struct{})
		started := make(chan struct{})
		primary := &gateModel{name: "p", u: billed, gate: gate, honorCtx: true, started: started}
		backup := &gateModel{name: "b", u: billed, gate: gate, honorCtx: true}
		ctx, cancel := context.WithCancel(context.Background())
		a := agent.New(primary, agent.NewMemStore()).Use(middleware.Hedge(0, backup), middleware.Retry(2, middleware.WithBackoff(0, 0)))
		as := a.Stream(ctx, "r", "q")
		go func() { <-started; cancel() }()
		for range as.Events() {
		}
		_, _ = as.Final()
		close(gate)
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > base+2 {
		buf := make([]byte, 1<<16)
		t.Fatalf("goroutines %d > base %d\n%s", g, base, buf[:runtime.Stack(buf, true)])
	}
}
