package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Text returns the concatenation of the message's Text parts (ignoring reasoning, tool
// calls, and tool results). Empty if the message carries no text.
func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if t, ok := p.(Text); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// Role identifies the author of a Message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn. Its content is a sequence of typed Parts (NOT a flat string),
// so reasoning/thinking parts (which Anthropic requires echoed back across turns),
// tool calls, tool results, and image inputs all survive round-trips intact. This is
// the provider-neutral type; adapters translate to/from provider wire formats.
type Message struct {
	Role  Role
	Parts []Part
}

// Part is a single piece of message content. Concrete parts: Text, Reasoning,
// ToolUse, ToolResult, Image.
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

// ToolUse is the model's request to call a tool (assistant turn). Signature is an opaque
// provider token bound to this call that must be sent back with it on later turns (a Gemini
// thoughtSignature); it is journaled with the call, and adapters that have no such token
// leave it empty and ignore it.
type ToolUse struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args,omitempty"`
	Signature string          `json:"signature,omitempty"`
}

func (ToolUse) part() {}

// ToolResult carries a tool's output back to the model (tool turn).
type ToolResult struct {
	ToolUseID string          `json:"tool_use_id"`
	Result    json.RawMessage `json:"result,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func (ToolResult) part() {}

// Image is a provider-neutral image input. Exactly one of Data or URL is set (Data XOR
// URL). For raw bytes, set Data and Mime (e.g. "image/png"); the adapter base64-encodes
// it (Anthropic base64 source / OpenAI data URI). For a hosted image, set URL and leave
// Data nil; Mime is not required for URLs. This is an input-only part: models emit text,
// reasoning, and tool calls, never images, so nothing produces an Image on the response
// path.
type Image struct {
	Mime string `json:"mime,omitempty"` // MIME type for Data (required when Data is set), e.g. "image/jpeg"; ignored for URL
	Data []byte `json:"data,omitempty"` // raw image bytes; adapters base64-encode. Mutually exclusive with URL.
	URL  string `json:"url,omitempty"`  // hosted image URL. Mutually exclusive with Data.
}

func (Image) part() {}

// UserText and SystemText are convenience constructors.
func UserText(s string) Message   { return Message{Role: RoleUser, Parts: []Part{Text{s}}} }
func SystemText(s string) Message { return Message{Role: RoleSystem, Parts: []Part{Text{s}}} }

// UserParts builds a user message from mixed parts, e.g. a prompt and one or more
// images: UserParts(Text{"what is this?"}, ImageData("image/png", raw)). It is the
// multimodal counterpart to UserText.
func UserParts(parts ...Part) Message { return Message{Role: RoleUser, Parts: parts} }

// ImageData builds an Image part from raw bytes with the given MIME type; the adapter
// base64-encodes the bytes onto the wire.
func ImageData(mime string, data []byte) Image { return Image{Mime: mime, Data: data} }

// ImageURL builds an Image part referencing a hosted image by URL.
func ImageURL(url string) Image { return Image{URL: url} }

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
	return marshalJournal(w)
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
	case Image:
		return "image", nil
	default:
		return "", fmt.Errorf("agent: unknown part type %T (%w)", p, ErrProtocol)
	}
}

func marshalPart(p Part) ([]byte, error) {
	kind, err := partKind(p)
	if err != nil {
		return nil, err
	}
	inner, err := marshalJournal(p)
	if err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(inner, &fields); err != nil {
		return nil, err
	}
	fields["type"], _ = json.Marshal(kind)
	return marshalJournal(fields)
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
	case "image":
		var v Image
		return v, json.Unmarshal(raw, &v)
	default:
		return nil, fmt.Errorf("agent: unknown part type %q (%w)", probe.Type, ErrProtocol)
	}
}
