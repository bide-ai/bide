package agent

import (
	"context"
	"errors"
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
