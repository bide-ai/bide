package agent

import (
	"context"
	"fmt"
)

// Replay returns a Model that re-emits the model outputs recorded in `source` for runID,
// in order — instead of calling a live LLM. Run an agent with it against a FRESH store to
// deterministically re-execute a past run:
//
//   - time-travel debugging — step through exactly what happened, offline and free;
//   - regression tests — capture a production run, replay it in CI (pairs with
//     testing/synctest), assert behavior didn't drift;
//   - evals over real traffic — the journal IS a golden dataset.
//
// Because the journal captures every model output across the whole (possibly nested)
// tree, the replay is exact. This is only possible because the durable substrate records
// a complete, replayable history in the first place.
func Replay(ctx context.Context, source Durable, runID string) (Model, error) {
	recs, err := source.History(ctx, runID)
	if err != nil {
		return nil, err
	}
	var msgs []Message
	for _, r := range recs {
		if r.Kind == StepModel && r.Message != nil {
			msgs = append(msgs, *r.Message)
		}
	}
	return &replayModel{msgs: msgs}, nil
}

type replayModel struct {
	msgs []Message
	i    int
}

func (m *replayModel) Stream(_ context.Context, _ Request) (*Stream, error) {
	if m.i >= len(m.msgs) {
		return nil, fmt.Errorf("replay: %w", ErrNoRecordedOutput)
	}
	msg := m.msgs[m.i]
	m.i++

	evs := emitsFor(msg)
	ch := make(chan Emit, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

// emitsFor converts an assistant Message back into the stream events that would have
// produced it (the inverse of msgBuilder).
func emitsFor(msg Message) []Emit {
	var out []Emit
	idx := 0
	for _, p := range msg.Parts {
		switch v := p.(type) {
		case Reasoning:
			if v.Text != "" {
				out = append(out, Emit{Event: ReasoningDelta{Text: v.Text}})
			}
			if v.Signature != "" {
				out = append(out, Emit{Event: ReasoningDelta{Signature: v.Signature}})
			}
		case Text:
			out = append(out, Emit{Event: TextDelta{Text: v.Text}})
		case ToolUse:
			out = append(out, Emit{Event: ToolCallDelta{Index: idx, ID: v.ID, Name: v.Name, ArgsFragment: v.Args}})
			idx++
		}
	}
	return append(out, Emit{Event: Finish{Reason: "stop"}})
}
