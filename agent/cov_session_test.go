package agent

import (
	"context"
	"errors"
	"iter"
	"sync/atomic"
	"testing"
)

// A session whose journal holds a turn or turn start that does not decode is not opened (ErrProtocol,
// naming the session): rebuilding a transcript from a corrupt record could answer the wrong turn.
func TestSession_CorruptJournalRefusesToOpen(t *testing.T) {
	ctx := context.Background()
	for _, step := range []string{sessionTurnStep(0), sessionStartStep(0)} {
		t.Run(step, func(t *testing.T) {
			j := memJournal()
			writeRaw(t, j, sessionJournalID("c1"), step, Record{Kind: StepValue, Result: []byte(`[1]`)})
			a := mustNew(&replyModel{}, j)
			if _, err := a.Session(ctx, "c1"); !errors.Is(err, ErrProtocol) {
				t.Fatalf("Session over a corrupt %s = %v; want ErrProtocol", step, err)
			}
		})
	}
}

// A session whose transcript cannot be loaded is a storage error, not an empty session.
func TestSession_LoadFailureIsStorageError(t *testing.T) {
	a := mustNew(&replyModel{}, mustJournal(loadFails{NewMemStore()}))
	if _, err := a.Session(context.Background(), "c1"); !errors.Is(err, ErrStorage) {
		t.Fatalf("Session = %v; want ErrStorage", err)
	}
}

// Send and SendOnce refuse a nil run option, and SendOnce an empty key, before the model is called
// or anything is journaled for the turn.
func TestSession_SendRefusals(t *testing.T) {
	ctx := context.Background()
	m := &replyModel{}
	j := memJournal()
	s := openSession(t, mustNew(m, j), "c1")
	if _, err := s.Send(ctx, UserText("hi"), nil); !errors.Is(err, ErrConfig) {
		t.Errorf("Send with a nil option = %v; want ErrConfig", err)
	}
	if _, err := s.SendOnce(ctx, "k", UserText("hi"), nil); !errors.Is(err, ErrConfig) {
		t.Errorf("SendOnce with a nil option = %v; want ErrConfig", err)
	}
	if _, err := s.SendOnce(ctx, "", UserText("hi")); !errors.Is(err, ErrConfig) {
		t.Errorf("SendOnce with an empty key = %v; want ErrConfig", err)
	}
	if n := m.calls.Load(); n != 0 {
		t.Errorf("the model was called %d times by refused sends", n)
	}
	if _, ok, _ := j.Get(ctx, sessionJournalID("c1"), sessionStartStep(0)); ok {
		t.Error("a refused send recorded a turn start")
	}
}

// Redelivering a SendOnce key answers from the recorded turn and does not call the model again,
// and the session reopened by another handle sees the same keyed turn.
func TestSession_SendOnceRedeliveryAnswersFromTheJournal(t *testing.T) {
	ctx := context.Background()
	m := &replyModel{}
	a := mustNew(m, memJournal())
	s := openSession(t, a, "c1")
	first, err := s.SendOnce(ctx, "k1", UserText("hi"))
	if err != nil || first.Message.Text() != "re: hi" {
		t.Fatalf("SendOnce = %v, %v", first, err)
	}
	for _, h := range []*Session{s, openSession(t, a, "c1")} {
		again, err := h.SendOnce(ctx, "k1", UserText("hi"))
		if err != nil || again.Message.Text() != "re: hi" {
			t.Fatalf("redelivery = %v, %v", again, err)
		}
	}
	if n := m.calls.Load(); n != 1 {
		t.Fatalf("model called %d times for one key; want 1", n)
	}
}

// covToggleLoad fails Load while armed.
type covToggleLoad struct {
	Store
	armed atomic.Bool
}

func (s *covToggleLoad) Unwrap() Store { return s.Store }
func (s *covToggleLoad) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if s.armed.Load() {
		return func(yield func(Entry, error) bool) { yield(Entry{}, errCovBlip) }
	}
	return s.Store.Load(ctx, runID, after)
}

// A SendOnce whose key is not in the handle's view reloads the transcript first; when that fails the
// send is a storage error that names the turn's run, the model is not called, and the same key
// succeeds once the store is back.
func TestSession_SendOnceReloadFailure(t *testing.T) {
	ctx := context.Background()
	st := &covToggleLoad{Store: NewMemStore()}
	m := &replyModel{}
	s := openSession(t, mustNew(m, mustJournal(st)), "c1")
	st.armed.Store(true)
	res, err := s.SendOnce(ctx, "k1", UserText("hi"))
	if !errors.Is(err, ErrStorage) || res == nil || res.RunID != sessionEventRunID("c1", "k1") {
		t.Fatalf("SendOnce = %+v, %v; want ErrStorage and the turn's run ID", res, err)
	}
	if m.calls.Load() != 0 {
		t.Fatal("the model was called on a session whose transcript could not be loaded")
	}
	st.armed.Store(false)
	if res, err := s.SendOnce(ctx, "k1", UserText("hi")); err != nil || res.Message.Text() != "re: hi" {
		t.Fatalf("SendOnce after recovery = %+v, %v", res, err)
	}
}

// A Send on a handle that fell behind another writer catches up from the journal and still gets its
// own turn: the other writer's turn is kept, the model sees it, and the stale handle's message is
// answered with its own reply.
func TestSession_StaleHandleCatchesUp(t *testing.T) {
	ctx := context.Background()
	a := mustNew(&replyModel{}, memJournal())
	s1, s2 := openSession(t, a, "c1"), openSession(t, a, "c1")
	if res, err := s1.Send(ctx, UserText("one")); err != nil || res.Message.Text() != "re: one" {
		t.Fatalf("s1 = %+v, %v", res, err)
	}
	res, err := s2.Send(ctx, UserText("two"))
	if err != nil || res.Message.Text() != "re: two" {
		t.Fatalf("stale s2 = %+v, %v; want its own reply", res, err)
	}
	if got := s2.Turns(); got != 2 {
		t.Fatalf("s2 has %d turns; want 2 (the other writer's and its own)", got)
	}
	if got := openSession(t, a, "c1").Turns(); got != 2 {
		t.Fatalf("a fresh handle sees %d turns; want 2", got)
	}
}
