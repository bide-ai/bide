package agent_test

// Deterministic reproductions of the claim-protocol review's findings against an earlier design
// (a claim-held pin on a reused claim id), kept as regressions: each test states the liveness or
// safety property it expects of the current design, where every claim takes a fresh id.

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

var errR3 = errors.New("connection lost")

// r3Store: faults[i] is a queue of per-prefix one-shot faults, "nc" (fail, not committed) or "c"
// (commit, then fail); after[prefix] runs once after an Insert of that prefix succeeds.
type r3Store struct {
	m      *agent.MemStore
	mu     sync.Mutex
	faults []r3Fault
	after  map[string]func()
	log    []string
}

type r3Fault struct{ prefix, mode string }

func (s *r3Store) take(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, f := range s.faults {
		if strings.HasPrefix(name, f.prefix) {
			s.faults = append(s.faults[:i:i], s.faults[i+1:]...)
			s.log = append(s.log, f.mode+" "+name)
			return f.mode
		}
	}
	return ""
}

func (s *r3Store) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	mode := s.take(name)
	if mode == "nc" {
		return agent.Entry{}, false, errR3
	}
	e, ok, err := s.m.Insert(ctx, runID, name, data)
	if mode == "c" {
		return agent.Entry{}, false, errR3
	}
	if err == nil {
		for p, f := range s.after {
			if strings.HasPrefix(name, p) && f != nil {
				s.after[p] = nil
				f()
			}
		}
	}
	return e, ok, err
}
func (s *r3Store) Get(ctx context.Context, r, n string) (agent.Entry, bool, error) {
	return s.m.Get(ctx, r, n)
}
func (s *r3Store) Load(ctx context.Context, r string, a int64) iter.Seq2[agent.Entry, error] {
	return s.m.Load(ctx, r, a)
}

func r3Dump(t *testing.T, m *agent.MemStore, runID string) {
	for e := range m.Load(context.Background(), runID, -1) {
		var r struct {
			Kind  string `json:"kind"`
			Claim string `json:"claim"`
		}
		_ = json.Unmarshal(e.Data, &r)
		t.Logf("  journal: %-70s kind=%s claim=%.8s", e.Name, r.Kind, r.Claim)
	}
}

// FINDING 1 (Step). A claim taken back from pendingClaims is pinned with a claim-held record under
// its not-started key. If the winner is then cancelled before fn runs, journalStep records "not
// started" under the same key: the Insert finds the claim-held record, Journal.notStarted takes
// that as success, and the marker stays live for ever. The effect never ran, yet every later
// drive halts.
func TestClaimHeldThenCancelled_StepHaltsForever(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	fired := 0
	body := func(context.Context) (string, error) { fired++; return "ok", nil }
	s := &r3Store{m: m, faults: []r3Fault{
		{"attempt:step:pay", "nc"},     // drive 1: the claim's marker Insert fails, not committed
		{"attempt:not-started:", "nc"}, // drive 1: its not-started record fails too: id remembered
	}}
	j1, _ := agent.NewJournal(s)
	_, err1 := j1.Step(ctx, "r", "pay", body)
	t.Logf("drive 1: %v", err1)

	// Drive 2, same process: takes the id back, wins the marker, pins claim_held; its caller is
	// cancelled just then (a shutdown, a deadline), before fn is called.
	ctx2, cancel := context.WithCancel(ctx)
	s.after = map[string]func(){"attempt:not-started:": cancel}
	j2, _ := agent.NewJournal(s)
	_, err2 := j2.Step(ctx2, "r", "pay", body)
	t.Logf("drive 2: %v (no error about the not-started record: it 'succeeded')", err2)

	j3, _ := agent.NewJournal(&r3Store{m: m}) // a new process, no faults
	_, err3 := j3.Step(ctx, "r", "pay", body)
	t.Logf("drive 3: %v; fired %d", err3, fired)
	r3Dump(t, m, "r")
	var halt *agent.OutcomeUnknown
	if fired == 0 && errors.As(err3, &halt) {
		t.Fatalf("the effect provably never started (drive 2 recorded it as not started without error), but the run halts for ever")
	}
}

// FINDING 1 (tool call): the same through the agent loop; a cancelled run is the trigger.
func TestClaimHeldThenCancelled_ToolHaltsForever(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	fired := 0
	charge := agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { fired++; return "ok", nil })
	model := func() agent.Model {
		return agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.TextTurn("done"))
	}
	s := &r3Store{m: m, faults: []r3Fault{{"attempt:tool:c1", "nc"}, {"attempt:not-started:", "nc"}}}
	j1, _ := agent.NewJournal(s)
	_, err1 := agenttest.MustNew(model(), j1, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
	t.Logf("drive 1: %v", err1)

	ctx2, cancel := context.WithCancel(ctx)
	s.after = map[string]func(){"attempt:not-started:": cancel}
	j2, _ := agent.NewJournal(s)
	_, err2 := agenttest.MustNew(model(), j2, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx2, "r", agent.UserText("hi"))
	t.Logf("drive 2: %v", err2)

	j3, _ := agent.NewJournal(&r3Store{m: m})
	_, err3 := agenttest.MustNew(model(), j3, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
	t.Logf("drive 3: %v; fired %d", err3, fired)
	r3Dump(t, m, "r")
	var halt *agent.OutcomeUnknown
	if fired == 0 && errors.As(err3, &halt) {
		t.Fatalf("the call provably never started, but the run halts for ever")
	}
}

// FINDING 1, second trigger (no cancellation): a claim-held Insert that commits and errors
// re-remembers the id; the next claim's marker Insert errors, and claim's own error path records
// "not started", which lands on the claim-held record, is taken as success, and drops the id.
func TestClaimHeldThenClaimError_StepHaltsForever(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	fired := 0
	body := func(context.Context) (string, error) { fired++; return "ok", nil }
	s := &r3Store{m: m, faults: []r3Fault{
		{"attempt:step:pay", "nc"}, {"attempt:not-started:", "nc"}, // drive 1: id remembered
	}}
	j, _ := agent.NewJournal(s)
	_, err1 := j.Step(ctx, "r", "pay", body)
	s.faults = []r3Fault{{"attempt:not-started:", "c"}} // drive 2: claim_held commits, errors: re-remembered
	_, err2 := j.Step(ctx, "r", "pay", body)
	s.faults = []r3Fault{{"attempt:step:pay", "c"}} // drive 3: the marker Insert errors
	_, err3 := j.Step(ctx, "r", "pay", body)
	j4, _ := agent.NewJournal(&r3Store{m: m})
	_, err4 := j4.Step(ctx, "r", "pay", body)
	t.Logf("drives: %v | %v | %v | %v; fired %d; faults %v", err1, err2, err3, err4, fired, s.log)
	r3Dump(t, m, "r")
	var halt *agent.OutcomeUnknown
	if fired == 0 && errors.As(err4, &halt) {
		t.Fatalf("the effect never started and every not-started write was acknowledged, but the run halts for ever")
	}
}

// FINDING 2 (tool call): the resume gate halts on a marker whose claim id this process holds in
// pendingClaims, so taking the id back never happens for a tool call whose marker committed.
func TestToolGateIgnoresRememberedClaim(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	fired := 0
	charge := agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { fired++; return "ok", nil })
	model := func() agent.Model {
		return agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.TextTurn("done"))
	}
	s := &r3Store{m: m, faults: []r3Fault{{"attempt:tool:c1", "c"}, {"attempt:not-started:", "nc"}}}
	j, _ := agent.NewJournal(s)
	_, err1 := agenttest.MustNew(model(), j, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
	_, err2 := agenttest.MustNew(model(), j, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi")) // same process
	t.Logf("drive 1: %v\ndrive 2: %v; fired %d", err1, err2, fired)
	var halt *agent.OutcomeUnknown
	if fired == 0 && errors.As(err2, &halt) {
		t.Fatalf("same process remembers the claim of the committed marker, yet the tool call halts")
	}
}

// Control for FINDING 2: the same through Step does take the id back and runs once.
func TestStepTakesRememberedClaim(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	fired := 0
	body := func(context.Context) (string, error) { fired++; return "ok", nil }
	s := &r3Store{m: m, faults: []r3Fault{{"attempt:step:pay", "c"}, {"attempt:not-started:", "nc"}}}
	j, _ := agent.NewJournal(s)
	_, err1 := j.Step(ctx, "r", "pay", body)
	v, err2 := j.Step(ctx, "r", "pay", body)
	t.Logf("drive 1: %v; drive 2: %q %v; fired %d", err1, v, err2, fired)
	if fired != 1 || err2 != nil {
		t.Fatalf("want one fire")
	}
}

// FINDING 3 (Step and tool): a winner cancelled before fn whose not-started write fails does not
// remember its claim id, unlike claim's own error path, so the same process halts on it.
func TestPostClaimNotStartedFailureNotRemembered(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	fired := 0
	body := func(context.Context) (string, error) { fired++; return "ok", nil }
	ctx1, cancel := context.WithCancel(ctx)
	s := &r3Store{m: m, faults: []r3Fault{{"attempt:not-started:", "nc"}},
		after: map[string]func(){"attempt:step:pay": cancel}}
	j, _ := agent.NewJournal(s)
	_, err1 := j.Step(ctx1, "r", "pay", body)
	_, err2 := j.Step(ctx, "r", "pay", body) // same process
	t.Logf("drive 1: %v\ndrive 2: %v; fired %d", err1, err2, fired)
	var halt *agent.OutcomeUnknown
	if fired == 0 && errors.As(err2, &halt) {
		t.Fatalf("the process knows its claim never started, yet halts on it")
	}
}

// A process that remembers its own claim of a call must not record another driver's attempt as
// not started. Process A's claim of the call fails outright (its marker and its not-started record
// are both lost), so A remembers its id. Process B then claims the call, runs it, and loses its
// result. When A resumes, the live marker is B's, not A's: A halts rather than void B's attempt
// and run the call a second time.
func TestRememberedClaimDoesNotVoidAnotherDriversAttempt(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	fired := 0
	charge := agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { fired++; return "ok", nil })
	model := func() agent.Model {
		return agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.TextTurn("done"))
	}
	a := &r3Store{m: m, faults: []r3Fault{{"attempt:tool:c1", "nc"}, {"attempt:not-started:", "nc"}}}
	ja, _ := agent.NewJournal(a)
	_, errA1 := agenttest.MustNew(model(), ja, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))

	b := &r3Store{m: m, faults: []r3Fault{{"tool:c1", "nc"}}} // B fires, then loses its result
	jb, _ := agent.NewJournal(b)
	_, errB := agenttest.MustNew(model(), jb, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))

	_, errA2 := agenttest.MustNew(model(), ja, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
	t.Logf("A: %v\nB: %v (fired %d)\nA again: %v", errA1, errB, fired, errA2)
	var halt *agent.OutcomeUnknown
	if fired != 1 || !errors.As(errA2, &halt) {
		t.Fatalf("A's resume = %v with the call fired %d times; want a halt and once", errA2, fired)
	}
}
