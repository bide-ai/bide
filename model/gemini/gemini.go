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
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/internal/errtext"
	"github.com/bide-ai/bide/model/internal/toolcfg"
	"github.com/bide-ai/bide/model/provider"
	"github.com/bide-ai/bide/schema"
)

// Model is a Google Gemini generateContent API adapter implementing agent.Model.
type Model struct {
	maxResponse int64 // the streamed reply cap; see WithMaxResponseBytes
	apiKey      string
	model       string
	maxTokens   int
	baseURL     string
	http        *http.Client
	toolCodec   provider.ToolResultCodec
}

var (
	_ agent.Model     = (*Model)(nil) // port/adapter contract
	_ agent.Describer = (*Model)(nil)
)

type Option func(*Model)

// WithMaxResponseBytes caps how many bytes of one streamed reply the adapter reads: a reply that
// runs longer fails with agent.ErrResponseTooLarge, which middleware.Retryable does not retry.
// The default is provider.DefaultMaxResponseBytes (32 MiB); n <= 0 keeps it. Raise it for replies
// that legitimately run longer, such as large inline images.
func WithMaxResponseBytes(n int64) Option { return func(m *Model) { m.maxResponse = n } }

func WithModel(id string) Option           { return func(m *Model) { m.model = id } }
func WithMaxTokens(n int) Option           { return func(m *Model) { m.maxTokens = n } }
func WithBaseURL(u string) Option          { return func(m *Model) { m.baseURL = strings.TrimRight(u, "/") } }
func WithHTTPClient(c *http.Client) Option { return func(m *Model) { m.http = c } }

// WithToolResultCodec encodes tool results sent to the model with c instead of
// raw JSON (for example GCF, to cut tokens on structured output). The journal
// keeps the JSON form; only what the model reads changes. Default is JSON.
// Gemini normally passes a JSON tool result through as a structured response;
// with a codec set, the encoded string is sent as {"result": <encoded>}.
func WithToolResultCodec(c provider.ToolResultCodec) Option {
	return func(m *Model) { m.toolCodec = c }
}

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

// Describe reports the adapter's identity: provider "gemini", the configured model ID, and support
// for a JSON-schema response format.
func (m *Model) Describe() agent.ModelInfo {
	return agent.ModelInfo{Provider: "gemini", Model: m.model, ResponseFormat: true}
}

// Stream implements agent.Model.
func (m *Model) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	body, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	// alt=sse asks Gemini for server-sent events (one JSON object per data line) rather
	// than the default streamed-JSON-array framing, which matches our SSE scanner.
	endpoint := m.baseURL + "/v1beta/models/" + m.model + ":streamGenerateContent?alt=sse"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
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
		return nil, provider.ClassifyHTTPError("gemini", resp)
	}

	return agent.NewStreamFunc(ctx, func(send func(agent.Emit) bool) { streamSSE(provider.LimitResponse(resp.Body, m.maxResponse), send) }), nil
}

// buildRequest translates the provider-neutral request into a Gemini generateContent
// payload. System turns fold into the top-level `systemInstruction`; RoleTool turns
// become user turns carrying functionResponse parts (Gemini has no dedicated tool role).
//
// Gemini answers a model turn's function calls with one content holding a functionResponse
// part per call, in the order of the calls. The agent sends one RoleTool message per result
// (and, on a resumed run, in the order the results were journaled), so consecutive RoleTool
// messages are gathered into one content and sorted into call order. Other consecutive
// same-role turns are merged too, but function responses never share a content with other
// parts. A turn left with no parts (an assistant turn holding only reasoning, an empty text)
// is skipped: Gemini rejects a content with no parts.
func (m *Model) buildRequest(req agent.Request) ([]byte, error) {
	// Tool names and the tool choice are checked first, so a setup the provider would reject
	// is a config error here rather than a 400.
	sendChoice, err := toolcfg.Check("gemini", toolcfg.GeminiName, req)
	if err != nil {
		return nil, err
	}
	type obj = map[string]any

	var systemParts []obj
	var contents []obj
	var responses []bool          // responses[i]: contents[i] holds function responses
	callOrder := map[string]int{} // tool-use id -> position among all the calls so far

	for _, msg := range req.Messages {
		if msg.Role == agent.RoleSystem {
			for _, p := range msg.Parts {
				if t, ok := p.(agent.Text); ok && t.Text != "" {
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
		isResponse := msg.Role == agent.RoleTool
		var parts []obj
		var order []int // for a tool turn, each part's position among the calls
		for _, p := range msg.Parts {
			if _, ok := p.(agent.ToolResult); isResponse && !ok {
				continue // a tool turn carries only results, as in the other adapters
			}
			switch v := p.(type) {
			case agent.Text:
				if v.Text == "" {
					continue // Gemini rejects an empty text part
				}
				parts = append(parts, obj{"text": v.Text})
			case agent.Reasoning:
				// Gemini does not take thought text back as input; what it needs from a thinking
				// turn is the thoughtSignature, which rides on the functionCall part it came with
				// (ToolUse.Signature). So prior Reasoning parts are dropped.
			case agent.ToolUse:
				var args any = map[string]any{}
				if len(v.Args) > 0 {
					args = json.RawMessage(v.Args)
				}
				part := obj{"functionCall": obj{"name": v.Name, "args": args}}
				if v.Signature != "" {
					part["thoughtSignature"] = v.Signature
				}
				callOrder[v.ID] = len(callOrder)
				parts = append(parts, part)
			case agent.ToolResult:
				// Gemini keys the response by tool name (not the call id). The ToolResult carries
				// only the call id, so the name is best-effort recovered by scanning prior turns.
				name := m.toolNameForResult(req.Messages, v.ToolUseID)
				var response any
				switch {
				case m.toolCodec != nil:
					// An explicit codec (for example GCF) renders the result to a
					// string; Gemini requires an object, so it is wrapped.
					response = obj{"result": provider.EncodeToolResultOr(m.toolCodec, v.Result)}
				case len(v.Result) > 0 && json.Valid(v.Result):
					// A JSON object passes through; a bare JSON value is wrapped so response is an object.
					if bytes.HasPrefix(bytes.TrimSpace(v.Result), []byte("{")) {
						response = json.RawMessage(v.Result)
					} else {
						response = obj{"result": json.RawMessage(v.Result)}
					}
				default:
					response = obj{"result": string(v.Result)}
				}
				pos, ok := callOrder[v.ToolUseID]
				if !ok {
					pos = len(callOrder) // a result for no known call goes after the rest
				}
				parts = append(parts, obj{"functionResponse": obj{"name": name, "response": response}})
				order = append(order, pos)
			case agent.Image:
				if v.URL != "" {
					mime, err := fileMime(v)
					if err != nil {
						return nil, err
					}
					parts = append(parts, obj{"fileData": obj{"mimeType": mime, "fileUri": v.URL}})
				} else {
					parts = append(parts, obj{"inlineData": obj{
						"mimeType": v.Mime,
						"data":     base64.StdEncoding.EncodeToString(v.Data),
					}})
				}
			}
		}
		if len(parts) == 0 {
			continue
		}
		if n := len(contents); n > 0 && contents[n-1]["role"] == role && responses[n-1] == isResponse {
			prev := contents[n-1]
			prev["parts"] = append(prev["parts"].([]obj), parts...)
			if isResponse {
				prev["order"] = append(prev["order"].([]int), order...)
			}
			continue
		}
		c := obj{"role": role, "parts": parts}
		if isResponse {
			c["order"] = order
		}
		contents = append(contents, c)
		responses = append(responses, isResponse)
	}
	for _, c := range contents {
		if order, ok := c["order"].([]int); ok {
			delete(c, "order")
			parts := c["parts"].([]obj)
			idx := make([]int, len(parts))
			for i := range idx {
				idx[i] = i
			}
			slices.SortStableFunc(idx, func(a, b int) int { return cmp.Compare(order[a], order[b]) })
			sorted := make([]obj, len(parts))
			for i, j := range idx {
				sorted[i] = parts[j]
			}
			c["parts"] = sorted
		}
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
		decl := obj{"name": t.Name, "description": t.Description}
		params, err := toolParameters(t.Input)
		if err != nil {
			return nil, fmt.Errorf("gemini: tool %q: %w (%w)", t.Name, err, agent.ErrConfig)
		}
		if params != nil {
			decl["parameters"] = params
		}
		decls = append(decls, decl)
	}
	if len(decls) > 0 {
		payload["tools"] = []obj{{"functionDeclarations": decls}}
	}

	// toolConfig.functionCallingConfig.mode: AUTO / ANY / NONE. "tool" forces ANY and
	// restricts the callable set to the named tool via allowedFunctionNames.
	if tc := req.ToolChoice; tc != nil && sendChoice {
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
			fcc["allowedFunctionNames"] = []string{tc.Name} // toolcfg.Check ensured it names a declared tool
		}
		payload["toolConfig"] = obj{"functionCallingConfig": fcc}
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

	// Structured output: ask for JSON and hand Gemini the schema, translated to the OpenAPI
	// subset Gemini reads (schema.Gemini). A schema the subset cannot express fails the
	// request rather than reach Gemini as one it would reject or read differently.
	if rf := req.ResponseFormat; rf != nil && len(rf.Schema) > 0 {
		s, err := schema.Gemini(rf.Schema)
		if err != nil {
			return nil, fmt.Errorf("gemini: response format %q: %w (%w)", rf.Name, err, agent.ErrConfig)
		}
		gen["responseMimeType"] = "application/json"
		gen["responseSchema"] = s
	}
	if len(gen) > 0 {
		payload["generationConfig"] = gen
	}

	return json.Marshal(payload)
}

// toolParameters translates a tool's argument schema for a function declaration. A tool that
// takes no arguments (no schema, or an object with no properties and no additionalProperties)
// gets nil, so the declaration carries no parameters: Gemini rejects an OBJECT whose properties
// are empty. Anything else goes through schema.Gemini.
func toolParameters(s json.RawMessage) (json.RawMessage, error) {
	if len(s) == 0 {
		return nil, nil
	}
	var probe struct {
		Type                 any             `json:"type"`
		Properties           map[string]any  `json:"properties"`
		AdditionalProperties json.RawMessage `json:"additionalProperties"`
	}
	if err := json.Unmarshal(s, &probe); err != nil {
		return nil, err
	}
	if probe.Type == "object" && len(probe.Properties) == 0 &&
		(probe.AdditionalProperties == nil || string(probe.AdditionalProperties) == "false") {
		return nil, nil
	}
	return schema.Gemini(s)
}

// fileMime is the mimeType of an image sent by URL, which Gemini's fileData requires: the
// Image's Mime, or else the type the URL path's extension names. An image with neither is a
// config error.
func fileMime(img agent.Image) (string, error) {
	if img.Mime != "" {
		return img.Mime, nil
	}
	p := img.URL
	if u, err := url.Parse(img.URL); err == nil {
		p = u.Path
	}
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		t, _, _ = strings.Cut(t, ";")
		return t, nil
	}
	return "", fmt.Errorf("gemini: image %q: set Image.Mime, since the URL names no known file type: %w", img.URL, agent.ErrConfig)
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
	Error      json.RawMessage `json:"error"` // a failure reported partway through the stream
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text             string `json:"text"`
				Thought          bool   `json:"thought"`          // Text is a thought summary, not answer text
				ThoughtSignature string `json:"thoughtSignature"` // opaque; sent back on the part it came with
				FunctionCall     *struct {
					ID   string          `json:"id"`
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

// newCallID returns a tool-call id for a call Gemini sent without one. The agent keys each
// call's result and journal step by this id, so it must differ from every id issued before
// it, in this response or any other: it is random, and carries nothing from the tool name or
// the call's position that could make two calls collide.
func newCallID() string {
	var b [12]byte
	rand.Read(b[:]) // never fails (crypto/rand panics rather than return an error)
	return "call_" + hex.EncodeToString(b[:])
}

// mapFinishReason maps Gemini's finishReason onto the neutral finish reasons (see agent.Finish).
// A natural stop is tool_use when the turn made a tool call. A turn cut off at the token limit is
// FinishLength even when it made one: the model may have meant to say or call more. A reason a
// safety, recitation, or blocklist filter gives is FinishFiltered. Any other reason
// (MALFORMED_FUNCTION_CALL, OTHER, one added later) is passed through unchanged, and the core
// refuses it rather than take the turn as done.
func mapFinishReason(reason string, sawToolCall bool) agent.FinishReason {
	switch reason {
	case "STOP":
		if sawToolCall {
			return agent.FinishToolUse
		}
		return agent.FinishStop
	case "MAX_TOKENS":
		return agent.FinishLength
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return agent.FinishFiltered
	case "":
		// No reason is not a natural stop: the core would take "" as one. streamSSE sends no Finish
		// without a reason, so this only keeps the mapping itself from ever making one an answer.
		return "FINISH_REASON_UNSPECIFIED"
	default:
		return agent.FinishReason(reason)
	}
}

// streamSSE reads Gemini's SSE stream and pushes normalized agent events. It closes both
// the body and the channel. Package-internal so it's unit-testable without a network
// round-trip. Gemini sends complete functionCall objects, so each becomes one
// ToolCallDelta carrying the whole args object and the part's thoughtSignature. Gemini
// usually sends no tool-call id, so one is made up (see newCallID) when the call carries
// none. A part flagged thought is reasoning, not answer text. Usage arrives on the
// usageMetadata of (typically) the final chunk; the terminal Finish carries it, matching the
// anthropic/openai contract that the agent core's finalize() reads.
//
// A finishReason ends the turn's content: a later chunk may repeat that reason or carry usage,
// but text, a functionCall, or a different finishReason is agent.ErrStreamProtocol, not more of
// the answer.
//
// Thought signatures on text parts are not kept: Gemini requires them back only on
// functionCall parts.
func streamSSE(body io.ReadCloser, send func(agent.Emit) bool) {
	defer body.Close()

	sc := provider.NewSSEScanner(body)

	var in, out, cacheRead int
	var lastReason string
	var sawToolCall bool
	var toolIndex int
	for sc.Scan() {
		data, ok := provider.SSEPayload(sc.Text())
		if !ok {
			continue // ignore blank separators and any non-data framing
		}
		var c chunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			send(agent.Emit{Err: fmt.Errorf("gemini sse decode: %w (%w)", err, agent.ErrModel)})
			return
		}
		if len(c.Error) > 0 && string(c.Error) != "null" {
			send(agent.Emit{Err: provider.ClassifyStreamError("gemini", []byte(data))})
			return
		}
		for _, cand := range c.Candidates {
			for _, part := range cand.Content.Parts {
				if lastReason != "" && (part.Text != "" || part.FunctionCall != nil) {
					send(agent.Emit{Err: fmt.Errorf("gemini: content after finishReason %s: %w", errtext.Quote(lastReason), agent.ErrStreamProtocol)})
					return
				}
				if part.Text != "" {
					var ev agent.Event = agent.TextDelta{Text: part.Text}
					if part.Thought {
						ev = agent.ReasoningDelta{Text: part.Text}
					}
					if !send(agent.Emit{Event: ev}) {
						return
					}
				}
				if fc := part.FunctionCall; fc != nil {
					sawToolCall = true
					args := json.RawMessage(fc.Args)
					if len(args) == 0 {
						args = json.RawMessage("{}")
					}
					id := fc.ID
					if id == "" {
						id = newCallID()
					}
					if !send(agent.Emit{Event: agent.ToolCallDelta{
						Index: toolIndex, ID: id, Name: fc.Name, ArgsFragment: args, Signature: part.ThoughtSignature,
					}}) {
						return
					}
					toolIndex++
				}
			}
			if cand.FinishReason != "" {
				if lastReason != "" && cand.FinishReason != lastReason {
					send(agent.Emit{Err: fmt.Errorf("gemini: finishReason %s after %s: %w", errtext.Quote(cand.FinishReason), errtext.Quote(lastReason), agent.ErrStreamProtocol)})
					return
				}
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
		send(agent.Emit{Err: provider.SSEReadError("gemini", err)})
		return
	}
	if lastReason == "" {
		// No candidate reported a finishReason, so the response stopped partway through the
		// turn. Send no Finish: the consumer sees agent.ErrIncompleteResponse.
		return
	}
	// One terminal Finish carries the reason and the usage totals accumulated from the
	// stream's usageMetadata, mirroring how anthropic/openai signal the end of a turn.
	send(agent.Emit{Event: agent.Finish{
		Reason: mapFinishReason(lastReason, sawToolCall),
		Raw:    lastReason,
		// promptTokenCount includes the cached tokens; agent.Usage counts them once, in
		// CacheReadTokens.
		Usage: agent.Usage{InputTokens: in - cacheRead, OutputTokens: out, CacheReadTokens: cacheRead},
	}})
}
