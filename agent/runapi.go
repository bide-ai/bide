// runapi.go holds the Run API (docs/design/api-v1.md, item 1) under its transitional names:
// Run, Resume, Stream and RunTyped. The 1.0 rewrite renames them Run,
// Resume, Stream and RunTyped, and removes the string entry points they replace.

package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ValidateRunID reports whether id may name a root run (Run, Resume, Stream,
// RunTyped): it is not empty and holds no '>', which the engine reserves for the run IDs of
// sub-agents and session turns. Any other string is a valid run ID: the journal's keys encode
// whatever they are given. A run ID that fails is ErrConfig, and the run entry points return it
// with a nil Result. (From a tool call's own context they also accept the programmatic sub-run IDs
// RunInfo.SubRunFor derives.)
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
	resume  bool // Resume or a recovery drive: a run with no run:start is ErrNotStarted
	// strictSaga is set by a session turn's and a sub-agent's drive that is not a saga's: a run
	// journaled as a saga is ErrConfig through them rather than driven as one (a root run's drive
	// with no WithSaga drives a saga run as the saga it is).
	strictSaga bool
	emit       func(RunEvent)
}

// runKind is what drives the run d drives.
func (d *driveSpec) runKind() RunKind {
	if d.kind == "" {
		return RunKindAgent
	}
	return d.kind
}

// Run drives the agent to the end of runID's run, answering input under opts, and returns
// the run's Result. The first drive of a run journals input and opts in run:start, and every later
// drive runs under them (see RunStart): resume an unfinished run with Run and the same
// input (or Resume, which needs neither), and a finished run returns its recorded end,
// whatever it is passed.
//
// The Result is non-nil whenever runID is valid (see ValidateRunID), whatever the error: a pause
// (*ApprovalPending, *InterruptPending, ...), a halt (*OutcomeUnknown), a failure, a saga's
// *SagaAborted, or ErrRunCancelled. Its Usage and Spend are the whole run's, sub-agents included;
// Turns and Duration are this call's. Message is the final answer, zero unless err is nil.
func (a *Agent) Run(ctx context.Context, runID string, input Message, opts ...RunOption) (*Result, error) {
	return a.runEntry(ctx, runID, &driveSpec{input: &input}, opts)
}

// Resume drives runID's run on from its journal: the input, saga flag and options its first
// drive journaled (see RunStart), and the last limit amendment. opts may raise or lower its limits
// (WithMaxTurns, WithTokenBudget: journaled as an amendment) and set the deployment's values
// (WithWaker, WithClock, WithMaxConcurrency, the identity's Actor); any other setting that
// differs from the journaled one is ErrConfig. A run with no run:start is ErrNotStarted. A typed
// run is resumed with RunTyped or ResumeTyped, and a session turn by its session.
func (a *Agent) Resume(ctx context.Context, runID string, opts ...RunOption) (*Result, error) {
	return a.runEntry(ctx, runID, &driveSpec{resume: true}, opts)
}

// runEntry is the body of the root run entry points: it validates runID, applies opts and drives
// the run, and returns a Result for every outcome but an invalid runID.
func (a *Agent) runEntry(ctx context.Context, runID string, d *driveSpec, opts []RunOption) (*Result, error) {
	if err := checkRunID(ctx, runID); err != nil {
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
	if !d.cfg.saga {
		msg, tot, turns, err := a.run(ctx, runID, d)
		if err != errSagaRun {
			return msg, tot, turns, err
		}
		d.cfg.saga = true // the run is journaled as a saga, and the drive passed no saga option
	}
	return a.runSagaWithTelemetry(ctx, runID, d)
}

// Stream drives the agent like Run but returns a live RunStream: token deltas,
// turn boundaries and tool start and finish arrive as events while the durable loop runs, and
// RunStream.Result returns what Run would.
func (a *Agent) Stream(ctx context.Context, runID string, input Message, opts ...RunOption) *RunStream {
	return a.streamEntry(ctx, runID, &driveSpec{input: &input}, opts)
}

// ResumeAgent returns the Resumer that drives a's runs: those of kind agent that are not typed,
// sagas included (the run's journal says which). It drives each under the options its run:start
// journaled (Resume's semantics), with opts setting only the deployment's values: WithWaker,
// WithClock, WithMaxConcurrency and an identity's Actor. A journaled setting in opts (a limit, the
// system prompt, sampling, tool choice, a tool filter, saga, an output mode, a principal) is
// ErrConfig on every call, since a recovery drive must not change a run's options. Any other run
// is ErrNotResumable: a typed run (ResumeTyped), a flow's (plan), and a session turn's, which only
// its session drives (it is seeded with the session's transcript, and only the session records the
// turn), so Recover never hands it one. So is a run whose run:start records no kind and no typed
// start (one an earlier version journaled): the record does not say whether a plain run or a typed
// one started it, so ResumeAgent does not guess; a deployment that knows drives it with a Resumer
// of its own (after ResumeAgent in ResumeAny, say).
func ResumeAgent(a *Agent, opts ...RunOption) Resumer {
	return func(ctx context.Context, runID string, start RunStart) error {
		if start.legacy() {
			return fmt.Errorf("run %s was started by an earlier version, whose run:start does not say whether the run is typed; drive it with a Resumer of your own: %w", runID, ErrNotResumable)
		}
		if k := start.kind(); k != RunKindAgent {
			return fmt.Errorf("run %s is of kind %q, which ResumeAgent does not drive: %w", runID, k, ErrNotResumable)
		}
		if start.Typed != nil {
			return fmt.Errorf("run %s is a typed run; resume it with ResumeTyped and its answer type: %w", runID, ErrNotResumable)
		}
		d, err := resumeDrive(runID, start, opts)
		if err != nil {
			return err
		}
		_, _, _, err = a.drive(ctx, runID, d)
		return err
	}
}

// ResumeTyped returns the Resumer that drives a's typed runs whose answer type is T: the runs of
// kind agent whose run:start journals T's schema digest, in their journaled output mode. opts are
// as for ResumeAgent. Any other run is ErrNotResumable.
func ResumeTyped[T any](a *Agent, opts ...RunOption) Resumer {
	return func(ctx context.Context, runID string, start RunStart) error {
		if start.kind() != RunKindAgent || start.Typed == nil {
			return fmt.Errorf("run %s is not a typed agent run: %w", runID, ErrNotResumable)
		}
		_, ts, err := typedAgent[T](a, start.Typed.Mode)
		if err != nil {
			return err
		}
		if ts.SchemaDigest != start.Typed.SchemaDigest {
			var zero T
			return fmt.Errorf("run %s's answer type is not %T (its schema digest differs): %w", runID, zero, ErrNotResumable)
		}
		d, err := resumeDrive(runID, start, opts)
		if err != nil {
			return err
		}
		_, _, err = runTyped[T](ctx, a, runID, d, start.Typed.Mode, nil)
		return err
	}
}

// resumeDrive is a recovery drive of runID, started as start, under opts: the run's journaled
// input and options, and only the deployment's values from opts (rule 9: a recovery drive passes no
// per-run option).
func resumeDrive(runID string, start RunStart, opts []RunOption) (*driveSpec, error) {
	d := &driveSpec{resume: true, input: &start.Input}
	if err := applyOptions("resume", &d.cfg, opts, RunOption.applyRun); err != nil {
		return nil, err
	}
	c := d.cfg
	if c.maxTurns != nil || c.tokenBudget != nil || c.systemPrompt != nil || c.sampling != nil || c.toolChoice != nil ||
		c.tools != nil || c.saga || c.outputMode != "" || c.identity != nil && (c.identity.OnBehalfOf != "" || c.identity.AuthorityRef != "") {
		return nil, fmt.Errorf("run %s: a recovery Resumer takes only the deployment's options (WithWaker, WithClock, WithMaxConcurrency, an identity's Actor); a run's own options are journaled: %w", runID, ErrConfig)
	}
	d.cfg.saga = start.Saga
	return d, nil
}
