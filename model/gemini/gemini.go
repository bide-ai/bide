// Package gemini adapts Google's Gemini generateContent API to agent.Model. It
// translates the provider-neutral []agent.Message/Part model to Gemini "contents"
// (text, inlineData/fileData images, functionCall, functionResponse), declares tools
// as functionDeclarations, opens the SSE stream (streamGenerateContent?alt=sse), and
// normalizes Gemini's response chunks into agent's Event taxonomy. Zero external deps:
// net/http + stdlib, exactly like the anthropic and openai adapters (the repo enforces
// a lean core via architecture_test.go, so google.golang.org/genai is deliberately not
// imported).
//
// Gemini sends COMPLETE functionCall objects (name + args object), not the streamed
// argument fragments Anthropic/OpenAI send. So one functionCall part becomes a single
// ToolCallDelta carrying the whole args object as ArgsFragment; the agent core's
// finalize() json.Valid gate still holds because a complete args object is valid JSON.
package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// Model is a Google Gemini generateContent API adapter implementing agent.Model.
type Model struct {
	apiKey    string
	model     string
	maxTokens int
	baseURL   string
	http      *http.Client
}

var _ agent.Model = (*Model)(nil) // port/adapter contract

type Option func(*Model)

func WithModel(id string) Option           { return func(m *Model) { m.model = id } }
func WithMaxTokens(n int) Option           { return func(m *Model) { m.maxTokens = n } }
func WithBaseURL(u string) Option          { return func(m *Model) { m.baseURL = strings.TrimRight(u, "/") } }
func WithHTTPClient(c *http.Client) Option { return func(m *Model) { m.http = c } }

// New constructs a Gemini model adapter. apiKey is your Google AI Studio API key. For
// Vertex AI or a proxy, override the host with WithBaseURL (the request path
// /v1beta/models/{model}:streamGenerateContent is appended to it).
func New(apiKey string, opts ...Option) *Model {
	m := &Model{
		apiKey:  apiKey,
		model:   "gemini-2.0-flash",
		baseURL: "https://generativelanguage.googleapis.com",
		http:    http.DefaultClient,
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
	// alt=sse asks Gemini for server-sent events (one JSON object per data line) rather
	// than the default streamed-JSON-array framing, which matches our SSE scanner.
	url := m.baseURL + "/v1beta/models/" + m.model + ":streamGenerateContent?alt=sse"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-goog-api-key", m.apiKey)
	httpReq.Header.Set("content-type", "application/json")

	resp, err := m.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, agent.ClassifyHTTPError("gemini", resp)
	}

	ch := make(chan agent.Emit)
	go streamSSE(resp.Body, ch)
	return agent.NewStream(ch), nil
}

// buildRequest translates the provider-neutral request into a Gemini generateContent
// payload. System turns fold into the top-level `systemInstruction`; RoleTool turns
// become user turns carrying functionResponse parts (Gemini has no dedicated tool role).
func (m *Model) buildRequest(req agent.Request) ([]byte, error) {
	type obj = map[string]any

	var systemParts []obj
	var contents []obj

	for _, msg := range req.Messages {
		if msg.Role == agent.RoleSystem {
			for _, p := range msg.Parts {
				if t, ok := p.(agent.Text); ok {
					systemParts = append(systemParts, obj{"text": t.Text})
				}
			}
			continue
		}
		// Gemini roles are "user" and "model"; tool results ride in as a user turn.
		role := "user"
		if msg.Role == agent.RoleAssistant {
			role = "model"
		}
		var parts []obj
		for _, p := range msg.Parts {
			switch v := p.(type) {
			case agent.Text:
				parts = append(parts, obj{"text": v.Text})
			case agent.Reasoning:
				// Gemini's "thought" handling differs from Anthropic's echo-back-with-signature
				// contract and has no stable request-side wire shape we can verify here, so prior
				// Reasoning parts are dropped rather than fabricated onto the wire. Any text the
				// model actually produced still round-trips via Text parts.
			case agent.ToolUse:
				var args any = map[string]any{}
				if len(v.Args) > 0 {
					args = json.RawMessage(v.Args)
				}
				parts = append(parts, obj{"functionCall": obj{"name": v.Name, "args": args}})
			case agent.ToolResult:
				// Gemini keys the response by tool name (not the call id). The ToolResult carries
				// only the call id, so the name is best-effort recovered by scanning prior turns.
				name := m.toolNameForResult(req.Messages, v.ToolUseID)
				var response any
				if len(v.Result) > 0 && json.Valid(v.Result) {
					// A JSON object passes through; a bare JSON value is wrapped so response is an object.
					if bytes.HasPrefix(bytes.TrimSpace(v.Result), []byte("{")) {
						response = json.RawMessage(v.Result)
					} else {
						response = obj{"result": json.RawMessage(v.Result)}
					}
				} else {
					response = obj{"result": string(v.Result)}
				}
				parts = append(parts, obj{"functionResponse": obj{"name": name, "response": response}})
			case agent.Image:
				if v.URL != "" {
					parts = append(parts, obj{"fileData": obj{"fileUri": v.URL}})
				} else {
					parts = append(parts, obj{"inlineData": obj{
						"mimeType": v.Mime,
						"data":     base64.StdEncoding.EncodeToString(v.Data),
					}})
				}
			}
		}
		contents = append(contents, obj{"role": role, "parts": parts})
	}

	payload := obj{
		"contents": contents,
	}
	if len(systemParts) > 0 {
		payload["systemInstruction"] = obj{"parts": systemParts}
	}

	// Tools: one functionDeclarations array holding every declared tool.
	var decls []obj
	for _, t := range req.Tools {
		decl := obj{"name": t.Name(), "description": t.Description()}
		if s := t.ArgsSchema(); len(s) > 0 {
			decl["parameters"] = json.RawMessage(s)
		}
		decls = append(decls, decl)
	}
	if len(decls) > 0 {
		payload["tools"] = []obj{{"functionDeclarations": decls}}
	}

	// toolConfig.functionCallingConfig.mode: AUTO / ANY / NONE. "tool" forces ANY and
	// restricts the callable set to the named tool via allowedFunctionNames.
	if tc := req.ToolChoice; tc != nil {
		fcc := obj{}
		switch tc.Mode {
		case "", "auto":
			fcc["mode"] = "AUTO"
		case "required":
			fcc["mode"] = "ANY"
		case "none":
			fcc["mode"] = "NONE"
		case "tool":
			fcc["mode"] = "ANY"
			if tc.Name != "" {
				fcc["allowedFunctionNames"] = []string{tc.Name}
			}
		}
		if len(fcc) > 0 {
			payload["toolConfig"] = obj{"functionCallingConfig": fcc}
		}
	}

	// generationConfig: sampling controls. maxOutputTokens comes from the construction
	// default unless the request-level Sampling.MaxTokens overrides it.
	gen := obj{}
	maxTokens := m.maxTokens
	if s := req.Sampling.MaxTokens; s != nil {
		maxTokens = *s
	}
	if maxTokens > 0 {
		gen["maxOutputTokens"] = maxTokens
	}
	if s := req.Sampling.Temperature; s != nil {
		gen["temperature"] = *s
	}
	if s := req.Sampling.TopP; s != nil {
		gen["topP"] = *s
	}
	if len(req.Sampling.Stop) > 0 {
		gen["stopSequences"] = req.Sampling.Stop
	}
	// Gemini has no seed parameter; req.Sampling.Seed is intentionally ignored.

	// Structured output: ask for JSON and hand Gemini the schema. Gemini accepts a
	// responseSchema in an OpenAPI-subset dialect; we pass the provider-neutral JSON
	// Schema through as-is. It covers the common object/array/string/number cases; exotic
	// JSON Schema keywords (oneOf, $ref, etc.) may be rejected by Gemini, so this is a
	// best-effort pass-through rather than a verified full-dialect translation.
	if rf := req.ResponseFormat; rf != nil && len(rf.Schema) > 0 {
		gen["responseMimeType"] = "application/json"
		gen["responseSchema"] = json.RawMessage(rf.Schema)
	}
	if len(gen) > 0 {
		payload["generationConfig"] = gen
	}

	return json.Marshal(payload)
}

// toolNameForResult recovers the tool name for a tool-result turn by finding the
// matching ToolUse (by id) in a prior assistant turn. Gemini keys functionResponse by
// name, but agent.ToolResult carries only the call id, so this bridges the two.
func (m *Model) toolNameForResult(msgs []agent.Message, toolUseID string) string {
	for _, msg := range msgs {
		for _, p := range msg.Parts {
			if tu, ok := p.(agent.ToolUse); ok && tu.ID == toolUseID {
				return tu.Name
			}
		}
	}
	return toolUseID // fall back to the id so the field is never empty
}

// chunk is the subset of a Gemini streamGenerateContent SSE chunk we consume.
type chunk struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text         string `json:"text"`
				FunctionCall *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
}

// mapFinishReason maps Gemini's finishReason onto the neutral reason strings the other
// adapters emit (Anthropic "end_turn"/"tool_use"; OpenAI "stop"/"tool_calls"). The agent
// core's finalize() does not branch on the string, so this is for the caller's benefit.
func mapFinishReason(reason string, sawToolCall bool) string {
	if sawToolCall {
		return "tool_use"
	}
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "":
		return "stop"
	default:
		return strings.ToLower(reason)
	}
}

// streamSSE reads Gemini's SSE stream and pushes normalized agent events. It closes both
// the body and the channel. Package-internal so it's unit-testable without a network
// round-trip. Gemini sends complete functionCall objects, so each becomes one
// ToolCallDelta carrying the whole args object; a stable synthetic id is assigned when
// Gemini omits one (it does not send tool-call ids). Usage arrives on the usageMetadata
// of (typically) the final chunk; the terminal Finish carries it, matching the
// anthropic/openai contract that the agent core's finalize() reads.
func streamSSE(body io.ReadCloser, ch chan<- agent.Emit) {
	defer close(ch)
	defer body.Close()

	sc := agent.NewSSEScanner(body)

	var in, out, cacheRead int
	var lastReason string
	var sawToolCall bool
	var toolIndex int
	for sc.Scan() {
		data, ok := agent.SSEPayload(sc.Text())
		if !ok {
			continue // ignore blank separators and any non-data framing
		}
		var c chunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			ch <- agent.Emit{Err: fmt.Errorf("gemini sse decode: %w (%w)", err, agent.ErrModel)}
			return
		}
		for _, cand := range c.Candidates {
			for _, part := range cand.Content.Parts {
				if part.Text != "" {
					ch <- agent.Emit{Event: agent.TextDelta{Text: part.Text}}
				}
				if fc := part.FunctionCall; fc != nil {
					sawToolCall = true
					args := json.RawMessage(fc.Args)
					if len(args) == 0 {
						args = json.RawMessage("{}")
					}
					// Synthesize a stable id: Gemini does not send tool-call ids.
					id := "call_" + fc.Name + strconv.Itoa(toolIndex)
					ch <- agent.Emit{Event: agent.ToolCallDelta{
						Index: toolIndex, ID: id, Name: fc.Name, ArgsFragment: args,
					}}
					toolIndex++
				}
			}
			if cand.FinishReason != "" {
				lastReason = cand.FinishReason
			}
		}
		if u := c.UsageMetadata; u != nil {
			in = u.PromptTokenCount
			out = u.CandidatesTokenCount
			cacheRead = u.CachedContentTokenCount
		}
	}
	if err := sc.Err(); err != nil {
		ch <- agent.Emit{Err: err}
		return
	}
	// One terminal Finish carries the reason and the usage totals accumulated from the
	// stream's usageMetadata, mirroring how anthropic/openai signal the end of a turn.
	ch <- agent.Emit{Event: agent.Finish{
		Reason: mapFinishReason(lastReason, sawToolCall),
		Usage:  agent.Usage{InputTokens: in, OutputTokens: out, CacheReadTokens: cacheRead},
	}}
}
