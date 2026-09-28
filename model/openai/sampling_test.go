package openai

import (
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func f64(v float64) *float64 { return &v }
func i(v int) *int           { return &v }
func i64(v int64) *int64     { return &v }

// Sampling fields map onto the OpenAI wire payload; unset fields are omitted.
func TestBuildRequest_Sampling(t *testing.T) {
	m := New("k", WithModel("gpt-4o"))
	body, err := m.buildRequest(agent.Request{
		Messages: []agent.Message{agent.UserText("hi")},
		Sampling: agent.Sampling{
			Temperature: f64(0), // explicit 0 must appear
			TopP:        f64(0.9),
			MaxTokens:   i(500),
			Seed:        i64(42),
			Stop:        []string{"END"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p["temperature"] != 0.0 {
		t.Fatalf("temperature = %v, want 0", p["temperature"])
	}
	if p["top_p"] != 0.9 {
		t.Fatalf("top_p = %v, want 0.9", p["top_p"])
	}
	if p["max_tokens"] != float64(500) {
		t.Fatalf("max_tokens = %v, want 500", p["max_tokens"])
	}
	if p["seed"] != float64(42) {
		t.Fatalf("seed = %v, want 42", p["seed"])
	}
	if stop, ok := p["stop"].([]any); !ok || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop = %v, want [END]", p["stop"])
	}
}

// With no Sampling set, sampling keys are absent (provider defaults apply).
func TestBuildRequest_NoSampling(t *testing.T) {
	m := New("k")
	body, _ := m.buildRequest(agent.Request{Messages: []agent.Message{agent.UserText("hi")}})
	var p map[string]any
	_ = json.Unmarshal(body, &p)
	for _, k := range []string{"temperature", "top_p", "seed", "stop"} {
		if _, present := p[k]; present {
			t.Fatalf("key %q should be absent when unset", k)
		}
	}
}

// Request-level MaxTokens overrides the adapter's construction default.
func TestBuildRequest_MaxTokensOverride(t *testing.T) {
	m := New("k", WithMaxTokens(100))
	body, _ := m.buildRequest(agent.Request{
		Messages: []agent.Message{agent.UserText("hi")},
		Sampling: agent.Sampling{MaxTokens: i(999)},
	})
	var p map[string]any
	_ = json.Unmarshal(body, &p)
	if p["max_tokens"] != float64(999) {
		t.Fatalf("max_tokens = %v, want 999 (request overrides default)", p["max_tokens"])
	}
}
