package openai

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// events drains a stream into its events and the error that ended it (nil after a Finish).
func events(src string) ([]agent.Event, error) {
	var evs []agent.Event
	for ev, err := range testStream(src).Events() {
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
	}
	return evs, nil
}

// An OpenAI-compatible server that reports usage on every chunk (vLLM with continuous usage
// stats, and similar) sends usage before the turn is over. A connection that drops after such a
// chunk has not finished the turn: the truncated text must read as ErrIncompleteResponse, not as
// the model's complete answer.
func TestStreamSSE_UsageChunkIsNotEndOfTurn(t *testing.T) {
	cut := `data: {"choices":[{"delta":{"content":"Your refund is appr"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}

`
	msg, _, err := testStream(cut).Message()
	if !errors.Is(err, agent.ErrIncompleteResponse) {
		t.Fatalf("truncated stream (usage, no finish_reason, no [DONE]) = %q, %v; want ErrIncompleteResponse", msg.Text(), err)
	}
}

// An empty finish_reason is the same as null (some compatible servers send "" on every chunk
// before the last): it does not end the turn.
func TestStreamSSE_EmptyFinishReasonIsNotEndOfTurn(t *testing.T) {
	cut := "data: {\"choices\":[{\"delta\":{\"content\":\"Your refund is appr\"},\"finish_reason\":\"\"}]}\n\n"
	msg, _, err := testStream(cut).Message()
	if !errors.Is(err, agent.ErrIncompleteResponse) {
		t.Fatalf("truncated stream (empty finish_reason) = %q, %v; want ErrIncompleteResponse", msg.Text(), err)
	}
}

// With usage on every chunk, the turn still ends with exactly one Finish, last, carrying the
// finish_reason and the last usage the server reported.
func TestStreamSSE_ContinuousUsageFinishesOnceWithLastUsage(t *testing.T) {
	src := `data: {"choices":[{"delta":{"content":"Your refund "}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}

data: {"choices":[{"delta":{"content":"is approved."}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}

data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2}}}

data: [DONE]

`
	evs, err := events(src)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	var finishes []agent.Finish
	for i, ev := range evs {
		if f, ok := ev.(agent.Finish); ok {
			finishes = append(finishes, f)
			if i != len(evs)-1 {
				t.Errorf("Finish at event %d of %d; it must be last", i, len(evs))
			}
		}
	}
	want := agent.Finish{Reason: "stop", Usage: agent.Usage{InputTokens: 3, OutputTokens: 3, CacheReadTokens: 2}}
	if len(finishes) != 1 || finishes[0] != want {
		t.Fatalf("finishes = %+v, want exactly [%+v]", finishes, want)
	}
}

// The usage-only chunk OpenAI sends after the finish_reason chunk (stream_options.include_usage)
// is reported even when the stream then ends without [DONE].
func TestStreamSSE_UsageAfterFinishReasonWithoutDone(t *testing.T) {
	src := `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":1}}

`
	msg, usage, err := testStream(src).Message()
	if err != nil || msg.Text() != "ok" || usage != (agent.Usage{InputTokens: 4, OutputTokens: 1}) {
		t.Fatalf("got %q, %+v, %v; want \"ok\", {4 1}, nil", msg.Text(), usage, err)
	}
}

// [DONE] alone ends the turn (a server that sends neither finish_reason nor usage).
func TestStreamSSE_DoneWithoutFinishReason(t *testing.T) {
	msg, _, err := testStream("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n").Message()
	if err != nil || msg.Text() != "ok" {
		t.Fatalf("got %q, %v; want \"ok\", nil", msg.Text(), err)
	}
}

// Content after the turn's finish_reason is a protocol violation, not more of the answer: the
// stream fails with an ErrModel instead of returning text the finished turn did not contain.
func TestStreamSSE_ContentAfterFinishReasonIsAnError(t *testing.T) {
	for name, after := range map[string]string{
		"text":       `{"choices":[{"delta":{"content":" and more"}}]}`,
		"reasoning":  `{"choices":[{"delta":{"reasoning_content":"hmm"}}]}`,
		"tool call":  `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":"{}"}}]}}]}`,
		"new reason": `{"choices":[{"delta":{},"finish_reason":"length"}]}`,
	} {
		src := "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: " + after + "\n\ndata: [DONE]\n\n"
		msg, _, err := testStream(src).Message()
		if err == nil || !errors.Is(err, agent.ErrModel) || errors.Is(err, agent.ErrIncompleteResponse) {
			t.Errorf("%s after finish_reason: got %q, %v; want a protocol ErrModel", name, msg.Text(), err)
		}
	}
}

// A repeated finish_reason with no content (a server that restates the final chunk to attach
// usage) is not new content and keeps the turn.
func TestStreamSSE_RepeatedFinishReasonIsAccepted(t *testing.T) {
	src := `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1}}

data: [DONE]

`
	msg, usage, err := testStream(src).Message()
	if err != nil || msg.Text() != "ok" || usage.OutputTokens != 1 {
		t.Fatalf("got %q, %+v, %v", msg.Text(), usage, err)
	}
}

// An empty finish_reason after the turn's reason is null, not a second reason.
func TestStreamSSE_EmptyFinishReasonAfterReasonIsAccepted(t *testing.T) {
	src := `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}

data: {"choices":[{"delta":{},"finish_reason":""}],"usage":{"prompt_tokens":4,"completion_tokens":1}}

data: [DONE]

`
	evs, err := events(src)
	if err != nil || len(evs) != 2 || evs[1] != (agent.Finish{Reason: "stop", Usage: agent.Usage{InputTokens: 4, OutputTokens: 1}}) {
		t.Fatalf("got %+v, %v", evs, err)
	}
}

// Usage is kept from the last chunk that carried it: a final chunk without usage does not erase
// what an earlier chunk reported.
func TestStreamSSE_LastUsageSurvivesChunksWithoutUsage(t *testing.T) {
	src := `data: {"choices":[{"delta":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":1}}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	_, usage, err := testStream(src).Message()
	if err != nil || usage != (agent.Usage{InputTokens: 4, OutputTokens: 1}) {
		t.Fatalf("usage = %+v, %v; want {4 1}", usage, err)
	}
}
