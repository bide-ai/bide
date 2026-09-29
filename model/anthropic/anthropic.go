// Package anthropic adapts Anthropic's Messages API to agent.Model. It translates the
// provider-neutral []agent.Message/Part model to Anthropic content blocks (text,
// thinking+signature, tool_use, tool_result), declares tools, opens the SSE stream,
// and normalizes Anthropic's event types into agent's Event taxonomy. Zero external
// deps — net/http + stdlib.
//
// Native Anthropic is exactly what Google ADK Go still lacks (#225/#1097); this is a
// wedge. Streaming tool-call args arrive as input_json_delta fragments — the canonical
// case the agent core's finalize() json.Valid gate handles.
package anthropic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/internal/toolcfg"
)

// Model is an Anthropic Messages API adapter implementing agent.Model.
type Model struct {
	apiKey    string
	model     string
	maxTokens int
	baseURL   string
	http      *http.Client
	cache     bool
	toolCodec agent.ToolResultCodec
}

var _ agent.Model = (*Model)(nil) // port/adapter contract

type Option func(*Model)

func WithModel(id string) Option           { return func(m *Model) { m.model = id } }
func WithMaxTokens(n int) Option           { return func(m *Model) { m.maxTokens = n } }
func WithBaseURL(u string) Option          { return func(m *Model) { m.baseURL = u } }
func WithHTTPClient(c *http.Client) Option { return func(m *Model) { m.http = c } }

// WithPromptCache turns on Anthropic prompt caching: cache_control breakpoints are
// placed on the system prompt and the tool definitions — the large, constant prefix an
// agent loop resends every turn — so repeat turns are billed at the cache-read rate.
// Cache hits/writes surface in agent.Usage (CacheReadTokens / CacheWriteTokens).
func WithPromptCache() Option { return func(m *Model) { m.cache = true } }

// WithToolResultCodec encodes tool results sent to the model with c instead of
// raw JSON (for example GCF, to cut tokens on structured output). The journal
// keeps the JSON form; only what the model reads changes. Default is JSON.
func WithToolResultCodec(c agent.ToolResultCodec) Option { return func(m *Model) { m.toolCodec = c } }

// New constructs an Anthropic model adapter. apiKey is your Anthropic API key.
func New(apiKey string, opts ...Option) *Model {
	m := &Model{
		apiKey:    apiKey,
		model:     "claude-sonnet-4-6",
		maxTokens: 4096,
		baseURL:   "https://api.anthropic.com",
		http:      http.DefaultClient,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Stream implements agent.Model.
func (m *Model) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	body, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", m.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("content-type", "application/json")

	resp, err := m.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, agent.ClassifyHTTPError("anthropic", resp)
	}

	return agent.NewStreamFunc(ctx, func(send func(agent.Emit) bool) { streamSSE(resp.Body, send) }), nil
}

// buildRequest translates the provider-neutral request into an Anthropic Messages
// payload. System turns fold into the top-level `system` field; RoleTool turns become
// user turns carrying tool_result blocks. Consecutive same-role turns are merged into a
// single turn: Anthropic requires messages to alternate between user and assistant, and
// folding out system turns (or mapping RoleTool to user) can otherwise leave two same-role
// turns adjacent. Tool input_schema is the tool's neutral schema used as-is; Anthropic
// accepts the schema/ package's neutral form directly, so no provider dialect transform is
// needed (unlike OpenAI strict mode).
func (m *Model) buildRequest(req agent.Request) ([]byte, error) {
	// Tool names and the tool choice are checked first, so a setup the provider would reject
	// is a config error here rather than a 400.
	sendChoice, err := toolcfg.Check("anthropic", toolcfg.AnthropicName, req)
	if err != nil {
		return nil, err
	}
	type block map[string]any

	var systemTexts []string
	var msgs []map[string]any

	for _, msg := range req.Messages {
		if msg.Role == agent.RoleSystem {
			for _, p := range msg.Parts {
				if t, ok := p.(agent.Text); ok && t.Text != "" {
					systemTexts = append(systemTexts, t.Text)
				}
			}
			continue
		}
		role := "user"
		if msg.Role == agent.RoleAssistant {
			role = "assistant"
		}
		var blocks []block
		for _, p := range msg.Parts {
			switch v := p.(type) {
			case agent.Text:
				if strings.TrimSpace(v.Text) == "" {
					continue // Anthropic rejects a text block with no non-whitespace text
				}
				blocks = append(blocks, block{"type": "text", "text": v.Text})
			case agent.Reasoning:
				// Anthropic takes thinking back only as it issued it: a thinking block with its
				// signature, or a redacted_thinking block's data. Reasoning with neither (from
				// another provider) cannot be verified and is dropped.
				switch {
				case v.Redacted != "":
					blocks = append(blocks, block{"type": "redacted_thinking", "data": v.Redacted})
				case v.Signature != "":
					blocks = append(blocks, block{"type": "thinking", "thinking": v.Text, "signature": v.Signature})
				}
			case agent.ToolUse:
				var input any = json.RawMessage(v.Args)
				if len(v.Args) == 0 {
					input = map[string]any{}
				}
				blocks = append(blocks, block{"type": "tool_use", "id": v.ID, "name": v.Name, "input": input})
			case agent.ToolResult:
				blocks = append(blocks, block{"type": "tool_result", "tool_use_id": v.ToolUseID, "content": agent.EncodeToolResultOr(m.toolCodec, v.Result), "is_error": v.IsError})
			case agent.Image:
				// URL source when URL is set; otherwise a base64 source from the raw bytes.
				var source map[string]any
				if v.URL != "" {
					source = map[string]any{"type": "url", "url": v.URL}
				} else {
					source = map[string]any{
						"type":       "base64",
						"media_type": v.Mime,
						"data":       base64.StdEncoding.EncodeToString(v.Data),
					}
				}
				blocks = append(blocks, block{"type": "image", "source": source})
			}
		}
		if len(blocks) == 0 {
			continue // a turn with no renderable parts would be empty content, which Anthropic rejects
		}
		// Merge into the previous turn when they share a role, rather than emitting an
		// illegal same-role adjacency; Anthropic wants strict user/assistant alternation.
		if n := len(msgs); n > 0 && msgs[n-1]["role"] == role {
			msgs[n-1]["content"] = append(msgs[n-1]["content"].([]block), blocks...)
			continue
		}
		msgs = append(msgs, map[string]any{"role": role, "content": blocks})
	}

	var tools []map[string]any
	for _, t := range req.Tools {
		var input any = map[string]any{"type": "object"}
		if s := t.ArgsSchema(); len(s) > 0 {
			input = json.RawMessage(s)
		}
		tools = append(tools, map[string]any{
			"name":         t.Name(),
			"description":  t.Description(),
			"input_schema": input,
		})
	}
	// Cache breakpoint on the last tool: Anthropic caches every block up to and including
	// the marked one, so this caches the whole (constant) tool-definition prefix.
	if m.cache && len(tools) > 0 {
		tools[len(tools)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
	}

	maxTokens := m.maxTokens // request-level Sampling overrides the construction default
	if s := req.Sampling.MaxTokens; s != nil {
		maxTokens = *s
	}
	payload := map[string]any{
		"model":      m.model,
		"max_tokens": maxTokens, // Anthropic requires max_tokens
		"stream":     true,
		"messages":   msgs,
	}
	if s := req.Sampling.Temperature; s != nil {
		payload["temperature"] = *s
	}
	if s := req.Sampling.TopP; s != nil {
		payload["top_p"] = *s
	}
	if len(req.Sampling.Stop) > 0 {
		payload["stop_sequences"] = req.Sampling.Stop
	}
	// Anthropic has no seed parameter; req.Sampling.Seed is intentionally ignored.
	// System text from several parts or turns is joined with a blank line, as separate
	// paragraphs, rather than run together.
	system := strings.Join(systemTexts, "\n\n")
	if system != "" {
		if m.cache {
			// A structured system block lets us attach a cache breakpoint to it.
			payload["system"] = []map[string]any{{
				"type":          "text",
				"text":          system,
				"cache_control": map[string]any{"type": "ephemeral"},
			}}
		} else {
			payload["system"] = system
		}
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	// tool_choice: {"type":"auto"} / {"type":"any"} (for "required") / {"type":"tool","name":...}
	// / {"type":"none"}. "none" keeps the tool declarations: Anthropic requires them whenever
	// the history holds tool_use blocks.
	if tc := req.ToolChoice; tc != nil && sendChoice {
		switch tc.Mode {
		case "", "auto":
			payload["tool_choice"] = map[string]any{"type": "auto"}
		case "required":
			payload["tool_choice"] = map[string]any{"type": "any"}
		case "tool":
			payload["tool_choice"] = map[string]any{"type": "tool", "name": tc.Name}
		case "none":
			payload["tool_choice"] = map[string]any{"type": "none"}
		}
	}
	return json.Marshal(payload)
}

// sseEvent is the union of Anthropic streaming event payloads we care about.
type sseEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Data string `json:"data"` // redacted_thinking: the encrypted reasoning
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Message *struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// streamSSE reads Anthropic's SSE stream and pushes normalized agent events. It closes
// the body. Package-internal so it's unit-testable without a network round-trip.
//
// The turn ends at message_stop, with one Finish carrying the stop reason and usage from the
// last message_delta (Anthropic may send more than one; its usage is cumulative). The content
// is complete once message_delta arrives: from then on a content event (a block's start, delta,
// or stop) or a second message_start is agent.ErrStreamProtocol, not more of the answer, while
// ping, a further message_delta, and event types this adapter does not know are allowed. Nothing
// after message_stop is read. A message_stop with no message_delta before it is
// agent.ErrStreamProtocol; a stream that ends before message_stop sends no Finish, so the
// consumer sees agent.ErrIncompleteResponse.
func streamSSE(body io.ReadCloser, send func(agent.Emit) bool) {
	defer body.Close()

	sc := agent.NewSSEScanner(body)

	var in, out, cacheRead, cacheWrite int
	var reason string
	var delta bool // a message_delta has arrived: the content is complete
	for sc.Scan() {
		data, ok := agent.SSEPayload(sc.Text())
		if !ok {
			continue // ignore `event:` lines and blank separators; dispatch on the JSON's type
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			send(agent.Emit{Err: fmt.Errorf("anthropic sse decode: %w (%w)", err, agent.ErrModel)})
			return
		}
		switch ev.Type {
		case "message_start", "content_block_start", "content_block_delta", "content_block_stop":
			if delta {
				send(agent.Emit{Err: fmt.Errorf("anthropic: %s after message_delta: %w", ev.Type, agent.ErrStreamProtocol)})
				return
			}
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				in = ev.Message.Usage.InputTokens
				cacheRead = ev.Message.Usage.CacheReadInputTokens
				cacheWrite = ev.Message.Usage.CacheCreationInputTokens
			}
		case "content_block_start":
			switch cb := ev.ContentBlock; {
			case cb == nil:
			case cb.Type == "tool_use":
				if !send(agent.Emit{Event: agent.ToolCallDelta{Index: ev.Index, ID: cb.ID, Name: cb.Name}}) {
					return
				}
			case cb.Type == "redacted_thinking":
				// The whole block arrives here, with no deltas; it goes back unchanged.
				if !send(agent.Emit{Event: agent.ReasoningDelta{Redacted: cb.Data}}) {
					return
				}
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if !send(agent.Emit{Event: agent.TextDelta{Text: ev.Delta.Text}}) {
					return
				}
			case "input_json_delta":
				if !send(agent.Emit{Event: agent.ToolCallDelta{Index: ev.Index, ArgsFragment: json.RawMessage(ev.Delta.PartialJSON)}}) {
					return
				}
			case "thinking_delta":
				if !send(agent.Emit{Event: agent.ReasoningDelta{Text: ev.Delta.Thinking}}) {
					return
				}
			case "signature_delta":
				if !send(agent.Emit{Event: agent.ReasoningDelta{Signature: ev.Delta.Signature}}) {
					return
				}
			}
		case "message_delta":
			delta = true
			if ev.Usage != nil {
				out = ev.Usage.OutputTokens
			}
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				reason = ev.Delta.StopReason
			}
		case "message_stop":
			if !delta {
				send(agent.Emit{Err: fmt.Errorf("anthropic: message_stop before message_delta: %w", agent.ErrStreamProtocol)})
				return
			}
			send(agent.Emit{Event: agent.Finish{Reason: reason, Usage: agent.Usage{
				InputTokens: in, OutputTokens: out, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
			}}})
			return
		case "error":
			send(agent.Emit{Err: agent.ClassifyStreamError("anthropic", []byte(data))})
			return
		}
	}
	if err := sc.Err(); err != nil {
		send(agent.Emit{Err: agent.SSEReadError("anthropic", err)})
	}
}
