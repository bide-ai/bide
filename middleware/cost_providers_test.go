package middleware

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/anthropic"
	"github.com/bide-ai/bide/model/gemini"
	"github.com/bide-ai/bide/model/openai"
)

// The same call through each provider: 1,000 input tokens of which 800 are served from the
// prompt cache, and 50 output tokens. Each provider reports that in its own wire format.
// Anthropic's input_tokens excludes cached tokens; OpenAI's prompt_tokens and Gemini's
// promptTokenCount include them. agent.Usage must mean the same thing whichever adapter
// produced it, so the cost of the call is the same.
func TestCost_SameCallCostsTheSameOnEveryProvider(t *testing.T) {
	sse := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "text/event-stream")
			io.WriteString(w, body)
		}))
	}
	providers := map[string]struct {
		body  string
		model func(url string) agent.Model
	}{
		"anthropic": {
			body: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":200,\"cache_read_input_tokens\":800}}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":50}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			model: func(url string) agent.Model { return anthropic.New("k", anthropic.WithBaseURL(url)) },
		},
		"openai": {
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":50,\"prompt_tokens_details\":{\"cached_tokens\":800}}}\n\n" +
				"data: [DONE]\n\n",
			model: func(url string) agent.Model { return openai.New("k", openai.WithBaseURL(url)) },
		},
		"gemini": {
			body:  "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1000,\"candidatesTokenCount\":50,\"cachedContentTokenCount\":800}}\n\n",
			model: func(url string) agent.Model { return gemini.New("k", gemini.WithBaseURL(url)) },
		},
	}
	rates := Rates{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.30}
	want := (200*3 + 50*15 + 800*0.30) / 1e6
	for name, p := range providers {
		t.Run(name, func(t *testing.T) {
			srv := sse(p.body)
			defer srv.Close()
			_, u, err := agent.Generate(context.Background(), p.model(srv.URL), agent.Request{Messages: []agent.Message{agent.UserText("q")}})
			if err != nil {
				t.Fatal(err)
			}
			if got := rates.Cost(u); math.Abs(got-want) > 1e-12 {
				t.Errorf("cost = $%.6f, want $%.6f (usage %+v)", got, want, u)
			}
			if got := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens; got != 1000 {
				t.Errorf("total input tokens = %d, want 1000 (usage %+v)", got, u)
			}
		})
	}
}
