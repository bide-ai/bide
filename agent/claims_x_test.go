package agent_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journalhook"
)

// Deterministic reproductions of the claim-protocol review's findings against an earlier design
// (a claim-held pin on a reused claim id), kept as regressions: each test states the liveness or
// safety property it expects of the current design, where every claim takes a fresh id.

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

var errLost = errors.New("connection lost")

// faultStore injects per-name faults over a MemStore: commitErr[name-prefix] commits the entry
// and then reports an error (once per prefix); failNoCommit[name-prefix] reports an error without
// committing (once per prefix).
type faultStore struct {
	m            *agent.MemStore
	mu           sync.Mutex
	commitErr    map[string]bool
	failNoCommit map[string]bool
	log          []string
}

func (s *faultStore) match(set map[string]bool, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, on := range set {
		if on && strings.HasPrefix(name, p) {
			set[p] = false
			return true
		}
	}
	return false
}

func (s *faultStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if s.match(s.failNoCommit, name) {
		s.log = append(s.log, "fail(no commit) "+name)
		return agent.Entry{}, false, errLost
	}
	e, ok, err := s.m.Insert(ctx, runID, name, data)
	if err == nil && ok && s.match(s.commitErr, name) {
		s.log = append(s.log, "commit+err "+name)
		return agent.Entry{}, false, errLost
	}
	return e, ok, err
}
func (s *faultStore) Get(ctx context.Context, r, n string) (agent.Entry, bool, error) {
	return s.m.Get(ctx, r, n)
}
func (s *faultStore) Load(ctx context.Context, r string, a int64) iter.Seq2[agent.Entry, error] {
	return s.m.Load(ctx, r, a)
}

// Drive 1: the claim's marker Insert commits and errors; the not-started record's Insert also
// commits and errors, so the claim id is remembered in pendingClaims.
// Drive 2 (same process): the loop sees the voided marker, claims the first attempt again, takes
// the remembered id, "wins" the marker that is already voided, and fires the effect. Its result
// write then fails (the crash window every marker exists to cover).
// Drive 3 (a new process): the marker is voided by the not-started record drive 1 wrote, so the
// effect is re-attempted and fires a second time.
func TestRememberedClaimRunsUnderAVoidedMarker_Tool(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	fired := 0
	charge := agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { fired++; return "ok", nil })
	model := func() agent.Model {
		return agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.TextTurn("done"))
	}
	s := &faultStore{m: m,
		commitErr:    map[string]bool{"attempt:tool:c1": true, "attempt:not-started:": true},
		failNoCommit: map[string]bool{},
	}
	j1, _ := agent.NewJournal(s)
	_, err1 := agenttest.MustNew(model(), j1, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
	t.Logf("drive 1: %v (fired %d)", err1, fired)

	s.failNoCommit["tool:c1"] = true // the result write of drive 2 is lost (or the process dies here)
	j2, _ := agent.NewJournal(s)     // same process: same store value
	_, err2 := agenttest.MustNew(model(), j2, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
	t.Logf("drive 2: %v (fired %d)", err2, fired)

	j3, _ := agent.NewJournal(&faultStore{m: m}) // a new process
	_, err3 := agenttest.MustNew(model(), j3, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
	t.Logf("drive 3: %v (fired %d)", err3, fired)
	t.Logf("faults: %v", s.log)
	if fired > 1 {
		t.Fatalf("side effect fired %d times", fired)
	}
}

// The same through Step.
func TestRememberedClaimRunsUnderAVoidedMarker_Step(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	fired := 0
	body := func(context.Context) (string, error) { fired++; return "ok", nil }
	s := &faultStore{m: m,
		commitErr:    map[string]bool{"attempt:step:pay": true, "attempt:not-started:": true},
		failNoCommit: map[string]bool{},
	}
	j1, _ := agent.NewJournal(s)
	_, err1 := j1.Step(ctx, "r", "pay", body)
	t.Logf("drive 1: %v (fired %d)", err1, fired)

	s.failNoCommit["pay"] = true
	j2, _ := agent.NewJournal(s)
	_, err2 := j2.Step(ctx, "r", "pay", body)
	t.Logf("drive 2: %v (fired %d)", err2, fired)

	j3, _ := agent.NewJournal(&faultStore{m: m})
	_, err3 := j3.Step(ctx, "r", "pay", body)
	t.Logf("drive 3: %v (fired %d)", err3, fired)
	if fired > 1 {
		t.Fatalf("side effect fired %d times", fired)
	}
}

// Control: the marker commits with an error and the not-started write fails without committing
// (equivalently, the process dies between them). A new process halts; nothing fires twice.
func TestClaimErrorThenNoNotStarted_NewProcessHalts(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	fired := 0
	body := func(context.Context) (string, error) { fired++; return "ok", nil }
	s := &faultStore{m: m,
		commitErr:    map[string]bool{"attempt:step:pay": true},
		failNoCommit: map[string]bool{"attempt:not-started:": true},
	}
	j1, _ := agent.NewJournal(s)
	_, err1 := j1.Step(ctx, "r", "pay", body)
	j2, _ := agent.NewJournal(&faultStore{m: m})
	_, err2 := j2.Step(ctx, "r", "pay", body)
	var halt *agent.OutcomeUnknown
	t.Logf("drive 1: %v; drive 2: %v; fired %d", err1, err2, fired)
	if fired != 0 || !errors.As(err2, &halt) {
		t.Fatalf("want a halt and nothing fired")
	}
}

// Two drivers each see their claim Insert error and each record not-started; only one marker
// exists (the one that committed). Re-drives fire the effect exactly once.
func TestTwoDriversBothRecordNotStarted(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	fired := 0
	body := func(context.Context) (string, error) { fired++; return "ok", nil }
	a := &faultStore{m: m, commitErr: map[string]bool{"attempt:step:pay": true}, failNoCommit: map[string]bool{}}
	b := &faultStore{m: m, commitErr: map[string]bool{}, failNoCommit: map[string]bool{"attempt:step:pay": true}}
	ja, _ := agent.NewJournal(a)
	jb, _ := agent.NewJournal(b)
	_, ea := ja.Step(ctx, "r", "pay", body)
	_, eb := jb.Step(ctx, "r", "pay", body)
	for i := 0; i < 3; i++ {
		jn, _ := agent.NewJournal(&faultStore{m: m})
		_, _ = jn.Step(ctx, "r", "pay", body)
	}
	t.Logf("a: %v; b: %v; fired %d", ea, eb, fired)
	if fired != 1 {
		t.Fatalf("fired %d, want 1", fired)
	}
}

// A claim whose marker and not-started writes both failed is recorded as not started by the next
// drive in the process, which re-attempts the effect under a fresh claim; its result write is then
// lost. A new process finds that live marker and halts: the effect fires once.
func TestReattemptWhoseResultIsLostHalts(t *testing.T) {
	ctx := context.Background()
	t.Run("tool", func(t *testing.T) {
		m := agent.NewMemStore()
		fired := 0
		charge := agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { fired++; return "ok", nil })
		model := func() agent.Model {
			return agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.TextTurn("done"))
		}
		s := &faultStore{m: m,
			commitErr:    map[string]bool{},
			failNoCommit: map[string]bool{"attempt:tool:c1": true, "attempt:not-started:": true},
		}
		j1, _ := agent.NewJournal(s)
		_, _ = agenttest.MustNew(model(), j1, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
		s.failNoCommit["tool:c1"] = true
		j2, _ := agent.NewJournal(s)
		_, err2 := agenttest.MustNew(model(), j2, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
		if fired != 1 {
			t.Fatalf("the held claim's drive fired %d times (%v), want once", fired, err2)
		}
		cs := agenttest.NewCountingStore(m)
		j3, _ := agent.NewJournal(cs)
		_, err3 := agenttest.MustNew(model(), j3, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
		var halt *agent.OutcomeUnknown
		if fired != 1 || !errors.As(err3, &halt) {
			t.Fatalf("a new process = %v with the effect fired %d times; want a halt and once", err3, fired)
		}
		// The halt comes from the drive's own read of the run: the marker is live, so no claim is
		// even attempted.
		if c := cs.Counts(); c.Insert != 0 {
			t.Fatalf("the halted drive made %d Inserts (%v); want none", c.Insert, c.Names)
		}
	})
	t.Run("step", func(t *testing.T) {
		m := agent.NewMemStore()
		fired := 0
		body := func(context.Context) (string, error) { fired++; return "ok", nil }
		s := &faultStore{m: m,
			commitErr:    map[string]bool{"attempt:step:pay": true},
			failNoCommit: map[string]bool{"attempt:not-started:": true},
		}
		j1, _ := agent.NewJournal(s)
		_, _ = j1.Step(ctx, "r", "pay", body)
		s.failNoCommit["pay"] = true
		j2, _ := agent.NewJournal(s)
		_, err2 := j2.Step(ctx, "r", "pay", body)
		if fired != 1 {
			t.Fatalf("the held claim's drive fired %d times (%v), want once", fired, err2)
		}
		j3, _ := agent.NewJournal(&faultStore{m: m})
		_, err3 := j3.Step(ctx, "r", "pay", body)
		var halt *agent.OutcomeUnknown
		if fired != 1 || !errors.As(err3, &halt) {
			t.Fatalf("a new process = %v with the effect fired %d times; want a halt and once", err3, fired)
		}
	})
}

// Deterministic reproductions of the counterexamples the TLA+ claims model (docs/design/
// formal-models.md, model 1) found against the claim rules: each test replays one trace step by
// step through store hooks, with no sleeps.

var errModelFault = errors.New("injected store fault")

// modelProc is one process's handle on a shared MemStore: its own store value, so its own
// in-flight steps and remembered claims. It implements no Leaser and no Unwrap, so a resolution
// through it takes the WithMinHaltAge path. fault, when set, picks an Insert's fault by name:
// "" (none), "nc" (fails, not committed) or "c" (commits, then fails). before runs ahead of each
// Insert, after runs once an Insert that did not fail has committed.
type modelProc struct {
	mem       *agent.MemStore
	mu        sync.Mutex
	fault     func(name string) string
	before    func(name string)
	after     func(name string)
	beforeGet func(name string)
}

func (p *modelProc) hooks() (func(string) string, func(string), func(string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fault, p.before, p.after
}

func (p *modelProc) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	fault, before, after := p.hooks()
	if before != nil {
		before(name)
	}
	mode := ""
	if fault != nil {
		mode = fault(name)
	}
	if mode == "nc" {
		return agent.Entry{}, false, errModelFault
	}
	e, ok, err := p.mem.Insert(ctx, runID, name, data)
	if err != nil {
		return e, ok, err
	}
	if mode == "c" {
		return agent.Entry{}, false, errModelFault
	}
	if after != nil {
		after(name)
	}
	return e, ok, nil
}

func (p *modelProc) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	p.mu.Lock()
	f := p.beforeGet
	p.mu.Unlock()
	if f != nil {
		f(name)
	}
	return p.mem.Get(ctx, runID, name)
}

func (p *modelProc) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return p.mem.Load(ctx, runID, after)
}

// Model trace regress/memo-overwrite (F1, liveness, no crash). In one process: d1 wins the first
// attempt's claim, is cancelled before its effect, and cannot record that it did not start, so the
// process remembers its claim id1. Meanwhile d2, which began its claim before id1 was remembered,
// fails its own marker Insert (d1's marker is there) and its not-started record commits and then
// errors, so the process remembers id2 too. Remembering id2 must not forget id1: the next claim
// writes the not-started record of every remembered id, which voids d1's marker, and runs the
// effect, which never ran. Remembering one id per marker key forgets id1, and every later drive
// halts on d1's marker for an effect nobody started.
func TestClaimMemo_KeepsEveryRememberedClaim(t *testing.T) {
	ctx := context.Background()
	p := &modelProc{mem: agent.NewMemStore()}
	j, err := agent.NewJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	const marker = "attempt:step:charge"
	var mu sync.Mutex
	markerInserts, nsInserts := 0, 0
	p.fault = func(name string) string {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case name == marker:
			markerInserts++
			if markerInserts == 2 { // d2's marker Insert fails, not committed
				return "nc"
			}
		case strings.HasPrefix(name, "attempt:not-started:"):
			nsInserts++
			switch nsInserts {
			case 1: // d1's not-started record fails, not committed: id1 is remembered
				return "nc"
			case 2: // d2's commits, then fails: id2 is remembered
				return "c"
			}
		}
		return ""
	}
	ctx1, cancel1 := context.WithCancel(ctx)
	defer cancel1()
	d2AtMarker, releaseD2 := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.before = func(name string) {
		mu.Lock()
		second := name == marker && markerInserts == 1
		mu.Unlock()
		if second { // d2 has begun its claim (took nothing) and is about to insert its marker
			close(d2AtMarker)
			<-releaseD2
		}
	}
	var ran atomic.Int32
	fn := func(context.Context) (string, error) { ran.Add(1); return "charged", nil }
	var d2Err error
	d2Done := make(chan struct{})
	p.after = func(name string) {
		if name != marker {
			return
		}
		once.Do(func() { // d1's claim has committed: d1 is cancelled, and d2 starts its claim
			cancel1()
			go func() { defer close(d2Done); _, d2Err = j.Step(ctx, "r", "charge", fn) }()
			<-d2AtMarker
		})
	}
	if _, err := j.Step(ctx1, "r", "charge", fn); err == nil {
		t.Fatal("d1 (cancelled after its claim) succeeded")
	}
	close(releaseD2)
	<-d2Done
	if d2Err == nil {
		t.Fatal("d2 (its marker Insert failed) succeeded")
	}
	p.mu.Lock()
	p.fault, p.before, p.after = nil, nil, nil
	p.mu.Unlock()
	if ran.Load() != 0 {
		t.Fatalf("the effect ran %d times before the re-drive, want 0", ran.Load())
	}
	v, err := j.Step(ctx, "r", "charge", fn)
	if err != nil || v != "charged" || ran.Load() != 1 {
		t.Fatalf("the re-drive = %q, %v, the effect ran %d times; want the effect run once: no attempt of it ever started", v, err, ran.Load())
	}
}

// Model traces findings/minage-revoid and findings/minage-intent (F2, double fire, no crash). A
// driver in process A claims a Step's attempt, is cancelled before its effect, and cannot record
// that it did not start: A remembers the claim, and the attempt is live. A reconciler in process R,
// on a store with no Leaser, resolves the halt as "not charged" with WithMinHaltAge. After its age
// check, a driver in A claims the effect: it writes the remembered not-started record (voiding
// the live attempt), claims the next attempt and calls the effect. The resolution must not then
// record "not charged" over a live driver's effect: the caller would call again and charge twice.
// The resolver claims the attempt after the live one under its own claim, and refuses
// (*HaltInFlight) when a driver holds it already.
func TestResolveHalt_MinAgeDoesNotOverrideARevivedClaim(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	pa := &modelProc{mem: mem}
	pr := &modelProc{mem: mem}
	ja, err := agent.NewJournal(pa)
	if err != nil {
		t.Fatal(err)
	}
	jr, err := agent.NewJournal(pr)
	if err != nil {
		t.Fatal(err)
	}
	pa.fault = func(name string) string {
		if strings.HasPrefix(name, "attempt:not-started:") {
			return "nc" // d1 cannot record that its attempt did not start
		}
		return ""
	}
	ctx1, cancel1 := context.WithCancel(ctx)
	defer cancel1()
	pa.after = func(name string) {
		if name == "attempt:step:charge" {
			cancel1() // d1 is cancelled once its claim has committed, before its effect
		}
	}
	var fired atomic.Int32
	inEffect, release := make(chan struct{}), make(chan struct{})
	fn := func(context.Context) (string, error) {
		if fired.Add(1) == 1 {
			close(inEffect)
			<-release
		}
		return "charged", nil
	}
	if _, err := ja.Step(ctx1, "r", "charge", fn); err == nil {
		t.Fatal("d1 (cancelled after its claim) succeeded")
	}
	pa.mu.Lock()
	pa.fault, pa.after = nil, nil
	pa.mu.Unlock()

	// The reconciler has read the run and checked the attempt's age; before it writes anything, d2
	// in process A revives the remembered claim and calls the effect.
	type res struct {
		v   string
		err error
	}
	d2 := make(chan res, 1)
	var once sync.Once
	pr.before = func(string) {
		once.Do(func() {
			go func() { v, err := ja.Step(ctx, "r", "charge", fn); d2 <- res{v, err} }()
			<-inEffect
		})
	}
	ref := agent.HaltRef{RunID: "r", Op: agent.OpRef{Kind: agent.OpStep, ID: "charge"}, Cause: agent.HaltCrashed}
	rerr := agent.ResolveHalt(ctx, jr, ref, agent.Outcome{Result: "not charged", IsError: true},
		agent.WithMinHaltAge(time.Second), agent.WithClock(func() time.Time { return time.Now().Add(time.Hour) }))
	close(release)
	got := <-d2
	rec, ok, err := ja.Get(ctx, "r", "charge")
	if err != nil || !ok {
		t.Fatalf("the step's record = %v, %v", ok, err)
	}
	if rec.IsError {
		t.Fatalf("the journal says %s (IsError) though the effect fired %d time(s); the resolution overrode a live driver (d2 = %q, %v; resolve = %v)",
			rec.Result, fired.Load(), got.v, got.err, rerr)
	}
	if _, ok := errors.AsType[*agent.HaltInFlight](rerr); !ok {
		t.Errorf("the resolution = %v; want *HaltInFlight (a driver holds the next attempt)", rerr)
	}
	if got.err != nil || got.v != "charged" || fired.Load() != 1 {
		t.Errorf("d2 = %q, %v, the effect fired %d time(s); want charged, once", got.v, got.err, fired.Load())
	}
}

// Model trace findings/resolve-void-on-error (F3, double fire, 3 error replies, no crash). A
// min-age resolution claims the attempt after the live one and then writes its verdict; the write
// errors but commits. If the resolution then records its own attempt as never started, a driver
// that lost that attempt to it and is checking whether it was voided finds it voided, claims the
// next attempt and runs the effect, though the journal now says "not charged". An errored verdict
// write leaves the resolution's attempt live: the driver then reads the verdict, or halts until
// the halt is resolved again.
func TestResolveHalt_AnErroredVerdictLeavesItsAttemptLive(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	pa := &modelProc{mem: mem}
	pr := &modelProc{mem: mem}
	ja, err := agent.NewJournal(pa)
	if err != nil {
		t.Fatal(err)
	}
	jr, err := agent.NewJournal(pr)
	if err != nil {
		t.Fatal(err)
	}
	pa.fault = func(name string) string {
		if strings.HasPrefix(name, "attempt:not-started:") {
			return "nc" // d1 cannot record that its attempt did not start: A remembers its claim
		}
		return ""
	}
	ctx1, cancel1 := context.WithCancel(ctx)
	defer cancel1()
	pa.after = func(name string) {
		if name == "attempt:step:charge" {
			cancel1()
		}
	}
	var fired atomic.Int32
	fn := func(context.Context) (string, error) { fired.Add(1); return "charged", nil }
	if _, err := ja.Step(ctx1, "r", "charge", fn); err == nil {
		t.Fatal("d1 (cancelled after its claim) succeeded")
	}
	pa.mu.Lock()
	pa.fault, pa.after = nil, nil
	pa.mu.Unlock()

	// d2 in A revives d1's claim (voiding attempt 0), loses attempt 1 to the resolution, and stops
	// as it reads whether attempt 1 was voided; the resolution's verdict write then commits and
	// errors, and d2 goes on.
	checking, resume := make(chan struct{}), make(chan struct{})
	var onceGet sync.Once
	pa.beforeGet = func(name string) {
		if strings.HasPrefix(name, "attempt:not-started:") && strings.HasSuffix(name, ":attempt:retry:1:step:charge") {
			onceGet.Do(func() { close(checking); <-resume })
		}
	}
	type res struct {
		v   string
		err error
	}
	d2 := make(chan res, 1)
	var once sync.Once
	pr.before = func(name string) {
		if name != "charge" {
			return
		}
		once.Do(func() {
			go func() { v, err := ja.Step(ctx, "r", "charge", fn); d2 <- res{v, err} }()
			<-checking
		})
	}
	pr.fault = func(name string) string {
		if name == "charge" {
			return "c" // the verdict commits, and the write reports an error
		}
		return ""
	}
	ref := agent.HaltRef{RunID: "r", Op: agent.OpRef{Kind: agent.OpStep, ID: "charge"}, Cause: agent.HaltCrashed}
	rerr := agent.ResolveHalt(ctx, jr, ref, agent.Outcome{Result: "not charged", IsError: true},
		agent.WithMinHaltAge(time.Second), agent.WithClock(func() time.Time { return time.Now().Add(time.Hour) }))
	if rerr == nil {
		t.Fatal("the resolution reported success though its verdict write failed")
	}
	close(resume)
	got := <-d2
	rec, ok, err := ja.Get(ctx, "r", "charge")
	if err != nil || !ok {
		t.Fatalf("the step's record = %v, %v", ok, err)
	}
	if fired.Load() != 0 {
		t.Fatalf("the effect fired %d time(s) after a verdict (%s, IsError=%v) was recorded (d2 = %q, %v)",
			fired.Load(), rec.Result, rec.IsError, got.v, got.err)
	}
}

// leasingProc is a modelProc that exposes the shared MemStore's Leaser through Unwrap, so a
// resolution through it takes the lease path.
type leasingProc struct{ *modelProc }

func (l leasingProc) Unwrap() agent.Store { return l.mem }

// Model trace findings/lease-revival (F4, double fire, no crash). d1 claims a Step's first
// attempt: its marker Insert commits and errors, and its not-started record fails, so process A
// remembers the claim. A resolution takes the run's lease (no driver holds it) and records "not
// charged". Meanwhile d2, a plain Step in A that holds no lease, takes the remembered claim, voids
// the first attempt and claims the second. The lease does not see d2, so the resolution must claim
// the attempt after the live one itself, as on the min-age path: d2 then loses it and reads the
// verdict instead of running the effect beside it.
func TestResolveHalt_LeasePathDoesNotOverrideARevivedClaim(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	pa := &modelProc{mem: mem}
	pr := &modelProc{mem: mem}
	ja, err := agent.NewJournal(pa)
	if err != nil {
		t.Fatal(err)
	}
	jr, err := agent.NewJournal(leasingProc{pr})
	if err != nil {
		t.Fatal(err)
	}
	pa.fault = func(name string) string {
		switch {
		case name == "attempt:step:charge":
			return "c" // d1's marker commits, and the Insert reports an error
		case strings.HasPrefix(name, "attempt:not-started:"):
			return "nc" // and its not-started record fails: A remembers the claim
		}
		return ""
	}
	var fired atomic.Int32
	fn := func(context.Context) (string, error) { fired.Add(1); return "charged", nil }
	if _, err := ja.Step(ctx, "r", "charge", fn); err == nil {
		t.Fatal("d1 (its marker Insert failed) succeeded")
	}
	pa.mu.Lock()
	pa.fault = nil
	pa.mu.Unlock()

	// Before the resolution writes its verdict, d2 revives d1's claim, voids attempt 0, and stops
	// just before its Insert of attempt 1; it goes on once the verdict is written.
	atRetry, resume := make(chan struct{}), make(chan struct{})
	var onceA sync.Once
	pa.before = func(name string) {
		if name == "attempt:retry:1:step:charge" {
			onceA.Do(func() { close(atRetry); <-resume })
		}
	}
	type res struct {
		v   string
		err error
	}
	d2 := make(chan res, 1)
	var onceR sync.Once
	pr.before = func(name string) {
		if name != "charge" {
			return
		}
		onceR.Do(func() {
			go func() { v, err := ja.Step(ctx, "r", "charge", fn); d2 <- res{v, err} }()
			<-atRetry
		})
	}
	ref := agent.HaltRef{RunID: "r", Op: agent.OpRef{Kind: agent.OpStep, ID: "charge"}, Cause: agent.HaltCrashed}
	rerr := agent.ResolveHalt(ctx, jr, ref, agent.Outcome{Result: "not charged", IsError: true})
	close(resume)
	got := <-d2
	rec, ok, err := ja.Get(ctx, "r", "charge")
	if err != nil || !ok {
		t.Fatalf("the step's record = %v, %v (resolve = %v)", ok, err, rerr)
	}
	if fired.Load() != 0 {
		t.Fatalf("the effect fired %d time(s) beside the verdict %s (IsError=%v); d2 = %q, %v; resolve = %v",
			fired.Load(), rec.Result, rec.IsError, got.v, got.err, rerr)
	}
	if rerr != nil {
		t.Errorf("the resolution = %v; want nil (it held the lease and the next attempt)", rerr)
	}
}

var errCommittedThenLost = errors.New("connection lost after commit")

// sweepStore stores the k-th new entry and then reports an error for it (A3's "complete on
// error" half), once.
type sweepStore struct {
	m      *agent.MemStore
	mu     sync.Mutex
	writes int
	k      int
	failed string
}

func (c *sweepStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	e, ok, err := c.m.Insert(ctx, runID, name, data)
	if err != nil || !ok {
		return e, ok, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	if c.writes == c.k {
		c.failed = name
		return agent.Entry{}, false, errCommittedThenLost
	}
	return e, ok, nil
}
func (c *sweepStore) Get(ctx context.Context, r, n string) (agent.Entry, bool, error) {
	return c.m.Get(ctx, r, n)
}
func (c *sweepStore) Load(ctx context.Context, r string, a int64) iter.Seq2[agent.Entry, error] {
	return c.m.Load(ctx, r, a)
}

// For every write of a run with one side-effect call, the write commits and reports an error.
// The run is then driven again, in the same process (same store value) or in a new one (a
// different store value over the same data). The side effect must fire at most once, the run must
// end complete or halted, and it must not halt over an effect that never ran.
func TestCommitThenFailSweep(t *testing.T) {
	for k := 1; k <= 7; k++ {
		for _, sameProcess := range []bool{true, false} {
			t.Run(fmt.Sprintf("write%d/same=%v", k, sameProcess), func(t *testing.T) {
				ctx := context.Background()
				m := agent.NewMemStore()
				fired := 0
				charge := agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { fired++; return "ok", nil })
				model := func() agent.Model {
					return agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.TextTurn("done"))
				}
				s := &sweepStore{m: m, k: k}
				j, _ := agent.NewJournal(s)
				_, err1 := agenttest.MustNew(model(), j, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
				var j2 *agent.Journal
				if sameProcess {
					j2, _ = agent.NewJournal(s) // same store value: shares the process's memory
				} else {
					j2, _ = agent.NewJournal(&sweepStore{m: m}) // a new process
				}
				_, err2 := agenttest.MustNew(model(), j2, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
				var halt *agent.OutcomeUnknown
				t.Logf("failed %q: first %v; second %v; fired %d", s.failed, err1, err2, fired)
				if fired > 1 {
					t.Fatalf("fired %d times", fired)
				}
				if err2 != nil && !errors.As(err2, &halt) {
					t.Errorf("second drive = %v, want complete or halted", err2)
				}
				if errors.As(err2, &halt) && fired == 0 {
					t.Errorf("halted over an effect that never ran: the claimant knew it did not start it")
				}
			})
		}
	}
}

// Audit's reading of the claim bookkeeping kinds (review of the claim protocol): a journal holding
// not-started records proves and verifies like any other; no such record is taken for
// a completed call or step; a call voided by a not-started record is provably absent from the
// tool-use key set, and a call that fired under a held claim is provably present.

func TestAuditReadsClaimBookkeeping(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(nil)
	m := agent.NewMemStore()
	fired := 0
	charge := agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { fired++; return "ok", nil })
	model := func() agent.Model {
		return agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.ToolTurn("c2", "charge", `{}`), agenttest.TextTurn("done"))
	}
	// c1: marker commits and errors, then not-started is recorded (voided); re-attempted under
	// attempt:retry:1. c2: claim taken back and held.
	s := &r3Store{m: m, faults: []r3Fault{{"attempt:tool:c1", "c"}}}
	j, _ := agent.NewJournal(s)
	_, err := agenttest.MustNew(model(), j, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
	t.Logf("drive 1: %v", err)
	s.faults = []r3Fault{{"attempt:tool:c2", "nc"}, {"attempt:not-started:", "nc"}}
	_, err = agenttest.MustNew(model(), j, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
	t.Logf("drive 2: %v", err)
	_, err = agenttest.MustNew(model(), j, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
	t.Logf("drive 3: %v; fired %d", err, fired)
	r3Dump(t, m, "r")
	if fired != 2 {
		t.Fatalf("fired %d, want 2", fired)
	}

	pkg, err := audit.Evidence(ctx, agenttest.MustJournal(m), "r", audit.Ed25519Signer{Priv: priv}, 1, audit.WithAllToolCalls())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range pkg.Actions {
		rec, err := a.Bundle.Record()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("action: %s %s (record %q kind %s)", a.Kind, a.Label, rec.Name, rec.Kind)
		if rec.Kind == agent.StepNotStarted {
			t.Fatalf("bookkeeping record %q offered as evidence of an action", rec.Name)
		}
	}
	if len(pkg.Actions) != 2 {
		t.Fatalf("%d actions, want the two completed calls", len(pkg.Actions))
	}
	rep, err := pkg.Verify(audit.Ed25519Verifier{Pub: pub})
	if err != nil || !rep.OK {
		t.Fatalf("verify: %v %+v", err, rep)
	}
	// Every leaf, bookkeeping included, proves against the root.
	mj := agenttest.MustJournal(m)
	hist, _ := mj.History(ctx, "r")
	root, err := audit.Root(ctx, mj, "r")
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range hist {
		p, err := audit.Prove(ctx, mj, "r", i)
		if err != nil {
			t.Fatal(err)
		}
		if err := audit.VerifyInclusion(root, r.Raw(), p); err != nil {
			t.Fatalf("leaf %d (%s) does not verify: %v", i, r.Name, err)
		}
	}
	// Tool-use key set: completed calls only.
	for _, key := range []string{"tooluse:c1", "tooluse:c2"} {
		if _, err := audit.ProveAbsent(hist, audit.ToolUseKeys, key); err == nil {
			t.Fatalf("%s proven absent, but the call completed", key)
		}
	}
	if _, err := audit.ProveAbsent(hist, audit.ToolUseKeys, "tooluse:c9"); err != nil {
		t.Fatalf("absence of a call never made: %v", err)
	}
}

// Saga rollback's reading of not-started records and re-attempted calls (review of the claim protocol).

type sagaCounts struct{ fired, undone int }

func sagaTools(c *sagaCounts) []agent.Tool {
	charge := agent.MustCompensatedFunc("charge", "", func(context.Context, struct{}) (string, error) { c.fired++; return "charged", nil },
		func(context.Context, struct{}, string) error { c.undone++; return nil })
	boom := agent.MustFunc("boom", "", func(context.Context, struct{}) (string, error) {
		return "", errors.New("boom")
	})
	return append([]agent.Tool{}, charge, boom)
}

// sagaFail records that a later call (c2) failed the saga, which starts the rollback.
func sagaFail(t *testing.T, m *agent.MemStore, runID string) {
	t.Helper()
	_, err := journalhook.Do(context.Background(), agenttest.MustJournal(m), runID, agent.ToolResultStep("c2"), func(context.Context) (any, error) {
		return agent.Record{Kind: agent.StepSagaFail, ToolUseID: "c2", Result: json.RawMessage(`"boom"`)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An attempt voided by its not-started record changed nothing: the rollback neither compensates
// it nor halts on it.
func TestSagaSkipsVoidedAttempt(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	c := &sagaCounts{}
	model := func() agent.Model {
		return agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.ToolTurn("c2", "boom", `{}`), agenttest.TextTurn("done"))
	}
	s := &r3Store{m: m, faults: []r3Fault{{"attempt:tool:c1", "c"}}} // marker commits, errors: voided
	j, _ := agent.NewJournal(s)
	_, err1 := agenttest.MustNew(model(), j, agent.WithTools(sagaTools(c)...), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	t.Logf("drive 1: %v", err1)
	sagaFail(t, m, "r")
	_, err2 := agenttest.MustNew(
		model(),
		agenttest.MustJournal(m),
		agent.WithTools(sagaTools(c)...),
		agent.WithMaxConcurrency(1),
	).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	t.Logf("drive 2: %v; fired %d undone %d", err2, c.fired, c.undone)
	r3Dump(t, m, "r")
	var ab *agent.SagaAborted
	if !errors.As(err2, &ab) || ab.CompensateErr != nil || c.undone != 0 || c.fired != 0 {
		t.Fatalf("want a clean abort that compensates nothing")
	}
}

// A call that fired under a re-attempt after a claim whose writes failed and recorded its result is a write:
// the rollback compensates it.
func TestSagaCompensatesCallFiredUnderHeldClaim(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	c := &sagaCounts{}
	model := func() agent.Model {
		return agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.ToolTurn("c2", "boom", `{}`), agenttest.TextTurn("done"))
	}
	s := &r3Store{m: m, faults: []r3Fault{{"attempt:tool:c1", "nc"}, {"attempt:not-started:", "nc"}}}
	j, _ := agent.NewJournal(s)
	_, err1 := agenttest.MustNew(model(), j, agent.WithTools(sagaTools(c)...), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	_, err2 := agenttest.MustNew(model(), j, agent.WithTools(sagaTools(c)...), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	t.Logf("drive 1: %v\ndrive 2: %v; fired %d undone %d", err1, err2, c.fired, c.undone)
	r3Dump(t, m, "r")
	var ab *agent.SagaAborted
	if !errors.As(err2, &ab) || c.fired != 1 || c.undone != 1 {
		t.Fatalf("want the fired call compensated once")
	}
}

// A call that fired under a held claim whose result was lost may have taken effect: the rollback
// halts on it.
func TestSagaHaltsOnHeldClaimWithoutResult(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	c := &sagaCounts{}
	model := func() agent.Model {
		return agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.ToolTurn("c2", "boom", `{}`), agenttest.TextTurn("done"))
	}
	s := &r3Store{m: m, faults: []r3Fault{{"attempt:tool:c1", "nc"}, {"attempt:not-started:", "nc"}}}
	j, _ := agent.NewJournal(s)
	_, err1 := agenttest.MustNew(model(), j, agent.WithTools(sagaTools(c)...), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	s.faults = []r3Fault{{"tool:c1", "nc"}} // the result write is lost after the call fired
	_, err2 := agenttest.MustNew(model(), j, agent.WithTools(sagaTools(c)...), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	sagaFail(t, m, "r")
	_, err3 := agenttest.MustNew(
		model(),
		agenttest.MustJournal(m),
		agent.WithTools(sagaTools(c)...),
		agent.WithMaxConcurrency(1),
	).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	t.Logf("drive 1: %v\ndrive 2: %v\ndrive 3: %v; fired %d undone %d", err1, err2, err3, c.fired, c.undone)
	r3Dump(t, m, "r")
	var ab *agent.SagaAborted
	var halt *agent.OutcomeUnknown
	if c.fired != 1 || !errors.As(err3, &ab) || !errors.As(ab.CompensateErr, &halt) {
		t.Fatalf("want the rollback to halt on the call that fired with no result")
	}
}
