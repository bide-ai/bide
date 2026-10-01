// runapi.go holds the Run API (docs/design/api-v1.md, item 1) under its transitional names:
// RunMessage, ResumeRun, StreamMessage and RunTypedMessage. The 1.0 rewrite renames them Run,
// Resume, Stream and RunTyped, and removes the string entry points they replace.

package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ValidateRunID reports whether id may name a root run (RunMessage, ResumeRun, StreamMessage,
// RunTypedMessage): it is not empty and holds no '>', which the engine reserves for the run IDs of
// sub-agents and session turns. Any other string is a valid run ID: the journal's keys encode
// whatever they are given. A run ID that fails is ErrConfig, and the run entry points return it
// with a nil Result.
func ValidateRunID(id string) error {
	if id == "" {
		return fmt.Errorf("run: empty runID: %w", ErrConfig)
	}
	if strings.Contains(id, subRunSep) {
		return fmt.Errorf("run: run ID %q contains %q, which the engine reserves for the run IDs of sub-agents and session turns: %w", id, subRunSep, ErrConfig)
	}
	return nil
}

// driveSpec is what one drive's caller asked for: the input it answers (nil: the journaled one),
// the transcript a session turn is seeded with, what drives the run, and the run options it
// passed.
type driveSpec struct {
	input   *Message
	seed    []Message // a session turn's transcript before its input; nil otherwise
	kind    RunKind
	session *SessionRef
	typed   *TypedStart
	cfg     runConfig
	resume  bool // ResumeRun or a recovery drive: a run with no run:start is ErrNotStarted
	emit    func(AgentEvent)
}

// RunMessage drives the agent to the end of runID's run, answering input under opts, and returns
// the run's Result. The first drive of a run journals input and opts in run:start, and every later
// drive runs under them (see RunStart): resume an unfinished run with RunMessage and the same
// input (or ResumeRun, which needs neither), and a finished run returns its recorded end,
// whatever it is passed.
//
// The Result is non-nil whenever runID is valid (see ValidateRunID), whatever the error: a pause
// (*ApprovalPending, *InterruptPending, ...), a halt (*OutcomeUnknown), a failure, a saga's
// *SagaAborted, or ErrRunCancelled. Its Usage and Spend are the whole run's, sub-agents included;
// Turns and Duration are this call's. Message is the final answer, zero unless err is nil.
//
// Deprecated: transitional; renamed by the 1.0 rewrite. RunMessage becomes Run, and the current
// Run(ctx, runID, input string) is removed.
func (a *Agent) RunMessage(ctx context.Context, runID string, input Message, opts ...RunOption) (*Result, error) {
	return a.runEntry(ctx, runID, &driveSpec{input: &input}, opts)
}

// ResumeRun drives runID's run on from its journal: the input, saga flag and options its first
// drive journaled (see RunStart), and the last limit amendment. opts may raise or lower its limits
// (WithMaxTurns, WithTokenBudget: journaled as an amendment) and set the deployment's values
// (WithWaker, WithClock, WithMaxConcurrency, the identity's Actor); any other setting that
// differs from the journaled one is ErrConfig. A run with no run:start is ErrNotStarted. A typed
// run is resumed with RunTypedMessage or ResumeTyped, and a session turn by its session.
//
// Deprecated: transitional; renamed by the 1.0 rewrite. ResumeRun becomes Resume.
func (a *Agent) ResumeRun(ctx context.Context, runID string, opts ...RunOption) (*Result, error) {
	return a.runEntry(ctx, runID, &driveSpec{resume: true}, opts)
}

// runEntry is the body of the root run entry points: it validates runID, applies opts and drives
// the run, and returns a Result for every outcome but an invalid runID.
func (a *Agent) runEntry(ctx context.Context, runID string, d *driveSpec, opts []RunOption) (*Result, error) {
	if err := ValidateRunID(runID); err != nil {
		return nil, err
	}
	start := time.Now()
	res := &Result{RunID: runID}
	if err := applyOptions("run", &d.cfg, opts, RunOption.applyRun); err != nil {
		res.Duration = time.Since(start)
		return res, err
	}
	msg, tot, turns, err := a.drive(ctx, runID, d)
	res.Usage, res.Spend, res.Turns, res.Duration = tot.answer, tot.spend, turns, time.Since(start)
	if err == nil {
		res.Message = msg
	}
	return res, err
}

// drive runs one drive of runID for d: a saga's (d.cfg.saga, or a run journaled as one) through
// the saga path, any other through run.
func (a *Agent) drive(ctx context.Context, runID string, d *driveSpec) (Message, usageTotals, int, error) {
	if d.resume && d.input == nil {
		st, ok, err := RecordedStart(ctx, a.store, runID)
		if err != nil {
			return Message{}, usageTotals{}, 0, err
		}
		if !ok {
			return Message{}, usageTotals{}, 0, fmt.Errorf("run %s: %w", runID, ErrNotStarted)
		}
		in := st.Input
		d.input = &in
		if st.Saga {
			d.cfg.saga = true
		}
	}
	if d.cfg.saga {
		return a.runSagaWithTelemetry(ctx, runID, d.input.Text(), d.emit)
	}
	return a.run(ctx, runID, append(append([]Message(nil), d.seed...), *d.input), false, d.emit)
}

// StreamMessage drives the agent like RunMessage but returns a live AgentStream: token deltas,
// turn boundaries and tool start and finish arrive as events while the durable loop runs, and
// AgentStream.Result returns what RunMessage would.
//
// Deprecated: transitional; renamed by the 1.0 rewrite. StreamMessage becomes Stream, and the
// current Stream(ctx, runID, input string) is removed.
func (a *Agent) StreamMessage(ctx context.Context, runID string, input Message, opts ...RunOption) *AgentStream {
	return a.streamEntry(ctx, runID, &driveSpec{input: &input}, opts)
}

// ResumeAgent returns the Resumer that drives a with the runs it can: runs of kind agent that are
// not typed.
func ResumeAgent(a *Agent, opts ...RunOption) Resumer {
	return func(ctx context.Context, runID string, start RunStart) error { return errP14NotBuilt }
}

// ResumeTyped returns the Resumer that drives a's typed runs whose answer type is T.
func ResumeTyped[T any](a *Agent, opts ...RunOption) Resumer {
	return func(ctx context.Context, runID string, start RunStart) error { return errP14NotBuilt }
}
