// replay.go holds Replay, which re-emits a journaled run's recorded model outputs as a Model.

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
// tree, the replay is exact. Each turn ends with the Finish its record holds: the reason and the
// provider's raw reason the original turn journaled (a record written before they were journaled
// gets the reason its message implies, "tool_use" when it calls tools, else "stop"), and the
// replayed turn's record names the model its original record named (Record.Model), not the
// replaying Model. Spend is replayed too: a turn reports the usage the original turn discarded
// (failed attempts, losing hedge targets, and requests journaled in a late spend record after
// it) in its Finish's Discarded, beside its own Usage, and a model call that
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
			t := recordedTurn{msg: *r.Message, reason: r.Finish, raw: r.RawFinish, model: r.Model}
			if r.Usage != nil {
				t.usage = *r.Usage
			}
			if r.DiscardedUsage != nil {
				t.discarded = *r.DiscardedUsage
			}
			turns = append(turns, t)
		case strings.HasPrefix(r.Name, spendStepPrefix) && r.DiscardedUsage != nil:
			turns = append(turns, recordedTurn{usage: *r.DiscardedUsage, failed: true})
		case strings.HasPrefix(r.Name, lateSpendPrefix) && r.DiscardedUsage != nil && len(turns) > 0:
			// Requests of earlier turns that ended late: the turn before reports their spend, so the
			// replayed run's spend matches (in that turn's record rather than a record of its own).
			if last := &turns[len(turns)-1]; last.failed {
				addUsage(&last.usage, *r.DiscardedUsage)
			} else {
				addUsage(&last.discarded, *r.DiscardedUsage)
			}
		}
	}
	return &replayModel{turns: turns}, nil
}

// recordedTurn is one journaled model call: its message, the usage it reported, and the usage
// of the requests its turn discarded. A failed one is a model call that failed for good, with the
// usage its requests reported.
type recordedTurn struct {
	msg              Message
	reason           FinishReason
	raw              string
	model            *ModelInfo
	usage, discarded Usage
	failed           bool
}

// errReplayedFailure fails a replayed model call where the original call failed.
var errReplayedFailure = errors.New("replay: the recorded model call failed here")

type replayModel struct {
	turns []recordedTurn
	i     int
}

func (m *replayModel) Stream(context.Context, Request) (*Stream, error) {
	if m.i >= len(m.turns) {
		return nil, fmt.Errorf("replay: %w", ErrNoRecordedOutput)
	}
	t := m.turns[m.i]
	m.i++

	if t.failed {
		// The usage, then the failure: the stream reports what the original call spent.
		ch := make(chan Emit, 2)
		ch <- Emit{Event: Finish{Reason: FinishStop, Usage: t.usage}}
		ch <- Emit{Err: errReplayedFailure}
		close(ch)
		return replayed(NewStream(ch), nil), nil
	}
	// The original turn's other requests were billed too: Discarded reports them to the run as
	// spend.
	evs := emitsFor(t.msg, Finish{Reason: t.reason, Raw: t.raw, Usage: t.usage, Discarded: t.discarded})
	ch := make(chan Emit, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return replayed(NewStream(ch), t.model), nil
}

// replayed marks s as a replayed turn's stream whose record names model m (nil for none).
func replayed(s *Stream, m *ModelInfo) *Stream {
	s.replayed, s.recorded = true, m
	return s
}

// emitsFor converts an assistant Message and the Finish that ended it back into the stream events
// that would have produced them (the inverse of msgBuilder). Every message msgBuilder produces round-trips.
// A message it cannot produce may not: an unsigned thinking block stays open until a
// redacted block or the end of the stream, so one followed by text or another thinking
// block merges with it.
//
// The stream ends with fin. An empty fin.Reason (a record written before the reason was journaled)
// becomes the reason the message implies: "tool_use" when it has tool calls, else "stop".
func emitsFor(msg Message, fin Finish) []Emit {
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
	if fin.Reason == "" {
		fin.Reason = FinishStop
		if idx > 0 {
			fin.Reason = FinishToolUse
		}
	}
	return append(out, Emit{Event: fin})
}
