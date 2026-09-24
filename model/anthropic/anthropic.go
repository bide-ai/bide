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
	"bufio"
	"bytes"
	"context"
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
}

var _ agent.Model = (*Model)(nil) // port/adapter contract

type Option func(*Model)

func WithModel(id string) Option          { return func(m *Model) { m.model = id } }
func WithMaxTokens(n int) Option          { return func(m *Model) { m.maxTokens = n } }
func WithBaseURL(u string) Option         { return func(m *Model) { m.baseURL = u } }
func WithHTTPClient(c *http.Client) Option { return func(m *Model) { m.http = c } }

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
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic: status %d: %s", resp.StatusCode, b)
	}

	ch := make(chan agent.Emit)
	go streamSSE(resp.Body, ch)
	return agent.NewStream(ch), nil
}

// buildRequest translates the provider-neutral request into an Anthropic Messages
// payload. System turns fold into the top-level `system` field; RoleTool turns become
// user turns carrying tool_result blocks.
//
// TODO: merge consecutive same-role turns (Anthropic wants alternating user/assistant);
// TODO: input_schema comes from the schema/ provider-aware emitter once built.
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
			}
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

	payload := map[string]any{
		"model":      m.model,
		"max_tokens": m.maxTokens,
		"stream":     true,
		"messages":   msgs,
	}
	if system.Len() > 0 {
		payload["system"] = system.String()
	}
	if len(tools) > 0 {
		payload["tools"] = tools
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
			InputTokens int `json:"input_tokens"`
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

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // raise the 64KB line cap (openai-go #368 lesson)

	var in, out int
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // ignore `event:` lines and blank separators; dispatch on the JSON's type
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			ch <- agent.Emit{Err: fmt.Errorf("anthropic sse decode: %w", err)}
			return
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				in = ev.Message.Usage.InputTokens
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
			ch <- agent.Emit{Event: agent.Finish{Reason: reason, Usage: agent.Usage{InputTokens: in, OutputTokens: out}}}
		case "message_stop":
			return
		case "error":
			ch <- agent.Emit{Err: fmt.Errorf("anthropic stream error: %s", data)}
			return
		}
	}
	if err := sc.Err(); err != nil {
		ch <- agent.Emit{Err: err}
	}
}
