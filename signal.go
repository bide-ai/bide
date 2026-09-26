package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// Awaiting is returned by Run when a tool called Await and no signal has been delivered for
// that name yet. The run has paused durably at the await point. Deliver a signal with
// Signal (same name), then re-invoke Run with the same runID to continue. Awaiting is the
// externally-pushed dual of Interrupted: Interrupt asks a human and resumes with their
// answer; Await waits for an event an outside system delivers.
type Awaiting struct {
	RunID  string
	Name   string
	Prompt any // optional caller payload describing what the run is waiting for
}

func (e *Awaiting) Error() string {
	return fmt.Sprintf("run %s awaiting signal %q", e.RunID, e.Name)
}

// Await blocks the current run until a single-shot signal named `name` is delivered, then
// returns its payload. Call it from inside a tool (the agent loop supplies the run context).
// On first encounter, with no signal recorded, it returns the zero T and an *Awaiting error
// that propagates out of Run, pausing the run durably. After Signal records a payload for
// the same name and Run is re-invoked, Await returns that payload and execution continues
// past this point. The payload survives a crash: it is a journaled step.
//
// Await is the externally-pushed counterpart of Interrupt. Like Interrupt and Sleep it must
// be called from a retry-safe tool (Safety.ReadOnly or Idempotent): on resume the tool
// re-runs from the top until the await resolves, so everything before the Await call must be
// safe to repeat. Use distinct names for distinct awaits; each pauses and resolves
// independently.
func Await[T any](ctx context.Context, name string) (T, error) {
	var zero T
	d, runID, ok := runContext(ctx)
	if !ok {
		return zero, fmt.Errorf("agent: Await called outside a running agent: %w", ErrConfig)
	}
	recs, err := d.History(ctx, runID)
	if err != nil {
		return zero, fmt.Errorf("agent: await %q: %w (%w)", name, err, ErrStorage)
	}
	step := signalStep(name)
	for _, r := range recs {
		if r.Kind == StepSignal && r.Name == step {
			var v T
			if len(r.Result) > 0 {
				if err := json.Unmarshal(r.Result, &v); err != nil {
					return zero, fmt.Errorf("agent: decode signal %q: %w (%w)", name, err, ErrProtocol)
				}
			}
			return v, nil
		}
	}
	return zero, &Awaiting{RunID: runID, Name: name}
}

// Signal delivers a single-shot signal to a run, journaled at-most-once by name: a
// redelivery (a retried webhook, an at-least-once queue) is a no-op and the first payload
// wins. This turns at-least-once transport into exactly-once application to the run. Safe to
// call from any process; the store's primary-key / ON CONFLICT is the cross-process dedup.
//
// Signal only records the payload. After delivering, re-invoke Run with the same runID to
// resume the awaiting run: directly, or via a Waker scheduled at the current time.
func Signal[T any](ctx context.Context, d Durable, runID, name string, payload T) error {
	if runID == "" {
		return fmt.Errorf("Signal: empty runID: %w", ErrConfig)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("agent: encode signal %q: %w (%w)", name, err, ErrConfig)
	}
	_, err = d.Do(ctx, runID, signalStep(name), func(context.Context) (Record, error) {
		return Record{Kind: StepSignal, Result: b}, nil
	})
	return err
}

func signalStep(name string) string { return "signal:" + name }
