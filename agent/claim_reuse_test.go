package agent_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

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
	charge := agent.Func("charge", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { fired++; return "ok", nil })
	model := func() agent.Model {
		return agent.NewScriptedModel(agent.ToolTurn("c1", "charge", `{}`), agent.TextTurn("done"))
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
	_, err1 := agent.Step(ctx, j1, "r", "pay", body)
	t.Logf("drive 1: %v (fired %d)", err1, fired)

	s.failNoCommit["pay"] = true
	j2, _ := agent.NewJournal(s)
	_, err2 := agent.Step(ctx, j2, "r", "pay", body)
	t.Logf("drive 2: %v (fired %d)", err2, fired)

	j3, _ := agent.NewJournal(&faultStore{m: m})
	_, err3 := agent.Step(ctx, j3, "r", "pay", body)
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
	_, err1 := agent.Step(ctx, j1, "r", "pay", body)
	j2, _ := agent.NewJournal(&faultStore{m: m})
	_, err2 := agent.Step(ctx, j2, "r", "pay", body)
	var halt *agent.ResumeHalt
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
	_, ea := agent.Step(ctx, ja, "r", "pay", body)
	_, eb := agent.Step(ctx, jb, "r", "pay", body)
	for i := 0; i < 3; i++ {
		jn, _ := agent.NewJournal(&faultStore{m: m})
		_, _ = agent.Step(ctx, jn, "r", "pay", body)
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
		charge := agent.Func("charge", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { fired++; return "ok", nil })
		model := func() agent.Model {
			return agent.NewScriptedModel(agent.ToolTurn("c1", "charge", `{}`), agent.TextTurn("done"))
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
		var halt *agent.ResumeHalt
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
		_, _ = agent.Step(ctx, j1, "r", "pay", body)
		s.failNoCommit["pay"] = true
		j2, _ := agent.NewJournal(s)
		_, err2 := agent.Step(ctx, j2, "r", "pay", body)
		if fired != 1 {
			t.Fatalf("the held claim's drive fired %d times (%v), want once", fired, err2)
		}
		j3, _ := agent.NewJournal(&faultStore{m: m})
		_, err3 := agent.Step(ctx, j3, "r", "pay", body)
		var halt *agent.ResumeHalt
		if fired != 1 || !errors.As(err3, &halt) {
			t.Fatalf("a new process = %v with the effect fired %d times; want a halt and once", err3, fired)
		}
	})
}
