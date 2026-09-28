package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/blackwell-systems/bide/agent"
)

func f64(v float64) *float64 { return &v }
func iptr(v int) *int        { return &v }
func i64(v int64) *int64     { return &v }

// Sampling maps onto the Anthropic wire payload: stop_sequences (not stop), max_tokens
// override, and Seed intentionally dropped (Anthropic has no seed).
func TestBuildRequest_Sampling(t *testing.T) {
	m := New("k", WithMaxTokens(4096))
	body, err := m.buildRequest(agent.Request{
		Messages: []agent.Message{agent.UserText("hi")},
		Sampling: agent.Sampling{
			Temperature: f64(0),
			TopP:        f64(0.5),
			MaxTokens:   iptr(200),
			Stop:        []string{"STOP"},
			Seed:        i64(7), // unsupported → must not appear
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
	if p["top_p"] != 0.5 {
		t.Fatalf("top_p = %v, want 0.5", p["top_p"])
	}
	if p["max_tokens"] != float64(200) {
		t.Fatalf("max_tokens = %v, want 200 (override)", p["max_tokens"])
	}
	if seqs, ok := p["stop_sequences"].([]any); !ok || len(seqs) != 1 || seqs[0] != "STOP" {
		t.Fatalf("stop_sequences = %v, want [STOP]", p["stop_sequences"])
	}
	if _, present := p["seed"]; present {
		t.Fatal("seed must not appear (Anthropic has no seed)")
	}
	if _, present := p["stop"]; present {
		t.Fatal("Anthropic uses stop_sequences, not stop")
	}
}
