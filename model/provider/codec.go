package provider

import "encoding/json"

// ToolResultCodec renders a tool result's canonical JSON bytes into the string
// content that is sent to the model. The journal always stores the JSON form
// (see agent.Record.Result), so this only changes what the model reads: an adapter
// can hand the model a more token-efficient or better-comprehended encoding
// (for example GCF via github.com/bide-ai/bide/codec/gcf) without touching the
// durable record or the audit trail. Implementations must be pure and
// deterministic.
type ToolResultCodec interface {
	EncodeToolResult(raw json.RawMessage) (string, error)
}

// JSONToolResultCodec is the default codec: it passes the canonical JSON
// through unchanged. It is equivalent to what an adapter does when no
// ToolResultCodec is configured.
type JSONToolResultCodec struct{}

// EncodeToolResult returns the JSON bytes as a string, unchanged.
func (JSONToolResultCodec) EncodeToolResult(raw json.RawMessage) (string, error) {
	return string(raw), nil
}

// EncodeToolResultOr applies the codec to raw and returns the result. When the
// codec is nil it returns the canonical JSON string (the default behavior).
// When the codec returns an error it degrades to the same JSON string rather
// than failing the whole model request: the JSON form is always a correct
// payload, so a codec hiccup never drops a tool result. Adapters call this at
// the point where a ToolResult is serialized into a provider request.
func EncodeToolResultOr(c ToolResultCodec, raw json.RawMessage) string {
	if c == nil {
		return string(raw)
	}
	s, err := c.EncodeToolResult(raw)
	if err != nil {
		return string(raw)
	}
	return s
}
