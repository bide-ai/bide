package gemini

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// Describe names the provider and the model ID the requests carry, default or configured, and
// ModelInfoOf finds it on the adapter and through a wrapper that unwraps to it.
func TestDescribe(t *testing.T) {
	for _, tc := range []struct {
		m    *Model
		want agent.ModelInfo
	}{
		{New("k"), agent.ModelInfo{Provider: "gemini", Model: "gemini-2.0-flash", ResponseFormat: true}},
		{New("k", WithModel("custom-model")), agent.ModelInfo{Provider: "gemini", Model: "custom-model", ResponseFormat: true}},
	} {
		if got := tc.m.Describe(); got != tc.want {
			t.Errorf("Describe() = %+v, want %+v", got, tc.want)
		}
		if got, ok := agent.ModelInfoOf(tc.m); !ok || got != tc.want {
			t.Errorf("ModelInfoOf(adapter) = %+v, %v; want %+v, true", got, ok, tc.want)
		}
		if got, ok := agent.ModelInfoOf(wrapped{tc.m}); !ok || got != tc.want {
			t.Errorf("ModelInfoOf(wrapper) = %+v, %v; want %+v, true", got, ok, tc.want)
		}
	}
}

// wrapped is a Model wrapper that does not describe itself but unwraps to the adapter.
type wrapped struct{ agent.Model }

func (w wrapped) Unwrap() agent.Model { return w.Model }

// Describe's ResponseFormat is what the adapter does with a request that sets one: it is true
// only if such a request is built rather than refused with agent.ErrConfig.
func TestDescribe_ResponseFormatMatchesTheAdapter(t *testing.T) {
	m := New("k")
	req := agent.Request{
		Messages:       []agent.Message{agent.UserText("hi")},
		ResponseFormat: &agent.ResponseFormat{Name: "out", Schema: []byte(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`)},
	}
	_, err := m.buildRequest(req)
	if supported := err == nil; supported != m.Describe().ResponseFormat {
		t.Fatalf("Describe().ResponseFormat = %v, but buildRequest with a response format returned %v", m.Describe().ResponseFormat, err)
	}
	if err != nil && !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("refused response format: err %v, want agent.ErrConfig", err)
	}
}
