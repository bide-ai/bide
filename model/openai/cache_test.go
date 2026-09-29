package openai

import "testing"

// OpenAI caches prefixes automatically; its cached_tokens surface in agent.Usage.
func TestStreamSSE_ReportsCachedTokens(t *testing.T) {
	const stream = `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":800}}}

data: [DONE]

`
	_, u, err := testStream(stream).Message()
	if err != nil {
		t.Fatal(err)
	}
	// prompt_tokens 1000 includes the 800 cached tokens, which are counted once, as cache reads.
	if u.InputTokens != 200 || u.OutputTokens != 5 {
		t.Fatalf("usage = %+v", u)
	}
	if u.CacheReadTokens != 800 {
		t.Errorf("CacheReadTokens = %d, want 800", u.CacheReadTokens)
	}
	if u.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d, want 0 (OpenAI has no explicit cache write)", u.CacheWriteTokens)
	}
}
