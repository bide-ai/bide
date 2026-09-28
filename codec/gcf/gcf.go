// Package gcf provides an agent.ToolResultCodec that encodes tool results as
// GCF (Graph Compact Format) instead of JSON when they are sent to the model.
// GCF is more token-efficient and better comprehended by frontier models than
// JSON for structured data, so this can cut prompt tokens on tool output. It is
// opt-in per model (model adapters default to JSON), and it changes only what
// the model reads: bide's journal and audit trail keep the canonical JSON.
package gcf

import (
	"encoding/json"

	"github.com/bide-ai/bide/agent"
	gcfgo "github.com/blackwell-systems/gcf-go"
)

// Codec renders a tool result's JSON bytes as a GCF document.
type Codec struct{}

var _ agent.ToolResultCodec = Codec{}

// New returns a GCF tool-result codec. Wire it into a model adapter with that
// adapter's WithToolResultCodec option, for example:
//
//	openai.New(key, openai.WithToolResultCodec(gcf.New()))
func New() agent.ToolResultCodec { return Codec{} }

// EncodeToolResult decodes the canonical JSON into a generic value and encodes
// it as GCF. Empty input yields an empty string. A decode error is returned so
// the caller can fall back to JSON (agent.EncodeToolResultOr does this).
func (Codec) EncodeToolResult(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	return gcfgo.EncodeGenericChecked(v)
}
