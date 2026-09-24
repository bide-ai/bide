package agent

import (
	"context"
	"testing"
)

// captureModel records the last Request it was asked to stream, then delegates.
type captureModel struct {
	inner Model
	got   *Request
}

func (m *captureModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	*m.got = req
	return m.inner.Stream(ctx, req)
}

// WithSampling options flow into the Request the model receives.
func TestSampling_FlowsIntoRequest(t *testing.T) {
	var got Request
	m := &captureModel{inner: &scriptModel{turns: [][]Emit{textTurn("ok")}}, got: &got}
	a := New(m, NewMemStore()).WithSampling(
		Temperature(0),
		MaxTokens(500),
		TopP(0.9),
		Stop("END"),
		Seed(42),
	)

	if _, err := a.Run(context.Background(), "r", "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	s := got.Sampling
	if s.Temperature == nil || *s.Temperature != 0 {
		t.Fatalf("Temperature = %v, want set to 0", s.Temperature)
	}
	if s.MaxTokens == nil || *s.MaxTokens != 500 {
		t.Fatalf("MaxTokens = %v, want 500", s.MaxTokens)
	}
	if s.TopP == nil || *s.TopP != 0.9 {
		t.Fatalf("TopP = %v, want 0.9", s.TopP)
	}
	if len(s.Stop) != 1 || s.Stop[0] != "END" {
		t.Fatalf("Stop = %v, want [END]", s.Stop)
	}
	if s.Seed == nil || *s.Seed != 42 {
		t.Fatalf("Seed = %v, want 42", s.Seed)
	}
}

// Unset sampling stays nil (provider defaults), so an explicit 0 is distinguishable.
func TestSampling_UnsetIsNil(t *testing.T) {
	var got Request
	m := &captureModel{inner: &scriptModel{turns: [][]Emit{textTurn("ok")}}, got: &got}
	a := New(m, NewMemStore())

	if _, err := a.Run(context.Background(), "r", "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Sampling.Temperature != nil || got.Sampling.MaxTokens != nil {
		t.Fatal("unset sampling fields should be nil")
	}
}
