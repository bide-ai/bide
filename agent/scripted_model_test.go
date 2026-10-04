package agent

// The scripted model, as package agenttest has it, for this package's own tests (which cannot import
// agenttest, which imports agent).

import (
	"context"
	"encoding/json"
)

// ScriptedModel is a deterministic, LLM-free Model that replays a fixed script of turns,
// one turn per model call. It is the batteries-included version of the model fakes people
// otherwise reverse-engineer from ToolCallDelta + Finish for tests, examples, and local
// dev, so you can drive the whole agent loop offline.
//
// It is replay-safe: it selects the turn to emit by counting the assistant messages
// already in the request, not by an internal cursor. On a durable resume the agent loop
// replays journaled assistant turns without calling the model, so a cursor would drift;
// counting the transcript instead lines the script back up with the live turn every time,
// including after a crash and re-run. Once the script is exhausted it repeats its last
// turn, so an over-long conversation does not panic.
//
// Build turns with ToolTurn, TextTurn, and ErrorTurn:
//
// A ScriptedModel is safe for the sequential agent loop; it is not intended for concurrent
// Stream calls from multiple runs at once.
type ScriptedModel struct {
	turns []ScriptedTurn
}

// ScriptedTurn is one scripted model output. Construct one with ToolTurn, TextTurn, or
// ErrorTurn rather than building it directly.
type ScriptedTurn struct {
	toolID   string
	toolName string
	args     json.RawMessage
	text     string
	err      error
}

// NewScriptedModel builds a ScriptedModel that replays turns in order, one per model call.
func NewScriptedModel(turns ...ScriptedTurn) *ScriptedModel {
	return &ScriptedModel{turns: turns}
}

// ToolTurn scripts a turn in which the model calls one tool: id is the tool-use ID the
// journal keys the call under, name is the tool, and args is its JSON arguments.
func ToolTurn(id, name, args string) ScriptedTurn {
	return ScriptedTurn{toolID: id, toolName: name, args: json.RawMessage(args)}
}

// TextTurn scripts a turn in which the model emits a final text answer, ending the run.
func TextTurn(text string) ScriptedTurn {
	return ScriptedTurn{text: text}
}

// ErrorTurn scripts a turn that fails the model call with err, so a test can exercise the
// loop's error path (a mid-run "crash" before the next step is journaled, say).
func ErrorTurn(err error) ScriptedTurn {
	return ScriptedTurn{err: err}
}

// Stream implements Model. It counts the assistant turns already in the request to pick
// the script entry to emit, so replay after a durable resume lines up with the live turn.
func (m *ScriptedModel) Stream(_ context.Context, req Request) (*Stream, error) {
	idx := 0
	for _, msg := range req.Messages {
		if msg.Role == RoleAssistant {
			idx++
		}
	}
	if idx >= len(m.turns) {
		idx = len(m.turns) - 1 // exhausted: repeat the last turn rather than panic
	}
	t := m.turns[idx]

	if t.err != nil {
		ch := make(chan Emit, 1)
		ch <- Emit{Err: t.err}
		close(ch)
		return NewStream(ch), nil
	}

	ch := make(chan Emit, 2)
	if t.toolName != "" {
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: t.toolID, Name: t.toolName, ArgsFragment: t.args}}
		ch <- Emit{Event: Finish{Reason: "tool_use"}}
	} else {
		ch <- Emit{Event: TextDelta{Text: t.text}}
		ch <- Emit{Event: Finish{Reason: "stop"}}
	}
	close(ch)
	return NewStream(ch), nil
}
