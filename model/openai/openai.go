// Package openai adapts the OpenAI Chat Completions API to agent.Model. Because that
// API is the de-facto standard, this one adapter — via WithBaseURL — runs on OpenAI,
// Ollama (local), DeepSeek, Groq, Together, OpenRouter, vLLM, LM Studio, Azure, Mistral,
// xAI, and any other OpenAI-compatible endpoint. That's how "run on any model" is one
// adapter, not N.
//
// Zero external deps (net/http + stdlib). Tool schemas use schema.OpenAIStrict when
// WithStrictSchema is set (genuine OpenAI structured-output strict mode — which many
// SDKs, incl. agenticenv, leave unused); off by default for max compatibility.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/schema"
)

type Model struct {
	apiKey    string
	model     string
	baseURL   string
	maxTokens int
	strict    bool
	http      *http.Client
}

var _ agent.Model = (*Model)(nil) // port/adapter contract

type Option func(*Model)

func WithModel(id string) Option           { return func(m *Model) { m.model = id } }
func WithMaxTokens(n int) Option           { return func(m *Model) { m.maxTokens = n } }
func WithBaseURL(u string) Option          { return func(m *Model) { m.baseURL = strings.TrimRight(u, "/") } }
func WithHTTPClient(c *http.Client) Option { return func(m *Model) { m.http = c } }
func WithStrictSchema() Option             { return func(m *Model) { m.strict = true } }

// New constructs an OpenAI-compatible adapter. For non-OpenAI endpoints set WithBaseURL
// (e.g. "http://localhost:11434/v1" for Ollama) and WithModel.
func New(apiKey string, opts ...Option) *Model {
	m := &Model{
		apiKey:  apiKey,
		model:   "gpt-4o",
		baseURL: "https://api.openai.com/v1",
		http:    http.DefaultClient,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

func (m *Model) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	body, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	if m.apiKey != "" {
		httpReq.Header.Set("authorization", "Bearer "+m.apiKey)
	}

	resp, err := m.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			d := parseRetryAfter(resp.Header.Get("Retry-After"))
			return nil, &agent.RateLimited{
				RetryAfter: d,
				Err:        fmt.Errorf("openai: rate limited (%w)", agent.ErrModel),
			}
		}
		return nil, fmt.Errorf("openai: status %d: %s (%w)", resp.StatusCode, b, agent.ErrModel)
	}

	ch := make(chan agent.Emit)
	go streamSSE(resp.Body, ch)
	return agent.NewStream(ch), nil
}

func (m *Model) buildRequest(req agent.Request) ([]byte, error) {
	type obj = map[string]any
	var msgs []obj

	for _, msg := range req.Messages {
		switch msg.Role {
		case agent.RoleSystem:
			msgs = append(msgs, obj{"role": "system", "content": textOf(msg)})
		case agent.RoleUser:
			msgs = append(msgs, obj{"role": "user", "content": textOf(msg)})
		case agent.RoleTool:
			// each tool result becomes its own tool message
			for _, p := range msg.Parts {
				if tr, ok := p.(agent.ToolResult); ok {
					msgs = append(msgs, obj{"role": "tool", "tool_call_id": tr.ToolUseID, "content": string(tr.Result)})
				}
			}
		case agent.RoleAssistant:
			am := obj{"role": "assistant", "content": textOf(msg)}
			var calls []obj
			for _, p := range msg.Parts {
				// NOTE: Reasoning parts are intentionally dropped — OpenAI does not accept
				// prior reasoning as input (unlike Anthropic, which requires echoing it).
				if tu, ok := p.(agent.ToolUse); ok {
					calls = append(calls, obj{
						"id": tu.ID, "type": "function",
						"function": obj{"name": tu.Name, "arguments": string(tu.Args)},
					})
				}
			}
			if len(calls) > 0 {
				am["tool_calls"] = calls
			}
			msgs = append(msgs, am)
		}
	}

	var tools []obj
	for _, t := range req.Tools {
		params := t.ArgsSchema()
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object"}`)
		}
		fn := obj{"name": t.Name(), "description": t.Description(), "parameters": json.RawMessage(params)}
		if m.strict {
			if s, err := schema.OpenAIStrict(params); err == nil {
				fn["parameters"] = json.RawMessage(s)
				fn["strict"] = true
			}
		}
		tools = append(tools, obj{"type": "function", "function": fn})
	}

	payload := obj{
		"model":          m.model,
		"messages":       msgs,
		"stream":         true,
		"stream_options": obj{"include_usage": true},
	}
	// max_tokens: request-level Sampling overrides the adapter's construction default.
	maxTokens := m.maxTokens
	if s := req.Sampling.MaxTokens; s != nil {
		maxTokens = *s
	}
	if maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}
	if s := req.Sampling.Temperature; s != nil {
		payload["temperature"] = *s
	}
	if s := req.Sampling.TopP; s != nil {
		payload["top_p"] = *s
	}
	if s := req.Sampling.Seed; s != nil {
		payload["seed"] = *s
	}
	if len(req.Sampling.Stop) > 0 {
		payload["stop"] = req.Sampling.Stop
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	if rf := req.ResponseFormat; rf != nil && len(rf.Schema) > 0 {
		// OpenAI strict structured outputs: the schema must be closed (additionalProperties
		// false, all keys required) — the same transform we apply to tool schemas.
		sch := json.RawMessage(rf.Schema)
		if strict, err := schema.OpenAIStrict(rf.Schema); err == nil {
			sch = strict
		}
		payload["response_format"] = obj{
			"type": "json_schema",
			"json_schema": obj{
				"name":   rf.Name,
				"schema": sch,
				"strict": true,
			},
		}
	}
	return json.Marshal(payload)
}

// parseRetryAfter parses an HTTP Retry-After header value. It accepts either an
// integer number of seconds or an HTTP-date. Returns 0 if absent or unparseable.
func parseRetryAfter(s string) time.Duration {
	if s == "" {
		return 0
	}
	if secs, err := strconv.Atoi(s); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(s); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

func textOf(m agent.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		if t, ok := p.(agent.Text); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

type chunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"` // DeepSeek/Ollama reasoning
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func streamSSE(body io.ReadCloser, ch chan<- agent.Emit) {
	defer close(ch)
	defer body.Close()

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var lastReason string
	var finished bool
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var c chunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			ch <- agent.Emit{Err: fmt.Errorf("openai sse decode: %w (%w)", err, agent.ErrModel)}
			return
		}
		for _, choice := range c.Choices {
			if rc := choice.Delta.ReasoningContent; rc != "" {
				ch <- agent.Emit{Event: agent.ReasoningDelta{Text: rc}}
			}
			if txt := choice.Delta.Content; txt != "" {
				ch <- agent.Emit{Event: agent.TextDelta{Text: txt}}
			}
			for _, tc := range choice.Delta.ToolCalls {
				ch <- agent.Emit{Event: agent.ToolCallDelta{
					Index: tc.Index, ID: tc.ID, Name: tc.Function.Name,
					ArgsFragment: json.RawMessage(tc.Function.Arguments),
				}}
			}
			if choice.FinishReason != nil {
				lastReason = *choice.FinishReason
			}
		}
		if c.Usage != nil {
			u := agent.Usage{InputTokens: c.Usage.PromptTokens, OutputTokens: c.Usage.CompletionTokens}
			if d := c.Usage.PromptTokensDetails; d != nil { // OpenAI caches prefixes automatically
				u.CacheReadTokens = d.CachedTokens
			}
			ch <- agent.Emit{Event: agent.Finish{Reason: lastReason, Usage: u}}
			finished = true
		}
	}
	if !finished { // servers that omit a usage chunk still get a terminal Finish
		ch <- agent.Emit{Event: agent.Finish{Reason: lastReason}}
	}
	if err := sc.Err(); err != nil {
		ch <- agent.Emit{Err: err}
	}
}
