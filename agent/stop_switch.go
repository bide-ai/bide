package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// StopCause says why a run was held by a stop (see Stopped).
type StopCause string

const (
	// StopCauseStop: an active stop covers the run, the tool or the agent.
	StopCauseStop StopCause = "stop"
	// StopCauseUnreadable: no stop is known, and the control run could not be read within the
	// staleness bound, so a side-effect call is held (fail closed).
	StopCauseUnreadable StopCause = "unreadable"
	// StopCauseUnknowable: the stops cannot be judged (StopState.UnknowableErr), so a side-effect call
	// a stop could cover is held (fail closed).
	StopCauseUnknowable StopCause = "unknowable"
)

// Stopped is returned by a run that a kill switch held (see StopSwitch): a pause, not a failure. The
// run's journal is intact, no attempt marker of a call that did not start stands (a claim that was won
// is recorded as not started), and the run is driven again, with RootRunID, once the stop is lifted.
// A call that had already begun when the stop was read finishes and is journaled.
type Stopped struct {
	RunRef
	Cause StopCause
	// StopID, Scope, Reason, By and Since describe the covering stop (StopCauseStop only).
	StopID string
	Scope  StopScope
	Reason string
	By     string
	Since  string
	// Detail says what could not be read or judged (the other causes).
	Detail string
}

// Error implements error.
func (e *Stopped) Error() string {
	switch e.Cause {
	case StopCauseStop:
		return fmt.Sprintf("run %s held by stop %.12s (%s) set by %s at %s: %s", e.RunID, e.StopID, e.Scope, e.By, e.Since, e.Reason)
	case StopCauseUnreadable:
		return fmt.Sprintf("run %s held: the kill switch's control run could not be read and the last read is too old: %s", e.RunID, e.Detail)
	}
	return fmt.Sprintf("run %s held: the stops cannot be judged: %s", e.RunID, e.Detail)
}

func (*Stopped) pause() {}

// StopSwitchOption configures NewStopSwitch.
type StopSwitchOption interface{ applySwitch(*StopSwitch) error }

type switchOption func(*StopSwitch) error

func (f switchOption) applySwitch(s *StopSwitch) error { return f(s) }

// WithStopPoll sets how often a StopSwitch reads the control run for the checks that use its cache
// (a drive's start, a turn's start, a call before its claim): the latency bound of a stop for them.
// Default 1s; at least 10ms. A call about to run reads the control run itself, so it does not wait for it.
func WithStopPoll(d time.Duration) StopSwitchOption {
	return switchOption(func(s *StopSwitch) error {
		if d < 10*time.Millisecond {
			return fmt.Errorf("WithStopPoll(%v): at least 10ms: %w", d, ErrConfig)
		}
		s.poll = d
		return nil
	})
}

// WithStopStaleness sets how old the last successful read may be for a side-effect call to run when
// the control run cannot be read now: past it the call is held (StopCauseUnreadable). Default 15s;
// zero holds a call on any failed read. A read-only call and a model turn are never held for it.
func WithStopStaleness(d time.Duration) StopSwitchOption {
	return switchOption(func(s *StopSwitch) error {
		if d < 0 {
			return fmt.Errorf("WithStopStaleness(%v): negative: %w", d, ErrConfig)
		}
		s.maxStale = d
		return nil
	})
}

// WithStopErrors sets the function a StopSwitch hands a failed read of the control run, and the alerts
// of a control that cannot judge the stops (StopState.ChainErr, Unverified in a basic control). It may be
// called from several goroutines.
func WithStopErrors(fn func(error)) StopSwitchOption {
	return switchOption(func(s *StopSwitch) error { s.onError = fn; return nil })
}

// StopSwitch is what an Agent consults (WithStopSwitch): a StopControl and the records it has read.
// A read is incremental, since only entries after the last one are loaded, and the fold runs over the
// records read so far. One switch may serve many agents of a process.
//
// A check is one of two kinds. A cached check (a drive's start, a turn's start, a call before its
// claim) uses the last read if it is younger than the poll interval, else reads. A synchronous check
// (a call after its claim, a call before it begins) always reads: a stop committed before the read is
// seen, so the bound for a side effect is the store's own visibility (on Postgres the control run must
// be read from the primary). When a read fails, a side-effect call runs only on a state read within the
// staleness bound, else it is held; a read-only call and a model turn use whatever was last read.
type StopSwitch struct {
	ctl      *StopControl
	poll     time.Duration
	maxStale time.Duration
	onError  func(error)

	mu     sync.Mutex
	recs   []Record
	after  int64 // Seq of the last entry read, -1 before the first read
	st     StopState
	have   bool      // st is a fold of recs
	lastOK time.Time // when the last read succeeded (monotonic)
}

// NewStopSwitch returns a switch over ctl.
func NewStopSwitch(ctl *StopControl, opts ...StopSwitchOption) (*StopSwitch, error) {
	if ctl == nil {
		return nil, fmt.Errorf("NewStopSwitch: nil control: %w", ErrConfig)
	}
	s := &StopSwitch{ctl: ctl, poll: time.Second, maxStale: 15 * time.Second, after: -1}
	for _, o := range opts {
		if o == nil {
			return nil, fmt.Errorf("NewStopSwitch: nil option: %w", ErrConfig)
		}
		if err := o.applySwitch(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// refresh reads the control run's entries after the last one and folds. The caller holds s.mu.
func (s *StopSwitch) refresh(ctx context.Context) error {
	j := s.ctl.j
	n := len(s.recs)
	first := s.after < 0
	for e, err := range j.store.Load(ctx, StopControlRun, s.after) {
		if err != nil {
			return storageErr("load the stop control run", err)
		}
		if first {
			first = false
			if err := j.checkFirst(StopControlRun, e); err != nil {
				return err
			}
		}
		rec, err := decodeStored(StopControlRun, e.Name, e.Data)
		if err != nil {
			return err
		}
		s.recs = append(s.recs, rec)
		s.after = e.Seq
	}
	if len(s.recs) != n || !s.have {
		st, err := s.ctl.stateOf(s.recs)
		if err != nil {
			return err
		}
		s.st, s.have = st, true
		if st.ChainErr != "" && s.onError != nil {
			s.onError(fmt.Errorf("kill switch: %s", st.ChainErr))
		}
	}
	s.lastOK = time.Now()
	return nil
}

// view returns the state to judge by, and whether it is known: a fresh read, or (when the read fails)
// the last state if it is within the staleness bound.
func (s *StopSwitch) view(ctx context.Context, sync bool, sideEffect bool) (StopState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.have && !sync && time.Since(s.lastOK) < s.poll {
		return s.st, true
	}
	err := s.refresh(ctx)
	if err == nil {
		return s.st, true
	}
	if s.onError != nil && !errors.Is(err, context.Canceled) {
		s.onError(fmt.Errorf("kill switch: %w", err))
	}
	switch {
	case !sideEffect:
		return s.st, s.have // the last read, however old; nothing known if there was none
	case s.have && time.Since(s.lastOK) <= s.maxStale:
		return s.st, true
	}
	return StopState{}, false
}

// Hold decides whether q is held, for run ref. sideEffect says the thing asked about can have a side
// effect (a tool that is not read-only), sync that the check must read the control run. It returns nil
// when nothing holds it.
func (s *StopSwitch) Hold(ctx context.Context, ref RunRef, q StopQuery, sideEffect, sync bool) *Stopped {
	st, known := s.view(ctx, sync, sideEffect)
	if !known {
		return &Stopped{RunRef: ref, Cause: StopCauseUnreadable, Detail: "the control run could not be read and the last read is older than the staleness bound"}
	}
	if e, ok := st.Covering(q); ok {
		return &Stopped{RunRef: ref, Cause: StopCauseStop, StopID: e.ID, Scope: e.Scope, Reason: e.Reason, By: e.By, Since: e.At}
	}
	if err := st.UnknowableErr(); err != nil && sideEffect && !q.Compensation {
		return &Stopped{RunRef: ref, Cause: StopCauseUnknowable, Detail: err.Error()}
	}
	return nil
}

// GlobalStop returns the active global stop, if there is one, from a cached read (a read when it is older than
// the poll interval): what a recovery pass asks, so that it lists nothing while everything is stopped.
func (s *StopSwitch) GlobalStop(ctx context.Context) (StopEntry, bool) {
	st, known := s.view(ctx, false, false)
	if !known {
		return StopEntry{}, false
	}
	return st.Covering(StopQuery{})
}
