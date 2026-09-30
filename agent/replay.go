package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ===========================================================================
// Replay: deterministic re-emission of a journaled run
// ===========================================================================

// Replay returns a Model that re-emits the model outputs recorded in `source` for runID,
// in order, instead of calling a live LLM. Run an agent with it against a FRESH store to
// deterministically re-execute a past run:
//
//   - time-travel debugging: step through exactly what happened, offline and free;
//   - regression tests: capture a production run, replay it in CI (pairs with
//     testing/synctest), assert behavior didn't drift;
//   - evals over real traffic: the journal IS a golden dataset.
//
// Because the journal captures every model output across the whole (possibly nested)
// tree, the replay is exact. Spend is replayed too: a turn reports the usage the original turn
// discarded (failed attempts, losing hedge targets) along with its own, and a model call that
// failed for good in the original fails again at the same point, reporting the same usage, so
// the replayed run's Result.Spend, journal and WithTokenBudget stops match the original's. A
// middleware in the replaying agent that retries such a failure moves on to the next recorded
// response instead. This is only possible because the durable substrate records
// a complete, replayable history in the first place.
func Replay(ctx context.Context, source Durable, runID string) (Model, error) {
	recs, err := source.History(ctx, runID)
	if err != nil {
		return nil, err
	}
	var turns []recordedTurn
	for _, r := range recs {
		switch {
		case r.Kind == StepModel && r.Message != nil:
			t := recordedTurn{msg: *r.Message}
			if r.Usage != nil {
				t.usage = *r.Usage
			}
			if r.DiscardedUsage != nil {
				t.discarded = *r.DiscardedUsage
			}
			turns = append(turns, t)
		case strings.HasPrefix(r.Name, spendStepPrefix) && r.DiscardedUsage != nil:
			turns = append(turns, recordedTurn{usage: *r.DiscardedUsage, failed: true})
		}
	}
	return &replayModel{turns: turns}, nil
}

// recordedTurn is one journaled model call: its message, the usage it reported, and the usage
// of the requests its turn discarded. A failed one is a model call that failed for good, with the
// usage its requests reported.
type recordedTurn struct {
	msg              Message
	usage, discarded Usage
	failed           bool
}

// errReplayedFailure fails a replayed model call where the original call failed.
var errReplayedFailure = errors.New("replay: the recorded model call failed here")

type replayModel struct {
	turns []recordedTurn
	i     int
}

func (m *replayModel) Stream(ctx context.Context, _ Request) (*Stream, error) {
	if m.i >= len(m.turns) {
		return nil, fmt.Errorf("replay: %w", ErrNoRecordedOutput)
	}
	t := m.turns[m.i]
	m.i++

	if t.failed {
		// The usage, then the failure: the stream reports what the original call spent.
		ch := make(chan Emit, 2)
		ch <- Emit{Event: Finish{Reason: "stop", Usage: t.usage}}
		ch <- Emit{Err: errReplayedFailure}
		close(ch)
		return NewStream(ch), nil
	}
	if t.discarded != (Usage{}) {
		// The original turn's other requests were billed too: report them to the run as spend.
		for _, h := range modelHooks(ctx) {
			if h.After != nil {
				h.After(t.discarded)
			}
		}
	}
	evs := emitsFor(t.msg, t.usage)
	ch := make(chan Emit, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

// emitsFor converts an assistant Message and the call's usage back into the stream events
// that would have produced them (the inverse of msgBuilder). Every message msgBuilder produces round-trips.
// A message it cannot produce may not: an unsigned thinking block stays open until a
// redacted block or the end of the stream, so one followed by text or another thinking
// block merges with it.
//
// The Finish carries u and a reason derived from the message: "tool_use" when it has tool
// calls, else "stop". The provider's own reason (such as a length cutoff) is not journaled:
// a ModelHandler returns only the message and usage, and middleware may answer with a
// message no stream produced.
func emitsFor(msg Message, u Usage) []Emit {
	var out []Emit
	idx := 0
	for _, p := range msg.Parts {
		switch v := p.(type) {
		case Reasoning:
			if v.Redacted != "" {
				out = append(out, Emit{Event: ReasoningDelta{Redacted: v.Redacted}})
				continue
			}
			// A block with no text and no signature still opens a thinking block.
			if v.Text != "" || v.Signature == "" {
				out = append(out, Emit{Event: ReasoningDelta{Text: v.Text}})
			}
			if v.Signature != "" {
				out = append(out, Emit{Event: ReasoningDelta{Signature: v.Signature}})
			}
		case Text:
			out = append(out, Emit{Event: TextDelta{Text: v.Text}})
		case ToolUse:
			out = append(out, Emit{Event: ToolCallDelta{Index: idx, ID: v.ID, Name: v.Name, ArgsFragment: v.Args, Signature: v.Signature}})
			idx++
		}
	}
	reason := FinishStop
	if idx > 0 {
		reason = FinishToolUse
	}
	return append(out, Emit{Event: Finish{Reason: reason, Usage: u}})
}
