package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/internal/journalhook"
)

// The prefixes of a plan flow's keys are reserved, so a Step inside a node's body cannot name a
// node's result, a branch choice or the topology digest.
func TestPlanPrefixesAreReserved(t *testing.T) {
	for _, name := range []string{"node:x", "node:iter:0:x", "switch:x", "flow:digest"} {
		if !IsReservedStepName(name) {
			t.Errorf("%q is not reserved", name)
		}
		if _, err := Step(context.Background(), NewMemStore(), "r", name, func(context.Context) (int, error) { return 1, nil }); !errors.Is(err, ErrConfig) {
			t.Errorf("Step(%q): err = %v, want ErrConfig", name, err)
		}
	}
}

func TestPlanNodeStep(t *testing.T) {
	for name, want := range map[string]bool{
		"node:a":            true,
		"node:iter":         true,
		"node:iter:0:a":     true,
		"node:iter:12:iter": true,
		"node:":             false,
		"node:a:b":          false,
		"node:iter:x:a":     false,
		"node:iter::a":      false,
		"node:iter:1:":      false,
		"node:iter:1:a:b":   false,
		"switch:a":          false,
		"a":                 false,
		"attempt:step:a":    false,
	} {
		if got := planNodeStep(name); got != want {
			t.Errorf("planNodeStep(%q) = %v, want %v", name, got, want)
		}
	}
}

// The engine step hook runs a Step only under a plan node's key, with an agent.Safety.
func TestJournalhookStepRefusesOtherNames(t *testing.T) {
	ran := 0
	fn := func(context.Context) (json.RawMessage, error) { ran++; return json.RawMessage(`1`), nil }
	for _, name := range []string{"x", "switch:x", "flow:digest", "run:start", "attempt:step:node:x", "node:a:b"} {
		if _, err := journalhook.Step(context.Background(), NewMemStore(), "r", name, Safety{}, fn); !errors.Is(err, ErrConfig) {
			t.Errorf("journalhook.Step(%q): err = %v, want ErrConfig", name, err)
		}
	}
	if _, err := journalhook.Step(context.Background(), NewMemStore(), "r", "node:x", "not a safety", fn); !errors.Is(err, ErrConfig) {
		t.Errorf("journalhook.Step with a non-Safety: err = %v, want ErrConfig", err)
	}
	if ran != 0 {
		t.Fatalf("a refused hook call ran its body %d times", ran)
	}
	got, err := journalhook.Step(context.Background(), NewMemStore(), "r", "node:x", Safety{}, fn)
	if err != nil || string(got) != "1" || ran != 1 {
		t.Fatalf("journalhook.Step(node:x) = %s, %v (ran %d); want 1, nil, 1", got, err, ran)
	}
}

// A run's start holds its kind: an agent run (whose start records no kind) and a flow run never
// drive each other's journal, and a flow run is held to its flow's name.
func TestRunStartHoldsKindAndFlow(t *testing.T) {
	ctx := context.Background()
	flow := RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: "1"}
	for _, tc := range []struct {
		name        string
		first, next RunStart
		ok          bool
	}{
		{"same flow", flow, flow, true},
		{"agent then agent", RunStart{Input: "1"}, RunStart{Input: "1"}, true},
		{"explicit agent kind", RunStart{Input: "1"}, RunStart{Kind: RunKindAgent, Input: "1"}, true},
		{"agent then flow", RunStart{Input: "1"}, flow, false},
		{"flow then agent", flow, RunStart{Input: "1"}, false},
		{"another flow", flow, RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "g"}, Input: "1"}, false},
		{"flow with no name", flow, RunStart{Kind: RunKindFlow, Input: "1"}, false},
		{"another input", flow, RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: "2"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMemStore()
			if err := journalhook.HoldStart(ctx, m, "r", tc.first); err != nil {
				t.Fatal(err)
			}
			err := journalhook.HoldStart(ctx, m, "r", tc.next)
			if tc.ok != (err == nil) || err != nil && !errors.Is(err, ErrConfig) {
				t.Fatalf("second drive: err = %v, want ok=%v (else ErrConfig)", err, tc.ok)
			}
		})
	}
}

// An agent run's start encodes as before, with no kind, and a flow's start names its kind and flow.
func TestRunStartEncoding(t *testing.T) {
	for _, tc := range []struct {
		start RunStart
		want  string
	}{
		{RunStart{Input: "hi"}, `{"input":"hi"}`},
		{RunStart{Input: "hi", Saga: true}, `{"input":"hi","saga":true}`},
		{RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: `{"a":1}`}, `{"input":"{\"a\":1}","kind":"flow","flow":{"name":"f"}}`},
	} {
		b, err := marshalJournal(tc.start)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("RunStart %+v encodes as %s, want %s", tc.start, b, tc.want)
		}
	}
}
