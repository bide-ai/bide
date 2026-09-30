package plan

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// safetyRegistry registers a tool "t" with the given Go-declared Safety.
func safetyRegistry(t *testing.T, s agent.Safety) *Registry {
	t.Helper()
	reg := NewRegistry()
	tool := agent.Func("t", "", s, func(context.Context, int) (int, error) { return 0, nil })
	if err := RegisterTool[int, int](reg, "t", tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

func safetyConfig(s string) string {
	return `{"flow":"f","nodes":[{"name":"t","block":"t","safety":"` + s + `"}],"wiring":[]}`
}

// Only Go code can say a step is safe to run twice. A config "safety" that would make a node
// more retry-safe than its Go registration declares (a side effect marked readonly or
// idempotent, or an idempotent tool marked readonly) is a load error naming the node.
func TestLoad_ConfigCannotRaiseRetrySafety(t *testing.T) {
	key := func(json.RawMessage) string { return "k" }
	for name, tc := range map[string]struct {
		base agent.Safety
		cfg  string
	}{
		"side effect to readonly":   {agent.Safety{}, "readonly"},
		"side effect to idempotent": {agent.Safety{}, "idempotent"},
		"side effect to retryable":  {agent.Safety{}, "retryable"},
		"idempotent to readonly":    {agent.Safety{Idempotent: true}, "readonly"},
		"keyed to readonly":         {agent.Safety{IdempotencyKey: key}, "readonly"},
	} {
		_, err := Load[int, int]([]byte(safetyConfig(tc.cfg)), safetyRegistry(t, tc.base))
		if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `"t"`) {
			t.Errorf("%s: Load = %v; want ErrConfig naming node \"t\"", name, err)
		}
	}
}

// A join's merge block is Go code registered without a Safety, so a config cannot mark the join
// retry-safe either.
func TestLoad_ConfigCannotRaiseAJoinsRetrySafety(t *testing.T) {
	cfg := strings.Replace(diamondConfig, `"merge": "mergeBlock"`, `"merge": "mergeBlock", "safety": "readonly"`, 1)
	if _, err := Load[int, string]([]byte(cfg), diamondRegistry(t)); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("join marked readonly over a merge registered as a side effect: Load = %v; want ErrConfig", err)
	}
}

// A config may lower retry safety: mark a readonly tool idempotent, or mark any retry-safe tool
// "side_effect" so a crash with no recorded outcome halts instead of re-running it. Lowering to
// side_effect clears an IdempotencyKey too, since the key alone makes a node retry-safe.
func TestLoad_ConfigMayLowerRetrySafety(t *testing.T) {
	key := func(json.RawMessage) string { return "k" }
	for name, tc := range map[string]struct {
		base      agent.Safety
		cfg       string
		wantRetry bool
		wantRO    bool
	}{
		"readonly to idempotent":    {agent.Safety{ReadOnly: true}, "idempotent", true, false},
		"readonly to side effect":   {agent.Safety{ReadOnly: true}, "side_effect", false, false},
		"idempotent to side effect": {agent.Safety{Idempotent: true}, "side_effect", false, false},
		"keyed to side effect":      {agent.Safety{IdempotencyKey: key}, "side_effect", false, false},
		"readonly stays readonly":   {agent.Safety{ReadOnly: true}, "readonly", true, true},
		"side effect stays":         {agent.Safety{}, "side_effect", false, false},
	} {
		flow, err := Load[int, int]([]byte(safetyConfig(tc.cfg)), safetyRegistry(t, tc.base))
		if err != nil {
			t.Errorf("%s: Load: %v", name, err)
			continue
		}
		s := flow.core.byName["t"].safety
		if nodeRetriableOnResume(s) != tc.wantRetry || s.ReadOnly != tc.wantRO {
			t.Errorf("%s: safety %+v; want retry-safe=%v ReadOnly=%v", name, s, tc.wantRetry, tc.wantRO)
		}
	}
}
