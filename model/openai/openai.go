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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/internal/errtext"
	"github.com/bide-ai/bide/model/internal/toolcfg"
	"github.com/bide-ai/bide/schema"
)

type Model struct {
	maxResponse int64 // the streamed reply cap; see WithMaxResponseBytes
	apiKey      string
	model       string
	baseURL     string
	maxTokens   int
	strict      bool
	// completionTokens picks the token-limit field: nil decides by endpoint and model (see
	// WithMaxCompletionTokens); set, it is forced on or off.
	completionTokens *bool
	http             *http.Client
	toolCodec        agent.ToolResultCodec
}

var _ agent.Model = (*Model)(nil) // port/adapter contract

type Option func(*Model)

// WithMaxResponseBytes caps how many bytes of one streamed reply the adapter reads: a reply that
// runs longer fails with agent.ErrResponseTooLarge, which middleware.Retryable does not retry.
// The default is agent.DefaultMaxResponseBytes (32 MiB); n <= 0 keeps it. Raise it for replies
// that legitimately run longer, such as large inline images.
func WithMaxResponseBytes(n int64) Option { return func(m *Model) { m.maxResponse = n } }

func WithModel(id string) Option           { return func(m *Model) { m.model = id } }
func WithMaxTokens(n int) Option           { return func(m *Model) { m.maxTokens = n } }
func WithBaseURL(u string) Option          { return func(m *Model) { m.baseURL = strings.TrimRight(u, "/") } }
func WithHTTPClient(c *http.Client) Option { return func(m *Model) { m.http = c } }
func WithStrictSchema() Option             { return func(m *Model) { m.strict = true } }

// WithMaxCompletionTokens chooses the request field that carries the token limit
// (WithMaxTokens, or Sampling.MaxTokens): max_completion_tokens when use is true, max_tokens
// when false. Without it the adapter decides: max_completion_tokens on OpenAI's own endpoint
// (api.openai.com, where max_tokens is deprecated and the reasoning models reject it) and for
// an OpenAI reasoning model (an o-series or gpt-5 model id, optionally behind a "vendor/"
// prefix) on any endpoint, and max_tokens otherwise, which is what OpenAI-compatible servers
// such as Ollama and vLLM implement. Set it for an endpoint the default gets wrong, such as
// Azure OpenAI serving a reasoning model under a deployment name.
func WithMaxCompletionTokens(use bool) Option {
	return func(m *Model) { m.completionTokens = &use }
}

// WithToolResultCodec encodes tool results sent to the model with c instead of
// raw JSON (for example GCF, to cut tokens on structured output). The journal
// keeps the JSON form; only what the model reads changes. Default is JSON.
func WithToolResultCodec(c agent.ToolResultCodec) Option { return func(m *Model) { m.toolCodec = c } }

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
		return nil, agent.ClassifyHTTPError("openai", resp)
	}

	return agent.NewStreamFunc(ctx, func(send func(agent.Emit) bool) { streamSSE(agent.LimitResponse(resp.Body, m.maxResponse), send) }), nil
}

func (m *Model) buildRequest(req agent.Request) ([]byte, error) {
	// Tool names and the tool choice are checked first, so a setup the provider would reject
	// is a config error here rather than a 400.
	sendChoice, err := toolcfg.Check("openai", toolcfg.OpenAIName, req)
	if err != nil {
		return nil, err
	}
	type obj = map[string]any
	var msgs []obj
	// userRun holds the user messages merged into the last entry of msgs while that entry is a
	// user turn; see the RoleUser case.
	var userRun []agent.Message

	for _, msg := range req.Messages {
		if msg.Role != agent.RoleUser {
			userRun = nil
		}
		switch msg.Role {
		case agent.RoleSystem:
			// Several text parts are separate paragraphs of the prompt, not one run-on line.
			var texts []string
			for _, p := range msg.Parts {
				if t, ok := p.(agent.Text); ok && t.Text != "" {
					texts = append(texts, t.Text)
				}
			}
			msgs = append(msgs, obj{"role": "system", "content": strings.Join(texts, "\n\n")})
		case agent.RoleUser:
			// Consecutive user messages (a retrieved-context message ahead of the question, say)
			// merge into one user turn, in order: some OpenAI-compatible servers (vLLM with a
			// Mistral or Llama chat template) reject two user messages in a row.
			userRun = append(userRun, msg)
			if len(userRun) > 1 {
				msgs[len(msgs)-1]["content"] = mergedUserContent(userRun)
				continue
			}
			msgs = append(msgs, obj{"role": "user", "content": userContent(msg)})
		case agent.RoleTool:
			// each tool result becomes its own tool message
			for _, p := range msg.Parts {
				if tr, ok := p.(agent.ToolResult); ok {
					msgs = append(msgs, obj{"role": "tool", "tool_call_id": tr.ToolUseID, "content": agent.EncodeToolResultOr(m.toolCodec, tr.Result)})
				}
			}
		case agent.RoleAssistant:
			am := obj{"role": "assistant", "content": textOf(msg)}
			var calls []obj
			for _, p := range msg.Parts {
				// NOTE: Reasoning parts are intentionally dropped — OpenAI does not accept
				// prior reasoning as input (unlike Anthropic, which requires echoing it).
				if tu, ok := p.(agent.ToolUse); ok {
					args := string(tu.Args)
					if args == "" {
						args = "{}" // arguments must be a JSON object; a call with none has {}
					}
					calls = append(calls, obj{
						"id": tu.ID, "type": "function",
						"function": obj{"name": tu.Name, "arguments": args},
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
			// A schema strict mode cannot express fails the request: sending it non-strict would
			// quietly drop the strict mode the caller asked for.
			s, err := schema.OpenAIStrict(params)
			if err != nil {
				return nil, fmt.Errorf("openai: tool %q: strict schema: %w (%w)", t.Name(), err, agent.ErrConfig)
			}
			fn["parameters"] = json.RawMessage(s)
			fn["strict"] = true
		}
		tools = append(tools, obj{"type": "function", "function": fn})
	}

	payload := obj{
		"model":          m.model,
		"messages":       msgs,
		"stream":         true,
		"stream_options": obj{"include_usage": true},
	}
	// The token limit: request-level Sampling overrides the adapter's construction default. It
	// goes in max_completion_tokens or max_tokens (see WithMaxCompletionTokens).
	maxTokens := m.maxTokens
	if s := req.Sampling.MaxTokens; s != nil {
		maxTokens = *s
	}
	if maxTokens > 0 {
		if m.useCompletionTokens() {
			payload["max_completion_tokens"] = maxTokens
		} else {
			payload["max_tokens"] = maxTokens
		}
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
	// tool_choice: "auto" / "none" / "required" / {"type":"function","function":{"name":...}}.
	if tc := req.ToolChoice; tc != nil && sendChoice {
		switch tc.Mode {
		case "", "auto":
			payload["tool_choice"] = "auto"
		case "none":
			payload["tool_choice"] = "none"
		case "required":
			payload["tool_choice"] = "required"
		case "tool":
			payload["tool_choice"] = obj{"type": "function", "function": obj{"name": tc.Name}}
		}
	}
	if rf := req.ResponseFormat; rf != nil && len(rf.Schema) > 0 {
		// OpenAI strict structured outputs: the schema must be closed (additionalProperties
		// false, all keys required), the same transform we apply to tool schemas. A schema
		// strict mode cannot express fails the request rather than go out marked strict.
		sch, err := schema.OpenAIStrict(rf.Schema)
		if err != nil {
			return nil, fmt.Errorf("openai: response format %q: strict schema: %w (%w)", rf.Name, err, agent.ErrConfig)
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

// reasoningModel matches OpenAI reasoning model ids (o1, o3-mini, o4-mini, gpt-5, gpt-5.1, ...),
// which take max_completion_tokens and reject max_tokens wherever they are served.
var reasoningModel = regexp.MustCompile(`^(o[1-9][0-9]*(-|$)|gpt-5)`)

// useCompletionTokens reports whether the token limit goes in max_completion_tokens.
func (m *Model) useCompletionTokens() bool {
	if m.completionTokens != nil {
		return *m.completionTokens
	}
	if u, err := url.Parse(m.baseURL); err == nil && u.Hostname() == "api.openai.com" {
		return true
	}
	id := m.model
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:] // a router's "vendor/model" id
	}
	return reasoningModel.MatchString(id)
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

// userContent renders a user turn's content. With no image parts it returns a plain
// string (the common case, maximally compatible). When any Image part is present it
// returns the OpenAI array-of-parts form: text parts as {"type":"text","text":...} and
// images as {"type":"image_url","image_url":{"url":...}}, where raw bytes become a
// "data:<mime>;base64,<b64>" data URI and a URL passes through unchanged.
func userContent(m agent.Message) any {
	hasImage := false
	for _, p := range m.Parts {
		if _, ok := p.(agent.Image); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return textOf(m)
	}
	type obj = map[string]any
	var parts []obj
	for _, p := range m.Parts {
		switch v := p.(type) {
		case agent.Text:
			parts = append(parts, obj{"type": "text", "text": v.Text})
		case agent.Image:
			url := v.URL
			if url == "" {
				url = "data:" + v.Mime + ";base64," + base64.StdEncoding.EncodeToString(v.Data)
			}
			parts = append(parts, obj{"type": "image_url", "image_url": obj{"url": url}})
		}
	}
	return parts
}

// mergedUserContent renders consecutive user messages as the content of one user turn. With
// no image among them it is their texts joined by a blank line; otherwise it is the parts form
// (see userContent) of all their parts in order, each message's text its own part.
func mergedUserContent(run []agent.Message) any {
	var all agent.Message
	for _, m := range run {
		all.Parts = append(all.Parts, m.Parts...)
	}
	if parts, ok := userContent(all).([]map[string]any); ok {
		return parts
	}
	texts := make([]string, len(run))
	for i, m := range run {
		texts[i] = textOf(m)
	}
	return strings.Join(texts, "\n\n")
}

type chunk struct {
	Error   json.RawMessage `json:"error"` // a failure reported partway through the stream
	Choices []struct {
		Index int `json:"index"` // which completion; the adapter requests one, index 0
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

// finishReason maps an OpenAI finish_reason onto the neutral finish reasons (see agent.Finish).
// A reason it does not know is passed through unchanged, and the core refuses it rather than take
// the turn as done. A stream that ends with [DONE] and no finish_reason reports none ("").
func finishReason(r string) string {
	switch r {
	case "stop":
		return agent.FinishStop
	case "tool_calls", "function_call":
		return agent.FinishToolUse
	case "length":
		return agent.FinishLength
	case "content_filter":
		return agent.FinishFiltered
	default:
		return r
	}
}

// streamSSE reads OpenAI's SSE stream and pushes normalized agent events.
//
// The turn ends on a finish_reason or on [DONE], never on usage: some OpenAI-compatible servers
// (vLLM with continuous usage stats, and similar) report usage on every chunk, so a chunk that
// carries usage says nothing about whether the answer is complete. Usage is data, and the last
// usage seen goes out with the single terminal Finish, which is sent after [DONE] or, for a server
// that omits [DONE], when the stream ends after a finish_reason. A stream that ends with neither
// sends no Finish, so the consumer sees agent.ErrIncompleteResponse.
//
// Once a finish_reason has arrived the turn is over: a later chunk may repeat that reason or carry
// usage, but content (text, reasoning, a tool call) or a different finish_reason is
// agent.ErrStreamProtocol, not more of the answer.
func streamSSE(body io.ReadCloser, send func(agent.Emit) bool) {
	defer body.Close()

	sc := agent.NewSSEScanner(body)

	var reason string // the turn's finish_reason, once one has arrived
	var usage agent.Usage
	var done bool
	for sc.Scan() {
		data, ok := agent.SSEPayload(sc.Text())
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			break
		}
		var c chunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			send(agent.Emit{Err: fmt.Errorf("openai sse decode: %w (%w)", err, agent.ErrModel)})
			return
		}
		if len(c.Error) > 0 && string(c.Error) != "null" {
			send(agent.Emit{Err: agent.ClassifyStreamError("openai", []byte(data))})
			return
		}
		for _, choice := range c.Choices {
			if choice.Index != 0 {
				// The request asks for one completion. Another is not part of this answer.
				send(agent.Emit{Err: fmt.Errorf("openai: a chunk for choice %d; the request asked for one completion: %w", choice.Index, agent.ErrStreamProtocol)})
				return
			}
			d := choice.Delta
			if reason != "" && (d.ReasoningContent != "" || d.Content != "" || len(d.ToolCalls) > 0) {
				send(agent.Emit{Err: fmt.Errorf("openai: content after finish_reason %s: %w", errtext.Quote(reason), agent.ErrStreamProtocol)})
				return
			}
			if rc := d.ReasoningContent; rc != "" {
				if !send(agent.Emit{Event: agent.ReasoningDelta{Text: rc}}) {
					return
				}
			}
			if txt := d.Content; txt != "" {
				if !send(agent.Emit{Event: agent.TextDelta{Text: txt}}) {
					return
				}
			}
			for _, tc := range d.ToolCalls {
				if !send(agent.Emit{Event: agent.ToolCallDelta{
					Index: tc.Index, ID: tc.ID, Name: tc.Function.Name,
					ArgsFragment: json.RawMessage(tc.Function.Arguments),
				}}) {
					return
				}
			}
			if fr := choice.FinishReason; fr != nil && *fr != "" {
				if reason != "" && *fr != reason {
					send(agent.Emit{Err: fmt.Errorf("openai: finish_reason %s after %s: %w", errtext.Quote(*fr), errtext.Quote(reason), agent.ErrStreamProtocol)})
					return
				}
				reason = *fr
			}
		}
		if c.Usage != nil {
			usage = agent.Usage{InputTokens: c.Usage.PromptTokens, OutputTokens: c.Usage.CompletionTokens}
			// OpenAI caches prefixes automatically. prompt_tokens includes the cached tokens;
			// agent.Usage counts them once, in CacheReadTokens.
			if d := c.Usage.PromptTokensDetails; d != nil {
				usage.CacheReadTokens = d.CachedTokens
				usage.InputTokens -= d.CachedTokens
			}
		}
	}
	if err := sc.Err(); err != nil {
		send(agent.Emit{Err: agent.SSEReadError("openai", err)})
		return
	}
	if done || reason != "" {
		send(agent.Emit{Event: agent.Finish{Reason: finishReason(reason), Usage: usage}})
	}
}
