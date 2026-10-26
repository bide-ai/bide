package plan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// "safety" and "approval" lower or gate a node, so they belong on a node or a join, which creates one. On an
// edge or a switch element there is no node to apply them to; they were accepted and dropped, so an author
// who wrote an approval gate on a wire believed a node was gated and nothing was. They are refused, naming
// the element.
func TestWiringKeys_SafetyAndApprovalOnAnEdgeOrSwitchAreRefused(t *testing.T) {
	reg := NewRegistry()
	for _, n := range []string{"a", "b"} {
		if err := reg.RegisterStep[int, int](n, func(ctx context.Context, v int) (int, error) { return v, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.RegisterPredicate[int]("big", func(v int) bool { return v > 1 }); err != nil {
		t.Fatal(err)
	}
	const approval = `{"need":1,"approvers":["ops"]}`
	for what, c := range map[string]struct{ wire, key string }{
		"safety on an edge":    {`{"edge":["a","b"],"safety":"readonly"}`, "safety"},
		"approval on an edge":  {`{"edge":["a","b"],"approval":` + approval + `}`, "approval"},
		"safety on a switch":   {`{"switch":"a","when":[{"pred":"big","to":"b"}],"safety":"readonly"}`, "safety"},
		"approval on a switch": {`{"switch":"a","when":[{"pred":"big","to":"b"}],"approval":` + approval + `}`, "approval"},
	} {
		cfg := []byte(`{"version":1,"flow":"f","nodes":[{"name":"a","block":"a"},{"name":"b","block":"b"}],"wiring":[` + c.wire + `]}`)
		check := func(via string, err error) {
			t.Helper()
			if !errors.Is(err, agent.ErrConfig) || err == nil || !strings.Contains(err.Error(), "wiring[0]") || !strings.Contains(err.Error(), c.key) {
				t.Errorf("%s via %s = %v, want ErrConfig naming wiring[0] and %s", what, via, err, c.key)
			}
		}
		check("Validate", Validate(cfg, reg))
		_, err := Load[int, int](cfg, reg)
		check("Load", err)
		if r := InspectConfig(cfg, reg); !r.Errors() {
			t.Errorf("%s: InspectConfig reports no error", what)
		}
	}
}

// A join takes both keys, and a node takes them in nodes: the refusal is only for elements that have no node.
func TestWiringKeys_SafetyAndApprovalOnAJoinStillWork(t *testing.T) {
	reg := NewRegistry()
	for _, n := range []string{"a", "b"} {
		if err := reg.RegisterStep[int, int](n, func(ctx context.Context, v int) (int, error) { return v, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.RegisterJoin2[int, int, int]("merge", func(_ context.Context, x, y int) (int, error) { return x + y, nil }); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`{"version":1,"flow":"f","nodes":[{"name":"a","block":"a"},{"name":"b","block":"b"}],
 "wiring":[{"join":"j","inputs":["a","b"],"merge":"merge","safety":"side_effect","approval":{"need":1,"approvers":["ops"]}}]}`)
	var parsed config
	if err := parseConfig(cfg, &parsed); err != nil {
		t.Fatal(err)
	}
	core, err := assemble(&parsed, reg, false)
	if err != nil {
		t.Fatal(err)
	}
	if core.byName["j"].approval == nil {
		t.Error("the join's approval was dropped")
	}
}
