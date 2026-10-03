package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
)

// callStream is a model turn that calls tool name with id and ends with the given reason.
func callStream(id, name string, reason FinishReason) *Stream {
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: id, Name: name, ArgsFragment: json.RawMessage(`{}`)}}
	ch <- Emit{Event: Finish{Reason: reason}}
	close(ch)
	return NewStream(ch)
}

// A turn whose reason says the model stopped to call tools, but that carries no call, lost the
// calls it was for: it is not an answer, however complete its text looks.
func TestStream_ToolUseReasonWithoutCallsIsNotAnAnswer(t *testing.T) {
	msg, _, err := finishStream("Let me look that up.", FinishToolUse).Message()
	if !errors.Is(err, ErrStreamProtocol) {
		t.Fatalf("got %q, %v; want ErrStreamProtocol", msg.Text(), err)
	}
}

// stopCallModel calls "lookup" on its first turn but reports the natural-stop reason (as OpenAI
// does under a forced tool_choice), then answers.
type stopCallModel struct{ n atomic.Int32 }

func (m *stopCallModel) Stream(context.Context, Request) (*Stream, error) {
	if m.n.Add(1) == 1 {
		return callStream("c1", "lookup", FinishStop), nil
	}
	return finishStream("done", FinishStop), nil
}

// Whether the loop runs tools is decided by the turn's content, never by its finish reason: a
// turn that calls a tool and reports "stop" still runs the call.
func TestRun_ToolCallsRunWhateverTheReason(t *testing.T) {
	var calls atomic.Int32
	tool := Func("lookup", "", Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "found", nil
	})
	final, err := mustNew(&stopCallModel{}, memJournal(), WithTools(tool)).Run(context.Background(), "r", "go")
	if err != nil || final.Text() != "done" || calls.Load() != 1 {
		t.Fatalf("final %q, err %v, tool ran %d times; want done, nil, 1", final.Text(), err, calls.Load())
	}
}
