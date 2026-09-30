package agent

import (
	"context"
	"testing"
)

// describedModel is a Model that describes itself.
type describedModel struct{ info ModelInfo }

func (describedModel) Stream(context.Context, Request) (*Stream, error) { return nil, nil }
func (m describedModel) Describe() ModelInfo                            { return m.info }

// plainModel is a Model that neither describes nor unwraps.
type plainModel struct{}

func (plainModel) Stream(context.Context, Request) (*Stream, error) { return nil, nil }

// unwrapModel wraps inner without describing itself.
type unwrapModel struct{ inner Model }

func (unwrapModel) Stream(context.Context, Request) (*Stream, error) { return nil, nil }
func (m unwrapModel) Unwrap() Model                                  { return m.inner }

// selfModel unwraps to itself.
type selfModel struct{}

func (selfModel) Stream(context.Context, Request) (*Stream, error) { return nil, nil }
func (m selfModel) Unwrap() Model                                  { return m }

// describingWrapper describes itself and also unwraps: its own description wins.
type describingWrapper struct {
	unwrapModel
	info ModelInfo
}

func (m describingWrapper) Describe() ModelInfo { return m.info }

func wrapN(m Model, n int) Model {
	for range n {
		m = unwrapModel{m}
	}
	return m
}

func TestModelInfoOf(t *testing.T) {
	inner := ModelInfo{Provider: "p", Model: "m", ResponseFormat: true}
	outer := ModelInfo{Provider: "outer", Model: "o"}
	for name, tc := range map[string]struct {
		m    Model
		want ModelInfo
		ok   bool
	}{
		"describer":                  {describedModel{inner}, inner, true},
		"one wrapper":                {wrapN(describedModel{inner}, 1), inner, true},
		"three wrappers":             {wrapN(describedModel{inner}, 3), inner, true},
		"the outermost describer":    {describingWrapper{unwrapModel{describedModel{inner}}, outer}, outer, true},
		"no describer":               {plainModel{}, ModelInfo{}, false},
		"wrapper of a plain model":   {wrapN(plainModel{}, 2), ModelInfo{}, false},
		"nil":                        {nil, ModelInfo{}, false},
		"unwraps to nil":             {unwrapModel{nil}, ModelInfo{}, false},
		"cycle":                      {selfModel{}, ModelInfo{}, false},
		"describer at the last link": {wrapN(describedModel{inner}, maxUnwrap-1), inner, true},
		"describer past the bound":   {wrapN(describedModel{inner}, maxUnwrap), ModelInfo{}, false},
	} {
		got, ok := ModelInfoOf(tc.m)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: ModelInfoOf = %+v, %v; want %+v, %v", name, got, ok, tc.want, tc.ok)
		}
	}
}
