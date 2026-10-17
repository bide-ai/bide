package agent

import (
	"context"
	"errors"
	"strings"
	"time"
)

// errCovBlip is the error covGetFail and covInsertFail inject.
var errCovBlip = errors.New("injected store fault")

// covGetFail fails every Get of an entry whose name contains sub.
type covGetFail struct {
	Store
	sub string
}

func (f covGetFail) Unwrap() Store { return f.Store }
func (f covGetFail) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if strings.Contains(name, f.sub) {
		return Entry{}, false, errCovBlip
	}
	return f.Store.Get(ctx, runID, name)
}

// covInsertFail fails, uncommitted, every Insert of an entry whose name contains sub.
type covInsertFail struct {
	Store
	sub string
}

func (f covInsertFail) Unwrap() Store { return f.Store }
func (f covInsertFail) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if strings.Contains(name, f.sub) {
		return Entry{}, false, errCovBlip
	}
	return f.Store.Insert(ctx, runID, name, data)
}

// The test helpers of package agenttest, which this package's own tests cannot import (agenttest
// imports agent).

// memJournal returns a Journal over a new, empty MemStore.
func memJournal() *Journal { return mustJournal(NewMemStore()) }

// mustJournal returns NewJournal(s), and panics if that fails.
func mustJournal(s Store) *Journal { return must(NewJournal(s)) }

// mustNew returns New(model, j, opts...), and panics if it fails.
func mustNew(model Model, j *Journal, opts ...Option) *Agent { return must(New(model, j, opts...)) }

// hasValueStep reports whether runID's journal holds a StepValue record named name.
func hasValueStep(ctx context.Context, j *Journal, runID, name string) (bool, error) {
	r, ok, err := j.Get(ctx, runID, name)
	return ok && r.Kind == StepValue, err
}

// answerOf returns res's Message and err, as agenttest.Answer does.
func answerOf(res *Result, err error) (Message, error) {
	if res == nil {
		return Message{}, err
	}
	return res.Message, err
}

// SetExploreFlightHooks installs the two hooks the concurrent claim exploration
// (claim_explore_conc_test.go) schedules around, and returns a function that restores the hooks
// it replaced.
//
// join is called by a caller that joins another caller's step call in flight, before it waits,
// with a function that reports whether the joined call has returned (its flight is gone from the
// process's flights). decode is called at every record decode; the owner of a call in flight
// decodes the call's outcome right after the call returns, before it does anything else.
func SetExploreFlightHooks(join func(returned func() bool), decode func()) (restore func()) {
	jh := func(k flightKey) {
		flights.mu.Lock()
		f := flights.m[k]
		flights.mu.Unlock()
		returned := func() bool {
			if f == nil {
				return true
			}
			flights.mu.Lock()
			defer flights.mu.Unlock()
			return flights.m[k] != f
		}
		join(returned)
	}
	dh := func([]byte) { decode() }
	oldJ := flightJoinHook.Swap(&jh)
	oldD := decodeHook.Swap(&dh)
	return func() {
		flightJoinHook.Store(oldJ)
		decodeHook.Store(oldD)
	}
}

// SessionMuHeld reports whether s's mutex is held: the session tests check, from a compensator, that
// a cancelled saga turn's rollback runs without it. It is a test hook: TryLock fails while any
// goroutine holds the mutex, so it is meaningful only while no other caller uses s.
func SessionMuHeld(s *Session) bool {
	if s.mu.TryLock() {
		s.mu.Unlock()
		return false
	}
	return true
}

// testDecidedAt is the approver's decision time the tests sign.
var testDecidedAt = time.Date(2026, 10, 13, 12, 14, 3, 123_000_000, time.UTC)
