package agent_test

import (
	"context"
	"errors"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// hookStore forwards to a MemStore and calls hooks after each Insert and before each Get, with the
// per-name call count.
type hookStore struct {
	m        *agent.MemStore
	mu       sync.Mutex
	ins, get map[string]int
	afterIns func(name string, n int)
	beforeGt func(name string, n int)
}

func newHookStore() *hookStore {
	return &hookStore{m: agent.NewMemStore(), ins: map[string]int{}, get: map[string]int{}}
}

func (h *hookStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	e, ok, err := h.m.Insert(ctx, runID, name, data)
	h.mu.Lock()
	h.ins[name]++
	n, f := h.ins[name], h.afterIns
	h.mu.Unlock()
	if f != nil {
		f(name, n)
	}
	return e, ok, err
}

func (h *hookStore) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	h.mu.Lock()
	h.get[name]++
	n, f := h.get[name], h.beforeGt
	h.mu.Unlock()
	if f != nil {
		f(name, n)
	}
	return h.m.Get(ctx, runID, name)
}

func (h *hookStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return h.m.Load(ctx, runID, after)
}

// Two drivers of one side-effect Step in one process, through one Journal. The driver that wins
// the claim must run the step (or hand its value to the other). Instead, when the loser's
// halt-probe Do starts the shared in-flight entry first, the winner joins it, gets the loser's
// ResumeHalt, never calls fn, and records its attempt as not started: both drivers halt and
// nobody runs the step.
func TestStep_ClaimWinnerRunsTheStepWhenALoserReadsFirst(t *testing.T) {
	ctx := context.Background()
	h := newHookStore()
	j, err := agent.NewJournal(h)
	if err != nil {
		t.Fatal(err)
	}
	// Warm the header so the counted calls below are the step's own.
	if _, err := agent.Step(ctx, j, "r", "warm", func(context.Context) (int, error) { return 0, nil }, agent.StepSafety(agent.Safety{ReadOnly: true})); err != nil {
		t.Fatal(err)
	}
	winnerClaimed := make(chan struct{})
	releaseWinner := make(chan struct{})
	loserInFlight := make(chan struct{})
	releaseLoser := make(chan struct{})
	h.afterIns = func(name string, n int) {
		if name == "attempt:step:s" && n == 1 { // the winner's claim has committed
			close(winnerClaimed)
			<-releaseWinner
		}
	}
	h.beforeGt = func(name string, n int) {
		if name == "s" && n == 3 { // the loser's Do (inside the shared flight) reads the step
			close(loserInFlight)
			<-releaseLoser
		}
	}
	var ran atomic.Int32
	fn := func(context.Context) (string, error) { ran.Add(1); return "charged", nil }
	var wg sync.WaitGroup
	var winnerErr, loserErr error
	wg.Add(2)
	go func() { defer wg.Done(); _, winnerErr = agent.Step(ctx, j, "r", "s", fn) }()
	<-winnerClaimed
	go func() { defer wg.Done(); _, loserErr = agent.Step(ctx, j, "r", "s", fn) }()
	<-loserInFlight
	close(releaseWinner) // the winner now enters doFresh and joins the loser's flight
	time.Sleep(100 * time.Millisecond)
	close(releaseLoser)
	wg.Wait()
	var halt *agent.ResumeHalt
	t.Logf("winner: %v; loser: %v; fn ran %d time(s)", winnerErr, loserErr, ran.Load())
	if ran.Load() != 1 {
		t.Errorf("fn ran %d times across the two drivers, want exactly 1 (the claim winner)", ran.Load())
	}
	if errors.As(winnerErr, &halt) {
		t.Errorf("the driver that won the claim got a ResumeHalt (an unknown outcome) for a step nobody started")
	}
}
