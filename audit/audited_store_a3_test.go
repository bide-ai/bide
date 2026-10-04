package audit_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"iter"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// a3Store commits an Insert of the names in failAfter, then reports an error (A3: an errored
// write may have committed), once per name.
type a3Store struct {
	inner     agent.Store
	mu        sync.Mutex
	failAfter map[string]bool
}

func (s *a3Store) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	e, ins, err := s.inner.Insert(ctx, runID, name, data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && ins && s.failAfter[name] {
		delete(s.failAfter, name)
		return agent.Entry{}, false, errors.New("connection lost after commit")
	}
	return e, ins, err
}
func (s *a3Store) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	return s.inner.Get(ctx, runID, name)
}
func (s *a3Store) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return s.inner.Load(ctx, runID, after)
}

// A write that commits but whose Insert errors (A3) is anchored all the same: the retry finds it
// stored (inserted false) or the run reads it back, and the anchored head must still cover the
// whole journal, the record the run ends on included.
func TestAuditedStore_A3WriteIsAnchored(t *testing.T) {
	for _, name := range []string{"run:complete", "@llm/0"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			_, priv, _ := ed25519.GenerateKey(nil)
			log := audit.NewMemAnchorLog()
			var anchorErrs int
			inner := &a3Store{inner: agent.NewMemStore(), failAfter: map[string]bool{name: true}}
			as, err := audit.NewAuditedStore(inner, edS(priv), log)
			if err != nil {
				t.Fatal(err)
			}
			as.OnError(func(string, error) { anchorErrs++ })
			j := agenttest.MustJournal(as)
			a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("done"), agenttest.TextTurn("done")), j)
			_, err1 := a.Run(ctx, "r1", agent.UserText("hi"))
			res, err2 := a.Run(ctx, "r1", agent.UserText("hi"))
			hist, _ := j.History(ctx, "r1")
			var names []string
			for _, r := range hist {
				names = append(names, r.Name)
			}
			last := 0
			for _, e := range log.Entries() {
				if e.RunID == "r1" && e.STH.Size > last {
					last = e.STH.Size
				}
			}
			t.Logf("first run err=%v; second run err=%v res=%v; journal %v; largest anchored size %d; OnError calls %d", err1, err2, res != nil, names, last, anchorErrs)
			if last != len(hist) {
				t.Fatalf("journal holds %d entries but the largest anchored head covers %d, with %d OnError calls", len(hist), last, anchorErrs)
			}
		})
	}
}

// failSizeAnchor fails every publish of tree size n while down.
type failSizeAnchor struct {
	inner *audit.MemAnchorLog
	n     int
	down  bool
}

func (a *failSizeAnchor) Publish(ctx context.Context, runID string, sth audit.SignedTreeHead) error {
	if a.down && sth.Size == a.n {
		return errors.New("anchor down")
	}
	return a.inner.Publish(ctx, runID, sth)
}

// The publish of a run's last write fails: OnError reports it, and nothing the run does later
// retries it (a finished run writes nothing). Reanchor, called on that signal, anchors the whole
// journal once the anchor is back, and anchors nothing more when called again.
func TestAuditedStore_FailedLastPublishIsReanchored(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(nil)
	log := audit.NewMemAnchorLog()
	anchor := &failSizeAnchor{inner: log, n: 4, down: true}
	var failed []string
	as, err := audit.NewAuditedStore(agent.NewMemStore(), edS(priv), anchor)
	if err != nil {
		t.Fatal(err)
	}
	as.OnError(func(runID string, _ error) { failed = append(failed, runID) })
	j := agenttest.MustJournal(as)
	if _, err := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("done")), j).Run(ctx, "r1", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	largest := func() int {
		n := 0
		for _, e := range log.Entries() {
			n = max(n, e.STH.Size)
		}
		return n
	}
	if len(failed) != 1 || largest() != 3 {
		t.Fatalf("OnError calls %v, largest anchored %d; want one for r1 and 3", failed, largest())
	}
	if err := as.Reanchor(ctx, "r1"); err == nil {
		t.Fatal("Reanchor with the anchor still down: no error")
	}
	anchor.down = false
	if err := as.Reanchor(ctx, "r1"); err != nil || largest() != 4 {
		t.Fatalf("Reanchor: %v, largest anchored %d; want 4", err, largest())
	}
	n := len(log.Entries())
	if err := as.Reanchor(ctx, "r1"); err != nil || len(log.Entries()) != n {
		t.Fatalf("a second Reanchor: %v, %d entries (was %d); want none published", err, len(log.Entries()), n)
	}
}

// lostStore loses (stores nothing, and errors) the first Insert of name.
type lostStore struct {
	agent.Store
	name string
	once sync.Once
}

func (s *lostStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	lost := false
	if name == s.name {
		s.once.Do(func() { lost = true })
	}
	if lost {
		return agent.Entry{}, false, errors.New("write lost")
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// A failed insert of a run's first record leaves the journal holding only its header: anchoring
// after the failed insert publishes no head of the header alone.
func TestAuditedStore_FailedFirstRecordAnchorsNoHeaderOnlyHead(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(nil)
	log := audit.NewMemAnchorLog()
	as, err := audit.NewAuditedStore(&lostStore{Store: agent.NewMemStore(), name: "run:start"}, edS(priv), log)
	if err != nil {
		t.Fatal(err)
	}
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("done")), agenttest.MustJournal(as))
	if _, err := a.Run(ctx, "r1", agent.UserText("hi")); err == nil {
		t.Fatal("the run's first record was lost, yet the run succeeded")
	}
	if n := len(log.Entries()); n != 0 {
		t.Fatalf("%d heads anchored for a journal holding only its header; want none", n)
	}
	if _, err := a.Run(ctx, "r1", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	for _, e := range log.Entries() {
		if e.STH.Size < 2 {
			t.Fatalf("a head of size %d was anchored", e.STH.Size)
		}
	}
}

// An insert under a context that is done anchors nothing and reports nothing to OnError (the
// anchoring would fail with the context); the next write covers what it stored.
func TestAuditedStore_DoneContextDoesNotAnchor(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	log := audit.NewMemAnchorLog()
	var errs int
	as, err := audit.NewAuditedStore(agent.NewMemStore(), edS(priv), log)
	if err != nil {
		t.Fatal(err)
	}
	as.OnError(func(string, error) { errs++ })
	j := agenttest.MustJournal(as)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := journaltest.Put(context.Background(), j, "r", "a", agent.Record{Kind: agent.StepValue}); err != nil {
		t.Fatal(err)
	}
	n := len(log.Entries())
	cancel()
	_, _, _ = as.Insert(ctx, "r", "b", nil)
	if errs != 0 || len(log.Entries()) != n {
		t.Fatalf("a done context: %d OnError calls, %d heads (was %d); want none and none", errs, len(log.Entries()), n)
	}
}
