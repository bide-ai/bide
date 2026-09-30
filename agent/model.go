package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"sync"
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
	ToolChoice     *ToolChoice     // nil = provider default (auto); see ToolChoice
}

// ToolChoice controls whether and how the model may call tools on a request. Mode is one
// of "" or "auto" (model decides, the default), "none" (never call a tool), "required"
// (must call some tool), or "tool" (must call the specific tool named in Name). A nil
// *ToolChoice on a Request means the provider default (auto).
//
// CAVEAT: in the multi-turn agent loop, forcing "required" or "tool" on EVERY turn
// prevents the model from ever emitting a final text answer, so the loop never
// terminates. Those two modes are intended for single-turn or typed/structured calls
// where exactly one tool round-trip is expected. Use "auto" (the default) for the loop.
type ToolChoice struct {
	Mode string // "", "auto", "none", "required", or "tool"
	Name string // tool name to force; only meaningful when Mode == "tool"
}

// ResponseFormat asks the provider to constrain the model's output to a named JSON schema
// (OpenAI structured outputs / "json_schema" response format). An adapter that does not
// support it (Anthropic) fails the request with ErrConfig rather than drop the constraint.
// Name is the schema name; Schema is provider-neutral JSON Schema (as from the schema
// package). See RunTypedNative.
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

// Usage is token accounting for a call; middleware turns it into cost. The four counts are
// disjoint, whatever the provider: each input token is counted in exactly one of
// InputTokens, CacheReadTokens, or CacheWriteTokens, so each is billed once at its own rate
// and TotalInputTokens is the whole prompt. Providers differ on the wire (Anthropic reports
// uncached input separately; OpenAI and Gemini report a prompt total that includes cached
// tokens), and adapters normalize to this form.
type Usage struct {
	// InputTokens is input tokens neither read from nor written to the prompt cache.
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	// CacheReadTokens is input tokens served from the provider's prompt cache (billed at
	// a discount). CacheWriteTokens is input tokens written to the cache on this call
	// (Anthropic cache creation; 0 for providers that cache implicitly, like OpenAI).
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// TotalInputTokens is every input token the call processed, cached or not.
func (u Usage) TotalInputTokens() int { return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens }

// Validate reports an error wrapping ErrNegativeUsage when any count in u is negative.
func (u Usage) Validate() error {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 {
		return fmt.Errorf("%+v: %w", u, ErrNegativeUsage)
	}
	return nil
}

// billable is u with any negative count set to zero.
func (u Usage) billable() Usage {
	return Usage{max(u.InputTokens, 0), max(u.OutputTokens, 0), max(u.CacheReadTokens, 0), max(u.CacheWriteTokens, 0)}
}

// TotalTokens is every token the call processed: all input plus output.
func (u Usage) TotalTokens() int { return u.TotalInputTokens() + u.OutputTokens }

// Event is a normalized streamed model event. The stream/ package maps every
// provider wire format (OpenAI SSE / Anthropic typed / Gemini NDJSON / Bedrock
// binary) onto these.
type Event interface{ event() }

type TextDelta struct{ Text string }

func (TextDelta) event() {}

// ReasoningDelta is a fragment of model thinking. A Signature ends the thinking block it
// belongs to, so the next ReasoningDelta starts a new Reasoning part. Redacted carries a whole
// encrypted block (Anthropic redacted_thinking), which becomes a Reasoning part of its own.
type ReasoningDelta struct {
	Text      string
	Signature string // opaque provider token to echo back on later turns (Anthropic thinking)
	Redacted  string // encrypted reasoning to echo back unchanged (Anthropic redacted_thinking)
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
	Signature    string // opaque provider token for this call, kept on ToolUse.Signature (Gemini thoughtSignature)
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
//
// A consumer that stops early releases the producer: breaking out of Events() does it,
// and so does Close. Cancelling the context passed to Model.Stream also releases it.
type Stream struct {
	ch   <-chan Emit
	done chan struct{} // closed when the consumer stops reading
	stop sync.Once
	err  error // a terminal error set by the producer before ch closes (NewStreamFunc)
}

// NewStream wraps an event channel the caller fills and closes. It suits producers that
// buffer their whole response up front. A producer that sends as it reads (a network
// adapter) should use NewStreamFunc, so it is released when the consumer stops reading;
// a goroutine blocked sending on ch is not.
func NewStream(ch <-chan Emit) *Stream { return &Stream{ch: ch, done: make(chan struct{})} }

// NewStreamFunc runs produce in its own goroutine and returns the Stream it feeds.
// produce delivers each event through send, which blocks until the consumer takes it and
// returns false once the consumer has stopped reading (it broke out of Events or called
// Close) or ctx is cancelled; produce should then return promptly, releasing whatever it
// holds, such as a response body. The stream ends when produce returns.
//
// If ctx cancels an event the consumer has not yet taken, the stream ends with ctx's error,
// so a consumer never mistakes a response cut short by cancellation for a complete one.
func NewStreamFunc(ctx context.Context, produce func(send func(Emit) bool)) *Stream {
	ch := make(chan Emit)
	s := &Stream{ch: ch, done: make(chan struct{})}
	go func() {
		defer close(ch)
		var cancelled bool
		produce(func(e Emit) bool {
			// Checked first because select picks among ready cases at random: once ctx is done,
			// no further event is delivered, even to a consumer that is waiting for one.
			if cancelled || ctx.Err() != nil {
				cancelled = true
				return false
			}
			select {
			case ch <- e:
				return true
			case <-s.done:
				return false
			case <-ctx.Done():
				cancelled = true
				return false
			}
		})
		if cancelled {
			s.err = ctx.Err() // published to the consumer by close(ch)
		}
	}()
	return s
}

// Close tells the producer the consumer will read no further events, so it can stop and
// release its resources. It is safe to call more than once, and after the stream ends.
func (s *Stream) Close() { s.stop.Do(func() { close(s.done) }) }

// Events returns a range-over-func iterator (Go 1.23+) over streamed events. Breaking out
// of the loop closes the stream. A stream that ends without a Finish event yields
// ErrIncompleteResponse last: the response stopped partway through the turn. A Finish is the
// turn's last event: an event after it yields ErrStreamProtocol and ends the stream.
func (s *Stream) Events() iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		var finished bool
		for e := range s.ch {
			if e.Err != nil {
				yield(nil, e.Err)
				s.Close()
				return
			}
			if finished {
				yield(nil, fmt.Errorf("%T after the turn's Finish: %w", e.Event, ErrStreamProtocol))
				s.Close()
				return
			}
			if _, ok := e.Event.(Finish); ok {
				finished = true
			}
			if !yield(e.Event, nil) {
				s.Close()
				return
			}
		}
		switch {
		case s.err != nil:
			yield(nil, s.err)
		case !finished:
			yield(nil, ErrIncompleteResponse)
		}
	}
}

// Message drains the stream and returns the assembled assistant Message + usage. When the stream
// fails, the usage is what it reported before failing, which the provider may still bill.
func (s *Stream) Message() (Message, Usage, error) { return s.drain(nil) }

// drain assembles the stream into a Message, forwarding each event to onEvent (if
// non-nil) as it arrives. This is the shared path behind Message() and the Agent's
// token-streaming model call: the caller sees live deltas while the assembled
// message is still produced for the journal and middleware.
func (s *Stream) drain(onEvent func(Event)) (Message, Usage, error) {
	var b msgBuilder
	for ev, err := range s.Events() {
		if err != nil {
			return Message{}, b.usage.billable(), err // the usage reported before the stream failed was billed
		}
		if onEvent != nil {
			onEvent(ev)
		}
		b.add(ev)
	}
	msg, err := b.finalize()
	if err == nil {
		err = b.usage.Validate()
	}
	if err != nil {
		return Message{}, b.usage.billable(), err
	}
	return msg, b.usage, nil
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
	reasoning    []Reasoning     // completed thinking blocks, in order
	thinking     strings.Builder // the open thinking block's text
	thinkingOpen bool
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
		if e.Redacted != "" {
			b.closeThinking()
			b.reasoning = append(b.reasoning, Reasoning{Redacted: e.Redacted})
			break
		}
		b.thinkingOpen = true
		b.thinking.WriteString(e.Text)
		if e.Signature != "" {
			b.closeThinking(e.Signature)
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
		if e.Signature != "" {
			tu.Signature = e.Signature
		}
		tu.Args = append(tu.Args, e.ArgsFragment...) // fragments concatenated; validated in finalize()
	case Finish:
		b.usage = e.Usage
	}
}

// closeThinking ends the open thinking block, if any, as a Reasoning part with the given
// signature (at most one).
func (b *msgBuilder) closeThinking(signature ...string) {
	if !b.thinkingOpen {
		return
	}
	r := Reasoning{Text: b.thinking.String()}
	if len(signature) > 0 {
		r.Signature = signature[0]
	}
	b.reasoning = append(b.reasoning, r)
	b.thinking.Reset()
	b.thinkingOpen = false
}

// finalize assembles the streamed events into a Message, validating that each tool
// call's concatenated argument fragments form complete JSON. A truncated stream
// yields invalid JSON — surface it rather than hand malformed args to a tool.
// (v1 json.Valid today; swaps to jsontext when we adopt json/v2 at the model layer.)
func (b *msgBuilder) finalize() (Message, error) {
	var parts []Part
	b.closeThinking()
	for _, r := range b.reasoning {
		parts = append(parts, r)
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
