package runview

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bide-ai/bide/plan"
)

func gateCfg(g map[string]any) string {
	b, _ := json.Marshal(g)
	return `{"version": 1, "flow": "f", "nodes": [{"name": "a", "block": "a"}, {"name": "pay", "block": "pay", "approval": ` + string(b) + `}],
	  "wiring": [{"edge": ["a", "pay"]}]}`
}

func gp(timeout string, exclude bool) map[string]any {
	g := map[string]any{"need": 2, "approvers": []string{"ops", "finance", "risk"}}
	if timeout != "" {
		g["timeout"] = timeout
	}
	if exclude {
		g["exclude_principal"] = true
	}
	return g
}

// The config page shows a gate's deadline and separation of duties next to its need and approvers:
// in the approvals card, the node's tooltip and its box.
func TestConfigViewShowsTimeoutAndExclusion(t *testing.T) {
	v := BuildConfig(Config{Name: "c.json", Data: []byte(gateCfg(gp("24h", true)))})
	want := "pay: 2 of 3: ops, finance, risk excluding the requester, times out after 24h"
	if len(v.Approvals) != 1 || v.Approvals[0] != want {
		t.Fatalf("approvals = %q; want %q", v.Approvals, want)
	}
	var n GraphNode
	for _, x := range v.Graph.Nodes {
		if x.Name == "pay" {
			n = x
		}
	}
	if n.Gate != "2 of 3: ops, finance, risk excluding the requester, times out after 24h" || n.GateLine != "gated: 2 of 3 · SoD · 24h" {
		t.Errorf("node gate %q, line %q", n.Gate, n.GateLine)
	}
	if !strings.Contains(string(v.Graph.SVG), "times out after 24h") {
		t.Error("the node tooltip does not say the gate times out")
	}
	if m := v.Graph.Mermaid; !strings.Contains(m, `pay | approval: 2 of 3: ops, finance, risk excluding the requester, times out after 24h`) {
		t.Errorf("the Mermaid text does not carry the gate:\n%s", m)
	}
	plain := BuildConfig(Config{Name: "c.json", Data: []byte(gateCfg(gp("", false)))})
	if plain.Approvals[0] != "pay: 2 of 3: ops, finance, risk" {
		t.Errorf("a gate with neither: %q", plain.Approvals)
	}
}

// A timeout makes the gate fail closed sooner (adding or shortening one: strengthened); removing or
// lengthening one lets a run wait longer and accepts decisions the old gate refused as late
// (weakened). A change in two directions is not strictly stronger, so it is weakened.
func TestDiffTimeoutClassification(t *testing.T) {
	for _, c := range []struct {
		name         string
		from, to     map[string]any
		verdict, say string
	}{
		{"timeout added", gp("", false), gp("24h", false), "strengthened", "approval 2 of 3, no timeout -> 2 of 3, times out after 24h"},
		{"timeout shortened", gp("48h", false), gp("24h", false), "strengthened", "approval 2 of 3, times out after 48h -> 2 of 3, times out after 24h"},
		{"timeout removed", gp("24h", false), gp("", false), "weakened", "approval 2 of 3, times out after 24h -> 2 of 3, no timeout"},
		{"timeout lengthened", gp("24h", false), gp("90h", false), "weakened", "approval 2 of 3, times out after 24h -> 2 of 3, times out after 90h"},
		{"exclusion removed", gp("", true), gp("", false), "weakened", "approval 2 of 3 excluding the requester -> 2 of 3"},
		{"exclusion removed, timeout kept", gp("24h", true), gp("24h", false), "weakened", "approval 2 of 3 excluding the requester, times out after 24h -> 2 of 3, times out after 24h"},
		{"exclusion added, timeout removed", gp("24h", false), gp("", true), "weakened", "approval 2 of 3, times out after 24h -> 2 of 3 excluding the requester, no timeout"},
		// A stricter need with a removed timeout is a change in two directions: not strictly stronger, so weakened.
		{"need raised, timeout removed", func() map[string]any { g := gp("24h", false); g["need"] = 1; return g }(), gp("", false), "weakened", ""},
		{"same", gp("24h", false), gp("24h", false), "", ""},
	} {
		v := diffOf(t, gateCfg(c.from), gateCfg(c.to), nil, nil)
		switch {
		case c.verdict == "" && len(v.Changes) == 0:
			continue
		case len(v.Changes) != 1:
			t.Errorf("%s: changes %q", c.name, lines(v.Changes))
			continue
		}
		ch := v.Changes[0]
		if ch.Gate != c.verdict || (c.say != "" && ch.Details[0] != c.say+" ("+c.verdict+")") {
			t.Errorf("%s: gate %q, detail %q; want %q (%s)", c.name, ch.Gate, ch.Details, c.say, c.verdict)
		}
		if weak := len(v.Weakened) > 0; weak != (c.verdict == "weakened") {
			t.Errorf("%s: weakened list %q", c.name, v.Weakened)
		}
	}
	// A single-decision gate's deadline counts too.
	single := func(ms int64) *plan.TopologyGate { return &plan.TopologyGate{Single: true, TimeoutMS: ms} }
	if gateChange(single(0), single(1000)) != "strengthened" || gateChange(single(1000), single(0)) != "weakened" || gateChange(single(1000), single(1000)) != "" {
		t.Error("single-decision deadlines misclassified")
	}
	if got := gateShort(single(3_600_000)); got != "one decision, times out after 1h" {
		t.Errorf("gateShort = %q", got)
	}
}
