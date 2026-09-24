package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// Interrupted is returned by Run when a tool called Interrupt and no resume value has
// been recorded for that key yet. The run has paused durably at the interrupt point.
// Inspect Prompt to decide what to ask the human, record an answer with Resume (same
// Key), then re-invoke Run with the same runID to continue.
type Interrupted struct {
	RunID  string
	Key    string
	Prompt any // caller-defined payload for the human: a question, options, current state
}

func (e *Interrupted) Error() string {
	return fmt.Sprintf("run %s interrupted at %q awaiting input", e.RunID, e.Key)
}

// Interrupt pauses the current run to request typed human input, identified by key. Call
// it from inside a tool (the agent loop supplies the run context). On first encounter it
// returns the zero T and an *Interrupted error that propagates out of Run, pausing the
// run durably. After Resume records a value for the same key and Run is re-invoked,
// Interrupt returns that value and execution continues past this point. This generalizes
// approve/deny (a bool) to an arbitrary typed answer.
//
// Interrupt must be called from a retry-safe tool (Safety.ReadOnly or Idempotent): on
// resume the tool re-runs from the top until the interrupt resolves, so everything
// before the Interrupt call must be safe to repeat. Use distinct keys for multiple
// interrupt points; each pauses and resumes independently.
func Interrupt[T any](ctx context.Context, key string, prompt any) (T, error) {
	var zero T
	d, runID, ok := runContext(ctx)
	if !ok {
		return zero, fmt.Errorf("agent: Interrupt called outside a running agent: %w", ErrConfig)
	}
	recs, err := d.History(ctx, runID)
	if err != nil {
		return zero, fmt.Errorf("agent: interrupt %q: %w (%w)", key, err, ErrStorage)
	}
	name := interruptStep(key)
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == name {
			var v T
			if len(r.Result) > 0 {
				if err := json.Unmarshal(r.Result, &v); err != nil {
					return zero, fmt.Errorf("agent: decode resume value for %q: %w (%w)", key, err, ErrProtocol)
				}
			}
			return v, nil
		}
	}
	return zero, &Interrupted{RunID: runID, Key: key, Prompt: prompt}
}

// Resume records the typed value a paused run is waiting for at key (see Interrupt), then
// re-invoke Run with the same runID to continue. Idempotent: the first value for a
// (runID, key) wins. The value survives a crash — it is a journaled step.
func Resume[T any](ctx context.Context, d Durable, runID, key string, value T) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("agent: encode resume value for %q: %w (%w)", key, err, ErrConfig)
	}
	_, err = d.Do(ctx, runID, interruptStep(key), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: b}, nil
	})
	return err
}

func interruptStep(key string) string { return "interrupt:" + key }
