package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
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
//
// Redacted holds reasoning the provider returned encrypted (Anthropic redacted_thinking
// data), which must also go back unchanged; Text and Signature are empty when it is set.
// Each thinking block is its own Reasoning part, in the order the model produced them.
type Reasoning struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
	Redacted  string `json:"redacted,omitempty"`
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
	if b, ok := spliceType(inner, kind); ok {
		return b, nil
	}
	return tagPart(inner, kind)
}

// tagPart returns inner, the journal encoding of a part's struct, with a "type" member naming
// kind added: the members decoded into a map, and the map encoded, which writes them sorted by
// name.
func tagPart(inner []byte, kind string) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(inner, &fields); err != nil {
		return nil, err
	}
	fields["type"], _ = json.Marshal(kind)
	return marshalJournal(fields)
}

// partMember is one member of an encoded JSON object: its name, and the member's bytes from the
// name's opening quote to the end of its value.
type partMember struct{ name, raw []byte }

// spliceType returns the bytes tagPart returns for inner and kind, without decoding inner, and
// reports whether it could. It can when inner is what marshalJournal writes for a part's struct: a
// compact object whose member names are distinct, need no escaping, are plain ASCII and do not
// include "type". Its members are then written back verbatim, with "type" added, sorted by name;
// that is what tagPart's map encoding writes, since the encoder keeps a compact value from
// marshalJournal byte for byte (see EncodeRecord) and sorts a map's names as bytes, which for
// ASCII names is the order it uses. Anything else reports false, and marshalPart takes tagPart.
func spliceType(inner []byte, kind string) ([]byte, bool) {
	n := len(inner)
	if n < 2 || inner[0] != '{' || inner[n-1] != '}' {
		return nil, false
	}
	var buf [8]partMember
	members := buf[:0]
	for i := 1; i < n-1; {
		if inner[i] != '"' {
			return nil, false
		}
		j := i + 1
		for ; j < n-1 && inner[j] != '"'; j++ {
			if c := inner[j]; c == '\\' || c < 0x20 || c >= 0x80 {
				return nil, false
			}
		}
		name := inner[i+1 : j]
		if j+1 >= n-1 || inner[j+1] != ':' || string(name) == "type" {
			return nil, false
		}
		for _, m := range members {
			if string(m.name) == string(name) {
				return nil, false
			}
		}
		end, ok := compactValueEnd(inner[:n-1], j+2)
		if !ok {
			return nil, false
		}
		members = append(members, partMember{name: name, raw: inner[i:end]})
		if end == n-1 {
			break
		}
		if inner[end] != ',' || end+1 == n-1 {
			return nil, false
		}
		i = end + 1
	}
	typ := []byte(`"type":"` + kind + `"`)
	members = append(members, partMember{name: typ[1:5], raw: typ})
	slices.SortFunc(members, func(a, b partMember) int { return bytes.Compare(a.name, b.name) })
	size := 1
	for _, m := range members {
		size += len(m.raw) + 1
	}
	out := make([]byte, 0, size)
	out = append(out, '{')
	for k, m := range members {
		if k > 0 {
			out = append(out, ',')
		}
		out = append(out, m.raw...)
	}
	return append(out, '}'), true
}

// compactValueEnd returns the index just past the JSON value that starts at b[i], where b is the
// inside of a compact object that the value is a member of (the object without its closing
// brace): past its closing quote or bracket, or at the ',' or the end of b that ends a number or
// literal. It reports false if no value starts at i, or a bracket in it is unmatched.
func compactValueEnd(b []byte, i int) (int, bool) {
	var stack [16]byte
	open := stack[:0] // the closing bracket each open array or object expects, innermost last
	inString := false
	for k := i; k < len(b); k++ {
		c := b[k]
		if inString {
			switch c {
			case '\\':
				k++
			case '"':
				inString = false
				if len(open) == 0 {
					return k + 1, true
				}
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			open = append(open, '}')
		case '[':
			open = append(open, ']')
		case '}', ']':
			if len(open) == 0 || open[len(open)-1] != c {
				return 0, false
			}
			open = open[:len(open)-1]
			if len(open) == 0 {
				return k + 1, true
			}
		case ',':
			if len(open) == 0 {
				return k, k > i
			}
		}
	}
	return len(b), len(open) == 0 && !inString && len(b) > i
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
