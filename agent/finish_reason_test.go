package agent

import (
	"context"
	"errors"
	"testing"
)

// finishStream is a model turn of text ending with the given finish reason.
func finishStream(text, reason string) *Stream {
	ch := make(chan Emit, 2)
	ch <- Emit{Event: TextDelta{Text: text}}
	ch <- Emit{Event: Finish{Reason: reason}}
	close(ch)
	return NewStream(ch)
}

// The finish reason says whether the turn is the model's whole answer. A turn cut off at the
// token limit or stopped by a filter is not, so it is an error rather than a message: recorded as
// a final answer, it would end the run with half an answer, for good.
func TestStream_FinishReasonDecidesTheTurn(t *testing.T) {
	for reason, wantErr := range map[string]error{
		"":           nil, // a Model that does not report a reason
		"stop":       nil,
		"tool_use":   nil,
		"length":     ErrOutputTruncated,
		"filtered":   ErrOutputFiltered,
		"end_turn":   ErrStreamProtocol, // a provider's own word: adapters map it, the core does not guess
		"pause_turn": ErrStreamProtocol,
	} {
		msg, _, err := finishStream("The total is", reason).Message()
		switch {
		case wantErr == nil && err != nil:
			t.Errorf("reason %q: err = %v, want the message", reason, err)
		case wantErr != nil && !errors.Is(err, wantErr):
			t.Errorf("reason %q: got %q, %v; want an error wrapping %v", reason, msg.Text(), err, wantErr)
		}
	}
}

// Truncated tool-call arguments stay ErrTruncatedToolArgs whatever the reason says, so a caller
// that classifies that error keeps working.
func TestStream_TruncatedArgsWinOverTheReason(t *testing.T) {
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "cut", Name: "lookup", ArgsFragment: []byte(`{"q":`)}}
	ch <- Emit{Event: Finish{Reason: "length"}}
	close(ch)
	if _, _, err := NewStream(ch).Message(); !errors.Is(err, ErrTruncatedToolArgs) {
		t.Fatalf("err = %v, want ErrTruncatedToolArgs", err)
	}
}

// reasonModel answers every call with text and the given finish reason.
type reasonModel struct{ reason string }

func (m reasonModel) Stream(context.Context, Request) (*Stream, error) {
	return finishStream("The total is", m.reason), nil
}

// A run whose final turn was cut off does not complete: nothing is journaled for the turn, the
// run is not marked complete, and a later run asks the model again.
func TestRun_CutOffFinalTurnDoesNotComplete(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{"length", "filtered"} {
		store := NewMemStore()
		msg, err := New(reasonModel{reason}, store).Run(ctx, "r", "sum it")
		if err == nil {
			t.Errorf("reason %q: Run returned %q with no error", reason, msg.Text())
		}
		if done, _ := IsComplete(ctx, store, "r"); done {
			t.Errorf("reason %q: the run was marked complete", reason)
		}
		if recs, _ := store.History(ctx, "r"); len(recs) != 0 {
			t.Errorf("reason %q: journal has %d records, want none", reason, len(recs))
		}
	}
}
