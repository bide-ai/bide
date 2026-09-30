package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// drvKey tags a context with the driver it belongs to, for the hooks below.
type drvKey struct{}

func drvOf(ctx context.Context) int { d, _ := ctx.Value(drvKey{}).(int); return d }

// getHookStore is a Store whose Get of a name calls get first.
type getHookStore struct {
	Store
	get func(ctx context.Context, name string)
}

func (s *getHookStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if s.get != nil {
		s.get(ctx, name)
	}
	return s.Store.Get(ctx, runID, name)
}

// doHookDurable is a Durable wrapper (audit.AuditedStore's shape) whose Do of a name calls do first.
type doHookDurable struct {
	Durable
	do func(ctx context.Context, name string)
}

func (w *doHookDurable) Unwrap() Durable { return w.Durable }

func (w *doHookDurable) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	if w.do != nil {
		w.do(ctx, name)
	}
	return w.Durable.Do(ctx, runID, name, fn)
}

// The claim model's regress/loser-leads schedule (spec/tla/claims/regress/loser-leads.cfg,
// WinnerNeverHalts), through a Durable wrapper: two drivers of one Step in one process. d1 wins
// the attempt's claim; d2 loses it and reads the step's result; d1 then sends the step. The
// loser's read must not be a call in flight that the winner joins: the winner would take the
// loser's halt as its own outcome, record that it never started, and halt though it owns the
// attempt. The winner must run the step, and the loser gets its value or a halt.
func TestLoserLeads_WinnerNeverHaltsThroughWrapper(t *testing.T) {
	ctx := context.Background()
	d1At := make(chan struct{})       // d1 has won its claim and is about to send the step
	d2Reading := make(chan struct{})  // d2, the loser, is reading the step's result
	joined := make(chan struct{})     // a caller joined a call of the step in flight
	winnerDone := make(chan struct{}) // d1's Step returned
	var once sync.Once
	h := func(k flightKey) {
		if k.name == "pay" {
			once.Do(func() { close(joined) })
		}
	}
	flightJoinHook.Store(&h)
	t.Cleanup(func() { flightJoinHook.Store(nil) })

	var readOnce sync.Once
	st := &getHookStore{Store: NewMemStore(), get: func(ctx context.Context, name string) {
		if name == "pay" && drvOf(ctx) == 2 {
			readOnce.Do(func() {
				close(d2Reading)
				select { // hold the loser's read until the winner joins it, or finishes on its own
				case <-joined:
				case <-winnerDone:
				}
			})
		}
	}}
	j, err := NewJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	var atOnce sync.Once
	w := &doHookDurable{Durable: j, do: func(ctx context.Context, name string) {
		if name == "pay" && drvOf(ctx) == 1 {
			atOnce.Do(func() {
				close(d1At)
				<-d2Reading
			})
		}
	}}
	var runs int
	pay := func(context.Context) (string, error) { runs++; return "paid", nil }

	var got1 string
	var err1 error
	go func() {
		defer close(winnerDone)
		got1, err1 = Step(context.WithValue(ctx, drvKey{}, 1), w, "r", "pay", pay)
	}()
	<-d1At
	got2, err2 := Step(context.WithValue(ctx, drvKey{}, 2), w, "r", "pay", pay)
	<-winnerDone

	if err1 != nil || got1 != "paid" || runs != 1 {
		t.Fatalf("the winner got %q, %v, and the step ran %d times; want paid, nil, once (WinnerNeverHalts)", got1, err1, runs)
	}
	var halt *OutcomeUnknown
	if err2 != nil && !errors.As(err2, &halt) {
		t.Fatalf("the loser got %v, want the value or a halt", err2)
	}
	if err2 == nil && got2 != "paid" {
		t.Fatalf("the loser got %q, want paid", got2)
	}
}

// A loser that finds the winner's call of the step in flight, through a Durable wrapper, and sees
// it fail, halts with HaltContended, as through the Journal: the winner was live after the claim
// and owns the effect.
func TestLoserJoinsFailedFlight_HaltContendedThroughWrapper(t *testing.T) {
	ctx := context.Background()
	inBody := make(chan struct{})
	release := make(chan struct{})
	joined := make(chan struct{})
	var once sync.Once
	h := func(k flightKey) {
		if k.name == "pay" {
			once.Do(func() { close(joined) })
		}
	}
	flightJoinHook.Store(&h)
	t.Cleanup(func() { flightJoinHook.Store(nil) })
	j, err := NewJournal(NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	w := &doHookDurable{Durable: j}
	winnerDone := make(chan error, 1)
	go func() {
		_, err := Step(ctx, w, "r", "pay", func(context.Context) (string, error) {
			close(inBody)
			<-release
			return "", errors.New("provider down")
		})
		winnerDone <- err
	}()
	<-inBody
	go func() {
		select { // release the winner once the loser waits on its call, or it never will
		case <-joined:
		case <-ctx.Done():
		}
		close(release)
	}()
	_, err2 := Step(ctx, w, "r", "pay", func(context.Context) (string, error) { return "", nil })
	<-winnerDone
	var halt *OutcomeUnknown
	if !errors.As(err2, &halt) || halt.Cause != HaltContended {
		t.Fatalf("the loser got %v, want a halt with cause HaltContended", err2)
	}
}
