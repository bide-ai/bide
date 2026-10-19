package plan

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// withoutUnused is reg with the blocks cfg does not use removed: the Registry a program that
// registered only what its config uses would have.
func withoutUnused(t *testing.T, reg *Registry, cfg string) *Registry {
	t.Helper()
	var c struct {
		Nodes []struct{ Block string } `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(cfg), &c); err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	for _, n := range c.Nodes {
		used[n.Block] = true
	}
	out := NewRegistry()
	for n, b := range reg.blocks {
		if used[n] {
			out.blocks[n] = b
		}
	}
	for n, p := range reg.preds {
		out.preds[n] = p
	}
	for n, m := range reg.merges {
		out.merges[n] = m
	}
	return out
}

// A registered block the config does not use makes assemble stop before it builds anything, so the
// report has no kinds, types, safety or digest for the whole flow. An editor with a palette always
// has unused blocks; AllowUnusedBlocks gives it the report it would have had without them.
func TestInspectConfigUnusedBlocks(t *testing.T) {
	reg := triageRegistry(t)
	if err := reg.RegisterStep("spare", cfgClassify); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(triageConfig)
	plain := InspectConfig(cfg, reg)
	if !plain.Errors() || plain.Digest != "" || !strings.Contains(plain.Problems[0].Message, `block "spare" is registered but never used`) {
		t.Fatalf("without the option: %+v digest %q", plain.Problems, plain.Digest)
	}
	if plain.Topology.Nodes[0].Kind != "" {
		t.Errorf("a report that stopped before building has node kinds: %+v", plain.Topology.Nodes[0])
	}
	got := InspectConfig(cfg, reg, AllowUnusedBlocks())
	want := InspectConfig(cfg, withoutUnused(t, reg, triageConfig))
	if got.Errors() || got.Digest == "" || got.Topology.Nodes[0].Kind == "" {
		t.Fatalf("with the option: %+v digest %q", got.Problems, got.Digest)
	}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if string(a) != string(b) {
		t.Errorf("not the report of the used blocks alone:\n%s\n%s", a, b)
	}
	// Load and Validate keep refusing it.
	if err := Validate(cfg, reg); err == nil || !strings.Contains(err.Error(), "registered but never used") {
		t.Errorf("Validate: %v", err)
	}
	if _, err := Load[cfgOrder, cfgReceipt](cfg, reg); err == nil || !strings.Contains(err.Error(), "registered but never used") {
		t.Errorf("Load: %v", err)
	}
}

func unknown(r ConfigReport) bool {
	for _, p := range r.Problems {
		if strings.Contains(p.Message, "unknown block") {
			return true
		}
	}
	return false
}

// The option forgives only the unused block: every other problem is still reported, and on every
// variant of every corpus config the report equals that of a Registry holding only the used blocks.
func TestInspectConfigAllowUnusedBlocksChangesNothingElse(t *testing.T) {
	total := 0
	for _, c := range describedCases() {
		reg := c.reg(t)
		if err := reg.RegisterStep("spare", func(_ context.Context, n dsIn) (dsIn, error) { return n, nil }); err != nil {
			t.Fatal(err)
		}
		d := reg.Describe()
		for _, cfg := range variants(t, c.cfg, reg) {
			want := InspectConfig([]byte(cfg), withoutUnused(t, reg, cfg))
			got := InspectConfig([]byte(cfg), reg, AllowUnusedBlocks())
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(want)
			if unknown(got) {
				// The list of registered blocks an unknown-block message offers names the blocks
				// the Registry holds, which differ by design; everything else must agree.
				if len(got.Problems) != len(want.Problems) || got.Digest != want.Digest || got.Typed != want.Typed {
					t.Fatalf("%s: differs for\n%s\n%s\n%s", c.name, cfg, a, b)
				}
				for i := range got.Problems {
					if got.Problems[i].Node != want.Problems[i].Node || got.Problems[i].Severity != want.Problems[i].Severity {
						t.Fatalf("%s: problem %d differs for\n%s\n%s\n%s", c.name, i, cfg, a, b)
					}
				}
				continue
			}
			if string(a) != string(b) {
				t.Fatalf("%s: differs for\n%s\n%s\n%s", c.name, cfg, a, b)
			}
			gd := InspectConfigDescribed([]byte(cfg), d, AllowUnusedBlocks())
			if unknown(got) {
				continue
			}
			if x, y := normalized(t, got, reg), normalized(t, gd, reg); x != y {
				t.Fatalf("%s: described differs for\n%s\n%s\n%s", c.name, cfg, x, y)
			}
			total++
		}
	}
	if total < 250 {
		t.Errorf("only %d configs", total)
	}
}
