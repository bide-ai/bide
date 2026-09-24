package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
)

// Model is the provider primitive. Streaming is FIRST-CLASS: Stream is the only
// method an adapter must implement. Generate is a convenience drain built on top —
// the opposite of frameworks that make blocking generation primary and bolt
// streaming on later.
type Model interface {
	Stream(ctx context.Context, req Request) (*Stream, error)
}

// Request is a single model call.
type Request struct {
	Messages       []Message
	Tools          []Tool
	Sampling       Sampling        // generation controls; zero value = provider/model defaults
	ResponseFormat *ResponseFormat // nil = free-form; set = constrain output to a JSON schema
}

// ResponseFormat asks the provider to constrain the model's output to a named JSON schema
// (OpenAI structured outputs / "json_schema" response format). Adapters that don't support
// it ignore it. Name is the schema name; Schema is provider-neutral JSON Schema (as from
// the schema package). See RunTypedNative.
type ResponseFormat struct {
	Name   string
	Schema json.RawMessage
}

// Sampling holds provider-neutral generation controls. Pointer fields distinguish
// "unset" (nil → use the provider/model default) from an explicit value — so a
// deliberate Temperature of 0 is not confused with "not specified". Each adapter maps
// the set fields onto its wire format and ignores those it doesn't support (e.g.
// Anthropic has no Seed). Set it once with Agent.WithSampling.
type Sampling struct {
	Temperature *float64 // 0..2 (OpenAI) / 0..1 (Anthropic); determinism at 0
	TopP        *float64 // nucleus sampling
	MaxTokens   *int     // cap on generated tokens; overrides the adapter's construction default
	Stop        []string // stop sequences
	Seed        *int64   // best-effort determinism (OpenAI; ignored where unsupported)
}

// Usage is token accounting for a call; middleware turns it into cost.
type Usage struct {
	InputTokens  int
	OutputTokens int
	// CacheReadTokens is input tokens served from the provider's prompt cache (billed at
	// a discount). CacheWriteTokens is input tokens written to the cache on this call
	// (Anthropic cache creation; 0 for providers that cache implicitly, like OpenAI).
	CacheReadTokens  int
	CacheWriteTokens int
}

// Event is a normalized streamed model event. The stream/ package maps every
// provider wire format (OpenAI SSE / Anthropic typed / Gemini NDJSON / Bedrock
// binary) onto these.
type Event interface{ event() }

type TextDelta struct{ Text string }

func (TextDelta) event() {}

type ReasoningDelta struct {
	Text      string
	Signature string // opaque provider token to echo back on later turns (Anthropic thinking)
}

func (ReasoningDelta) event() {}

// ToolCallDelta is an incremental fragment of a tool call, keyed by Index. Fragments
// are concatenated and gated by json.Valid before use (the UTF-8/partial-JSON safety
// the research flagged as universally missing).
type ToolCallDelta struct {
	Index        int
	ID           string
	Name         string
	ArgsFragment json.RawMessage
}

func (ToolCallDelta) event() {}

type Finish struct {
	Reason string
	Usage  Usage
}

func (Finish) event() {}

// Emit is what an adapter pushes onto its event channel (event or terminal error).
type Emit struct {
	Event Event
	Err   error
}

// Stream is a live model response. Range over Events() for a streaming UI, OR call
// Message() to drain it into the fully-assembled assistant Message — not both (a
// Stream is consumed once).
type Stream struct {
	ch <-chan Emit
}

// NewStream is used by provider adapters to wrap their event channel.
func NewStream(ch <-chan Emit) *Stream { return &Stream{ch: ch} }

// Events returns a range-over-func iterator (Go 1.23+) over streamed events.
func (s *Stream) Events() iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		for e := range s.ch {
			if !yield(e.Event, e.Err) {
				return
			}
		}
	}
}

// Message drains the stream and returns the assembled assistant Message + usage.
func (s *Stream) Message() (Message, Usage, error) { return s.drain(nil) }

// drain assembles the stream into a Message, forwarding each event to onEvent (if
// non-nil) as it arrives. This is the shared path behind Message() and the Agent's
// token-streaming model call: the caller sees live deltas while the assembled
// message is still produced for the journal and middleware.
func (s *Stream) drain(onEvent func(Event)) (Message, Usage, error) {
	var b msgBuilder
	for ev, err := range s.Events() {
		if err != nil {
			return Message{}, Usage{}, err
		}
		if onEvent != nil {
			onEvent(ev)
		}
		b.add(ev)
	}
	msg, err := b.finalize()
	return msg, b.usage, err
}

// Generate is the convenience drain: stream and assemble in one call.
func Generate(ctx context.Context, m Model, req Request) (Message, Usage, error) {
	s, err := m.Stream(ctx, req)
	if err != nil {
		return Message{}, Usage{}, err
	}
	return s.Message()
}

// msgBuilder assembles streamed events into a Message.
type msgBuilder struct {
	reasoning    strings.Builder
	reasoningSig string
	text         strings.Builder
	calls        map[int]*ToolUse
	order        []int
	usage        Usage
}

func (b *msgBuilder) add(ev Event) {
	switch e := ev.(type) {
	case TextDelta:
		b.text.WriteString(e.Text)
	case ReasoningDelta:
		b.reasoning.WriteString(e.Text)
		if e.Signature != "" {
			b.reasoningSig = e.Signature
		}
	case ToolCallDelta:
		if b.calls == nil {
			b.calls = map[int]*ToolUse{}
		}
		tu, ok := b.calls[e.Index]
		if !ok {
			tu = &ToolUse{}
			b.calls[e.Index] = tu
			b.order = append(b.order, e.Index)
		}
		if e.ID != "" {
			tu.ID = e.ID
		}
		if e.Name != "" {
			tu.Name = e.Name
		}
		tu.Args = append(tu.Args, e.ArgsFragment...) // fragments concatenated; validated in finalize()
	case Finish:
		b.usage = e.Usage
	}
}

// finalize assembles the streamed events into a Message, validating that each tool
// call's concatenated argument fragments form complete JSON. A truncated stream
// yields invalid JSON — surface it rather than hand malformed args to a tool.
// (v1 json.Valid today; swaps to jsontext when we adopt json/v2 at the model layer.)
func (b *msgBuilder) finalize() (Message, error) {
	var parts []Part
	if b.reasoning.Len() > 0 || b.reasoningSig != "" {
		parts = append(parts, Reasoning{Text: b.reasoning.String(), Signature: b.reasoningSig})
	}
	if b.text.Len() > 0 {
		parts = append(parts, Text{Text: b.text.String()})
	}
	for _, i := range b.order {
		tu := b.calls[i]
		if len(tu.Args) > 0 && !json.Valid(tu.Args) {
			return Message{}, fmt.Errorf("tool call %q: %w: %s", tu.Name, ErrTruncatedToolArgs, tu.Args)
		}
		parts = append(parts, *tu)
	}
	return Message{Role: RoleAssistant, Parts: parts}, nil
}
