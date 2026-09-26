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

	agent "github.com/dayna/go-agents"
)

// Model is an Anthropic Messages API adapter implementing agent.Model.
type Model struct {
	apiKey    string
	model     string
	maxTokens int
	baseURL   string
	http      *http.Client
	cache     bool
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

	ch := make(chan agent.Emit)
	go streamSSE(resp.Body, ch)
	return agent.NewStream(ch), nil
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
	type block map[string]any

	var system strings.Builder
	var msgs []map[string]any

	for _, msg := range req.Messages {
		if msg.Role == agent.RoleSystem {
			for _, p := range msg.Parts {
				if t, ok := p.(agent.Text); ok {
					system.WriteString(t.Text)
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
				blocks = append(blocks, block{"type": "text", "text": v.Text})
			case agent.Reasoning:
				b := block{"type": "thinking", "thinking": v.Text}
				if v.Signature != "" {
					b["signature"] = v.Signature
				}
				blocks = append(blocks, b)
			case agent.ToolUse:
				var input any = json.RawMessage(v.Args)
				if len(v.Args) == 0 {
					input = map[string]any{}
				}
				blocks = append(blocks, block{"type": "tool_use", "id": v.ID, "name": v.Name, "input": input})
			case agent.ToolResult:
				blocks = append(blocks, block{"type": "tool_result", "tool_use_id": v.ToolUseID, "content": string(v.Result), "is_error": v.IsError})
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
	if system.Len() > 0 {
		if m.cache {
			// A structured system block lets us attach a cache breakpoint to it.
			payload["system"] = []map[string]any{{
				"type":          "text",
				"text":          system.String(),
				"cache_control": map[string]any{"type": "ephemeral"},
			}}
		} else {
			payload["system"] = system.String()
		}
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	// tool_choice: {"type":"auto"} / {"type":"any"} (for "required") / {"type":"tool","name":...}.
	// Anthropic has no "none" equivalent, so map "none" the closest safe way by omitting the
	// tool declarations entirely (the model then cannot call a tool this turn).
	if tc := req.ToolChoice; tc != nil {
		switch tc.Mode {
		case "", "auto":
			payload["tool_choice"] = map[string]any{"type": "auto"}
		case "required":
			payload["tool_choice"] = map[string]any{"type": "any"}
		case "tool":
			payload["tool_choice"] = map[string]any{"type": "tool", "name": tc.Name}
		case "none":
			delete(payload, "tools")
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
// both the body and the channel. Exported-package-internal so it's unit-testable
// without a network round-trip.
func streamSSE(body io.ReadCloser, ch chan<- agent.Emit) {
	defer close(ch)
	defer body.Close()

	sc := agent.NewSSEScanner(body)

	var in, out, cacheRead, cacheWrite int
	for sc.Scan() {
		data, ok := agent.SSEPayload(sc.Text())
		if !ok {
			continue // ignore `event:` lines and blank separators; dispatch on the JSON's type
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			ch <- agent.Emit{Err: fmt.Errorf("anthropic sse decode: %w (%w)", err, agent.ErrModel)}
			return
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				in = ev.Message.Usage.InputTokens
				cacheRead = ev.Message.Usage.CacheReadInputTokens
				cacheWrite = ev.Message.Usage.CacheCreationInputTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				ch <- agent.Emit{Event: agent.ToolCallDelta{Index: ev.Index, ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}}
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				ch <- agent.Emit{Event: agent.TextDelta{Text: ev.Delta.Text}}
			case "input_json_delta":
				ch <- agent.Emit{Event: agent.ToolCallDelta{Index: ev.Index, ArgsFragment: json.RawMessage(ev.Delta.PartialJSON)}}
			case "thinking_delta":
				ch <- agent.Emit{Event: agent.ReasoningDelta{Text: ev.Delta.Thinking}}
			case "signature_delta":
				ch <- agent.Emit{Event: agent.ReasoningDelta{Signature: ev.Delta.Signature}}
			}
		case "message_delta":
			if ev.Usage != nil {
				out = ev.Usage.OutputTokens
			}
			reason := ""
			if ev.Delta != nil {
				reason = ev.Delta.StopReason
			}
			ch <- agent.Emit{Event: agent.Finish{Reason: reason, Usage: agent.Usage{
				InputTokens: in, OutputTokens: out, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
			}}}
		case "message_stop":
			return
		case "error":
			ch <- agent.Emit{Err: fmt.Errorf("anthropic stream error: %s (%w)", data, agent.ErrModel)}
			return
		}
	}
	if err := sc.Err(); err != nil {
		ch <- agent.Emit{Err: err}
	}
}
