package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// maxEchoedError bounds the error text a model's output can produce: the model-supplied part is
// cut to maxErrorBody bytes, and the rest of the message is short.
const maxEchoedError = maxErrorBody + 1024

// Tool-call arguments that are not valid JSON are the model's own output, of any size. The error
// that reports them must not carry them in full into the caller's logs.
func TestTruncatedToolArgs_ErrorIsBounded(t *testing.T) {
	huge := `{"q":"` + strings.Repeat("a", 1<<20) // never closed
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: []byte(huge)}}
	ch <- Emit{Event: Finish{Reason: "tool_use"}}
	close(ch)
	_, _, err := NewStream(ch).Message()
	if !errors.Is(err, ErrTruncatedToolArgs) {
		t.Fatalf("err = %v, want ErrTruncatedToolArgs", err)
	}
	if n := len(err.Error()); n > maxEchoedError {
		t.Fatalf("error text is %d bytes, want at most %d", n, maxEchoedError)
	}
}

// The same holds for the tool name, which the model also chooses.
func TestTruncatedToolArgs_ErrorBoundsTheName(t *testing.T) {
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "c1", Name: strings.Repeat("n", 1<<20), ArgsFragment: []byte(`{`)}}
	ch <- Emit{Event: Finish{Reason: "tool_use"}}
	close(ch)
	_, _, err := NewStream(ch).Message()
	if !errors.Is(err, ErrTruncatedToolArgs) {
		t.Fatalf("err = %v, want ErrTruncatedToolArgs", err)
	}
	if n := len(err.Error()); n > maxEchoedError {
		t.Fatalf("error text is %d bytes, want at most %d", n, maxEchoedError)
	}
}

// A model that calls a tool by a name no tool has gets ErrUnknownTool, naming what it asked for,
// but not all of a name of any length.
func TestUnknownTool_ErrorIsBounded(t *testing.T) {
	m := NewScriptedModel(ToolTurn("c1", strings.Repeat("x", 1<<20), `{}`), TextTurn("done"))
	_, err := mustNew(m, memJournal()).Run(context.Background(), "run", "go")
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
	if n := len(err.Error()); n > maxEchoedError {
		t.Fatalf("error text is %d bytes, want at most %d", n, maxEchoedError)
	}
}

// A tool middleware can hand the base handler a name no tool has; that error is journaled as the
// call's result, so it is bounded too.
func TestToolHandler_UnknownToolErrorIsBounded(t *testing.T) {
	a := mustNew(NewScriptedModel(TextTurn("x")), memJournal())
	_, _, err := a.toolHandler("r")(context.Background(), ToolUse{ID: "c1", Name: strings.Repeat("y", 1<<20)})
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
	if n := len(err.Error()); n > maxEchoedError {
		t.Fatalf("error text is %d bytes, want at most %d", n, maxEchoedError)
	}
}
