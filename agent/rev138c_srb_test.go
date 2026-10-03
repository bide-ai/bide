package agent_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// srbStore is a MemStore seen only as a Store: no Unwrap, so the session finds no Leaser.
type srbStore struct{ agent.Store }

// srbSetup builds a session whose saga turn for "one" ran book, paused on pay's approval, and was
// cancelled, so the next message drives the turn's rollback, which runs book's compensator. The
// compensator signals in (once), waits for out, counts the compensations in flight at once, and
// sets done before it returns.
func srbSetup(t *testing.T, leaser bool, onComp func()) (s *agent.Session, in, out chan struct{}, maxInFlight *atomic.Int32, done *atomic.Bool) {
	t.Helper()
	ctx := context.Background()
	var j *agent.Journal
	if leaser {
		j, _ = p14Journal(t)
	} else {
		store := srbStore{agent.NewMemStore()}
		if _, ok := agent.Capability[agent.Leaser](store); ok {
			t.Fatal("the store exposes a Leaser")
		}
		var err error
		if j, err = agent.NewJournal(store); err != nil {
			t.Fatal(err)
		}
	}
	in, out = make(chan struct{}), make(chan struct{})
	maxInFlight, done = new(atomic.Int32), new(atomic.Bool)
	var inFlight atomic.Int32
	var once sync.Once
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error {
			n := inFlight.Add(1)
			for m := maxInFlight.Load(); n > m && !maxInFlight.CompareAndSwap(m, n); m = maxInFlight.Load() {
			}
			if onComp != nil {
				onComp()
			}
			once.Do(func() { close(in) })
			<-out
			inFlight.Add(-1)
			done.Store(true)
			return nil
		})
	var c counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "book")}}, {calls: []agent.ToolUse{call("c2", "pay")}}, {text: "done"}, {text: "done2"}}}
	a := p14Build(t, model, j, agent.WithTools(book, c.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	var err error
	if s, err = a.Session(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, agent.UserText("one"), agent.WithSaga()); err == nil {
		t.Fatal("want the approval pause")
	}
	if err := agent.Cancel(ctx, j, "s>@turn/0", "stop"); err != nil {
		t.Fatal(err)
	}
	return s, in, out, maxInFlight, done
}

// Over a store with a Leaser, a second caller on the same handle while the first drives a
// cancelled saga turn's rollback (SRb, without s.mu) does not wait for it: it finds the turn run
// leased and gets ErrTurnContended, as a caller on another handle does.
func TestRev138c_SiblingSendDuringRollback(t *testing.T) {
	ctx := context.Background()
	s, in, out, maxInFlight, _ := srbSetup(t, true, nil)
	errA := make(chan error, 1)
	go func() { _, err := s.Send(ctx, agent.UserText("two")); errA <- err }()
	<-in // A is inside the compensator, without s.mu
	_, errB := s.Send(ctx, agent.UserText("three"))
	close(out)
	if err := <-errA; err != nil && !agent.IsPause(err) && !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("A: %v", err) // its own turn pauses (pay), or B's open turn refuses it
	}
	if !errors.Is(errB, agent.ErrTurnContended) {
		t.Fatalf("a sibling Send on the same handle during the rollback = %v, want ErrTurnContended", errB)
	}
	if n := maxInFlight.Load(); n != 1 {
		t.Fatalf("%d compensations ran at once", n)
	}
}

// Over a store with no Leaser, nothing keeps two drivers off one turn run, so the session holds
// s.mu across the rollback: the compensator runs with the handle's mutex held.
func TestRev138c_RollbackHoldsTheSessionMutexWithoutALeaser(t *testing.T) {
	ctx := context.Background()
	var s *agent.Session
	var held bool
	s, in, out, _, _ := srbSetup(t, false, func() { held = agent.SessionMuHeld(s) })
	close(out)
	if _, err := s.Send(ctx, agent.UserText("two")); err != nil && !agent.IsPause(err) {
		t.Fatal(err) // its own turn pauses on pay's approval
	}
	select {
	case <-in:
	default:
		t.Fatal("the turn's compensator did not run")
	}
	if !held {
		t.Fatal("over a store with no Leaser, the rollback ran without the session's mutex")
	}
}

// Over a store with no Leaser, a second caller on the same handle waits for the first's rollback
// of the cancelled saga turn instead of driving the same rollback concurrently: one compensation
// at a time, and the second caller returns only once the rollback is over. (The mutex itself is
// pinned by TestRev138c_RollbackHoldsTheSessionMutexWithoutALeaser; here B may reach the handle
// before or after A's rollback is released, and either way must not drive it too.)
func TestRev138c_SiblingSendWaitsForRollbackWithoutALeaser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute) // bounds a regression that blocks B
	defer cancel()
	s, in, out, maxInFlight, done := srbSetup(t, false, nil)
	errA := make(chan error, 1)
	go func() { _, err := s.Send(ctx, agent.UserText("two")); errA <- err }()
	<-in // A is inside the compensator
	errB := make(chan error, 1)
	returnedEarly := make(chan bool, 1)
	go func() {
		_, err := s.Send(ctx, agent.UserText("three"))
		returnedEarly <- !done.Load()
		errB <- err
	}()
	close(out)
	if err := <-errA; err != nil && !agent.IsPause(err) && !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("A: %v", err) // its own turn pauses (pay), or B's open turn refuses it
	}
	if <-returnedEarly {
		t.Fatal("B returned while A's rollback was in progress")
	}
	if err := <-errB; errors.Is(err, agent.ErrTurnContended) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("B = %v", err)
	}
	if n := maxInFlight.Load(); n != 1 {
		t.Fatalf("%d compensations ran at once", n)
	}
}
