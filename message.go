package agent

import (
	"encoding/json"
	"fmt"
)

// Role identifies the author of a Message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn. Its content is a sequence of typed Parts — NOT a flat string —
// so reasoning/thinking parts (which Anthropic requires echoed back across turns),
// tool calls, tool results, and images all survive round-trips intact. This is the
// provider-neutral type; adapters translate to/from provider wire formats.
type Message struct {
	Role  Role
	Parts []Part
}

// Part is a single piece of message content. Concrete parts: Text, Reasoning,
// ToolUse, ToolResult (Image etc. later).
type Part interface{ part() }

// Text is plain assistant/user text.
type Text struct {
	Text string `json:"text"`
}

func (Text) part() {}

// Reasoning is model thinking. Signature is the provider's opaque token that must be
// echoed back on later turns (Anthropic extended thinking); dropping it corrupts the
// conversation — which is exactly why Content can't be a flat string.
type Reasoning struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

func (Reasoning) part() {}

// ToolUse is the model's request to call a tool (assistant turn).
type ToolUse struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

func (ToolUse) part() {}

// ToolResult carries a tool's output back to the model (tool turn).
type ToolResult struct {
	ToolUseID string          `json:"tool_use_id"`
	Result    json.RawMessage `json:"result,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func (ToolResult) part() {}

// UserText and SystemText are convenience constructors.
func UserText(s string) Message   { return Message{Role: RoleUser, Parts: []Part{Text{s}}} }
func SystemText(s string) Message { return Message{Role: RoleSystem, Parts: []Part{Text{s}}} }

// toolUses returns the ToolUse parts of a message.
func (m Message) toolUses() []ToolUse {
	var out []ToolUse
	for _, p := range m.Parts {
		if tu, ok := p.(ToolUse); ok {
			out = append(out, tu)
		}
	}
	return out
}

// --- JSON round-tripping ---
//
// Part is an interface, so encoding/json can marshal it but cannot unmarshal back into
// it. Any durable Store (SQLite, Postgres, file) needs Parts to round-trip, so Message
// carries a "type"-tagged wire form. (Eino solves the same problem with
// RegisterSerializableType; we bake the tag in so there's nothing to register.)

type messageWire struct {
	Role  Role              `json:"role"`
	Parts []json.RawMessage `json:"parts,omitempty"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	w := messageWire{Role: m.Role}
	for _, p := range m.Parts {
		b, err := marshalPart(p)
		if err != nil {
			return nil, err
		}
		w.Parts = append(w.Parts, b)
	}
	return json.Marshal(w)
}

func (m *Message) UnmarshalJSON(b []byte) error {
	var w messageWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	m.Role = w.Role
	m.Parts = nil
	for _, raw := range w.Parts {
		p, err := unmarshalPart(raw)
		if err != nil {
			return err
		}
		m.Parts = append(m.Parts, p)
	}
	return nil
}

func partKind(p Part) (string, error) {
	switch p.(type) {
	case Text:
		return "text", nil
	case Reasoning:
		return "reasoning", nil
	case ToolUse:
		return "tool_use", nil
	case ToolResult:
		return "tool_result", nil
	default:
		return "", fmt.Errorf("agent: unknown part type %T (%w)", p, ErrProtocol)
	}
}

func marshalPart(p Part) ([]byte, error) {
	kind, err := partKind(p)
	if err != nil {
		return nil, err
	}
	inner, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(inner, &fields); err != nil {
		return nil, err
	}
	fields["type"], _ = json.Marshal(kind)
	return json.Marshal(fields)
}

func unmarshalPart(raw []byte) (Part, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	switch probe.Type {
	case "text":
		var v Text
		return v, json.Unmarshal(raw, &v)
	case "reasoning":
		var v Reasoning
		return v, json.Unmarshal(raw, &v)
	case "tool_use":
		var v ToolUse
		return v, json.Unmarshal(raw, &v)
	case "tool_result":
		var v ToolResult
		return v, json.Unmarshal(raw, &v)
	default:
		return nil, fmt.Errorf("agent: unknown part type %q (%w)", probe.Type, ErrProtocol)
	}
}
