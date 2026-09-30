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
	return `{"version":1,"flow":"f","nodes":[{"name":"t","block":"t","safety":"` + s + `"}],"wiring":[]}`
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
		if s.RetrySafe() != tc.wantRetry || s.ReadOnly != tc.wantRO {
			t.Errorf("%s: safety %+v; want retry-safe=%v ReadOnly=%v", name, s, tc.wantRetry, tc.wantRO)
		}
	}
}

// A merge block registered retry-safe in Go (RegisterJoin2/RegisterJoin3 take NodeOptions) is a
// join a config may keep retry-safe or lower; with no option it is a side effect.
func TestRegisterJoin_CarriesGoSafety(t *testing.T) {
	merge2 := func(_ context.Context, a int, s string) (string, error) { return s, nil }
	for name, tc := range map[string]struct {
		opts      []NodeOption
		cfg       string
		wantErr   bool
		wantRetry bool
	}{
		"readonly kept":        {[]NodeOption{ReadOnly()}, "readonly", false, true},
		"readonly, no config":  {[]NodeOption{ReadOnly()}, "", false, true},
		"readonly lowered":     {[]NodeOption{ReadOnly()}, "side_effect", false, false},
		"idempotent raised":    {[]NodeOption{Idempotent()}, "readonly", true, false},
		"no option, no config": {nil, "", false, false},
	} {
		reg := NewRegistry()
		for _, err := range []error{
			RegisterStep(reg, "split", func(_ context.Context, n int) (int, error) { return n, nil }),
			RegisterStep(reg, "y", func(_ context.Context, n int) (int, error) { return n, nil }),
			RegisterStep(reg, "z", func(_ context.Context, n int) (string, error) { return "", nil }),
			RegisterJoin2(reg, "mergeBlock", merge2, tc.opts...),
		} {
			if err != nil {
				t.Fatal(err)
			}
		}
		cfg := diamondConfig
		if tc.cfg != "" {
			cfg = strings.Replace(cfg, `"merge": "mergeBlock"`, `"merge": "mergeBlock", "safety": "`+tc.cfg+`"`, 1)
		}
		flow, err := Load[int, string]([]byte(cfg), reg)
		if tc.wantErr {
			if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `"merge"`) {
				t.Errorf("%s: Load = %v; want ErrConfig naming join \"merge\"", name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Load: %v", name, err)
			continue
		}
		if got := flow.core.byName["merge"].safety.RetrySafe(); got != tc.wantRetry {
			t.Errorf("%s: join retry-safe = %v, want %v", name, got, tc.wantRetry)
		}
	}
	reg := NewRegistry()
	if err := RegisterJoin3(reg, "m3", func(_ context.Context, a, b, c int) (int, error) { return a, nil }, ReadOnly()); err != nil {
		t.Fatal(err)
	}
	if !reg.merges["m3"].safety.ReadOnly {
		t.Error("RegisterJoin3 dropped its ReadOnly option")
	}
}

// The error for an unknown value lists the new "side_effect" value, and one for a raise names
// both levels, so an author can tell what the config may say.
func TestLoad_SafetyErrorsNameTheLevels(t *testing.T) {
	_, err := Load[int, int]([]byte(safetyConfig("sometimes")), safetyRegistry(t, agent.Safety{}))
	if err == nil || !strings.Contains(err.Error(), `"side_effect"`) {
		t.Errorf("unknown value: %v; want the accepted values including side_effect", err)
	}
	_, err = Load[int, int]([]byte(safetyConfig("readonly")), safetyRegistry(t, agent.Safety{Idempotent: true}))
	if err == nil || !strings.Contains(err.Error(), `"readonly"`) || !strings.Contains(err.Error(), `"idempotent" its Go registration`) {
		t.Errorf("raise: %v; want both levels named", err)
	}
	_, err = Load[int, int]([]byte(safetyConfig("idempotent")), safetyRegistry(t, agent.Safety{}))
	if err == nil || !strings.Contains(err.Error(), `"side_effect" its Go registration`) {
		t.Errorf("raise from a side effect: %v; want side_effect named", err)
	}
}

// Each retry-safety level has one config spelling. The pre-v1 "retryable" alias of "idempotent" is
// refused, on a node and on a join, even where "idempotent" would load, with an error naming
// "idempotent".
func TestLoad_RetryableSpellingIsRefused(t *testing.T) {
	const want = `unknown safety "retryable"; for the idempotent level write "idempotent"`
	if _, err := Load[int, int]([]byte(safetyConfig("idempotent")), safetyRegistry(t, agent.Safety{ReadOnly: true})); err != nil {
		t.Fatalf("idempotent on a readonly tool: Load = %v; want it to load", err)
	}
	_, err := Load[int, int]([]byte(safetyConfig("retryable")), safetyRegistry(t, agent.Safety{ReadOnly: true}))
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `"t"`) || !strings.Contains(err.Error(), want) {
		t.Errorf("node: Load = %v; want an ErrConfig naming node \"t\" and containing %q", err, want)
	}
	reg := diamondRegistry(t)
	cfg := strings.Replace(diamondConfig, `"merge": "mergeBlock"`, `"merge": "mergeBlock", "safety": "retryable"`, 1)
	if _, err := Load[int, string]([]byte(cfg), reg); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), want) {
		t.Errorf("join: Load = %v; want an ErrConfig containing %q", err, want)
	}
}
