package runview

import (
	"strings"
	"testing"
)

func TestRev316(t *testing.T) {
	base := gapCfg("", "")
	cases := []struct {
		name, to string
		wantWeak bool
	}{
		// An ungated read-only node becomes an effect (safety lowered): a new ungated effect.
		{"ungated node becomes a side effect", strings.Replace(base, `"name": "validate", "block": "validate", "safety": "readonly"`, `"name": "validate", "block": "validate", "safety": "side_effect"`, 1), true},
		// An ungated read-only node now runs another block (a block reference edit).
		{"ungated node's block swapped for an effect", strings.Replace(base, `"name": "validate", "block": "validate", "safety": "readonly"`, `"name": "validate", "block": "charge"`, 1), true},
		// receipt (behind the gate) renamed and reached directly.
		{"rename behind the gate plus bypass", strings.NewReplacer(`"name": "receipt", "block": "receipt"`, `"name": "receipt2", "block": "receipt"`, `["charge", "receipt"]`, `["validate", "receipt2"]`).Replace(base), true},
		// The gated node renamed, its gate kept: nothing got weaker.
		{"gated node renamed, gate kept", strings.NewReplacer(`"name": "charge"`, `"name": "pay"`, `["validate", "charge"]`, `["validate", "pay"]`, `["charge", "receipt"]`, `["pay", "receipt"]`).Replace(base), false},
	}
	for _, c := range cases {
		v := diffOf(t, base, c.to, nil, nil)
		if (len(v.Weakened) > 0) != c.wantWeak {
			t.Errorf("%s: weakened=%q; want weakened=%v", c.name, v.Weakened, c.wantWeak)
		}
	}
}

// A Switch whose arm conditions are swapped sends what went to the gated node to an ungated effect.
func TestRev316_ArmSwap(t *testing.T) {
	cfg := func(p1, p2 string) string {
		return `{"version": 1, "flow": "f", "entry": "validate",
	  "nodes": [{"name": "validate", "block": "validate", "safety": "readonly"},
	            {"name": "charge", "block": "charge", "safety": "side_effect", "approval": {"need": 2, "approvers": ["ops", "finance", "risk"]}},
	            {"name": "small", "block": "charge", "safety": "side_effect"}],
	  "wiring": [{"switch": "validate", "when": [{"pred": "` + p1 + `", "to": "charge"}], "else": "small"}]}`
	}
	v := diffOf(t, cfg("large", ""), cfg("tiny", ""), nil, nil)
	t.Logf("changes: %q weakened: %q", lines(v.Changes), v.Weakened)
	if len(v.Weakened) == 0 {
		t.Errorf("the gated arm's condition changed (large -> tiny), so large amounts now reach an ungated effect: not flagged")
	}
}
