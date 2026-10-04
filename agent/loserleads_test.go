package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
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

// claimHookStore is a Store wrapper (audit.AuditedStore's shape: it passes run IDs and names
// through and implements Unwrap) whose Insert of a name calls inserted once the inner Insert
// returns. The Insert of a step's attempt marker is its claim, so a hook on it runs when a driver
// has just won (or lost) the claim and is about to send the step.
type claimHookStore struct {
	Store
	inserted func(ctx context.Context, name string)
}

func (w *claimHookStore) Unwrap() Store { return w.Store }

func (w *claimHookStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	e, ok, err := w.Store.Insert(ctx, runID, name, data)
	if w.inserted != nil {
		w.inserted(ctx, name)
	}
	return e, ok, err
}

// The claim model's regress/loser-leads schedule (spec/tla/claims/regress/loser-leads.cfg,
// WinnerNeverHalts), through a store wrapper: two drivers of one Step in one process. d1 wins
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
	var atOnce sync.Once
	w, err := NewJournal(&claimHookStore{Store: st, inserted: func(ctx context.Context, name string) {
		if name == stepAttemptStep("pay") && drvOf(ctx) == 1 {
			atOnce.Do(func() {
				close(d1At)
				<-d2Reading
			})
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	var runs int
	pay := func(context.Context) (string, error) { runs++; return "paid", nil }

	var got1 string
	var err1 error
	go func() {
		defer close(winnerDone)
		got1, err1 = w.Step(context.WithValue(ctx, drvKey{}, 1), "r", "pay", pay)
	}()
	<-d1At
	got2, err2 := w.Step(context.WithValue(ctx, drvKey{}, 2), "r", "pay", pay)
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

// A loser that finds the winner's call of the step in flight, through a store wrapper, and sees
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
	w, err := NewJournal(&claimHookStore{Store: NewMemStore()})
	if err != nil {
		t.Fatal(err)
	}
	winnerDone := make(chan error, 1)
	go func() {
		_, err := w.Step(ctx, "r", "pay", func(context.Context) (string, error) {
			close(inBody)
			<-release
			return "", errors.New("provider down")
		})
		winnerDone <- err
	}()
	<-inBody
	go func() {
		select { // release the winner once the loser waits on its call (or, failing that, in the end)
		case <-joined:
		case <-time.After(10 * time.Second):
		}
		close(release)
	}()
	_, err2 := w.Step(ctx, "r", "pay", func(context.Context) (string, error) { return "", nil })
	<-winnerDone
	var halt *OutcomeUnknown
	if !errors.As(err2, &halt) || halt.Cause != HaltContended {
		t.Fatalf("the loser got %v, want a halt with cause HaltContended", err2)
	}
}

// The same rule when the loser's read finds nothing at once: the loser halts without starting a
// call of the step, so a winner that sends the step afterwards leads its own call. Were the loser
// to go on and read through a call in flight (the historical LoserLeads), the winner would join it.
func TestLoserLeads_LoserStartsNoCallAfterItsRead(t *testing.T) {
	ctx := context.Background()
	d1At := make(chan struct{})
	d2InFlight := make(chan struct{}) // d2 reads the step inside a call in flight
	d2Done := make(chan struct{})
	joined := make(chan struct{})
	winnerDone := make(chan struct{})
	var once sync.Once
	h := func(k flightKey) {
		if k.name == "pay" {
			once.Do(func() { close(joined) })
		}
	}
	flightJoinHook.Store(&h)
	t.Cleanup(func() { flightJoinHook.Store(nil) })
	var mu sync.Mutex
	reads := 0
	st := &getHookStore{Store: NewMemStore(), get: func(ctx context.Context, name string) {
		if name != "pay" || drvOf(ctx) != 2 {
			return
		}
		mu.Lock()
		reads++
		n := reads
		mu.Unlock()
		if n == 2 { // a second read of the step by the loser: a call in flight it leads
			close(d2InFlight)
			select {
			case <-joined:
			case <-winnerDone:
			}
		}
	}}
	var atOnce sync.Once
	w, err := NewJournal(&claimHookStore{Store: st, inserted: func(ctx context.Context, name string) {
		if name == stepAttemptStep("pay") && drvOf(ctx) == 1 {
			atOnce.Do(func() {
				close(d1At)
				select {
				case <-d2InFlight:
				case <-d2Done:
				}
			})
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	var runs int
	pay := func(context.Context) (string, error) { runs++; return "paid", nil }
	var got1 string
	var err1 error
	go func() {
		defer close(winnerDone)
		got1, err1 = w.Step(context.WithValue(ctx, drvKey{}, 1), "r", "pay", pay)
	}()
	<-d1At
	_, _ = w.Step(context.WithValue(ctx, drvKey{}, 2), "r", "pay", pay)
	close(d2Done)
	<-winnerDone
	if err1 != nil || got1 != "paid" || runs != 1 {
		t.Fatalf("the winner got %q, %v, and the step ran %d times; want paid, nil, once (WinnerNeverHalts)", got1, err1, runs)
	}
}
