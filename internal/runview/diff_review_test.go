package runview

import (
	"strings"
	"testing"
)

func TestDiffAnyNowUngatedEffect(t *testing.T) {
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
func TestDiffArmSwapIsFlagged(t *testing.T) {
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

// Rename pairing must not hide a bypass: rename receipt AND add an ungated route to it; rename a
// gated node AND loosen its gate; rename two nodes and wire a bypass between them.
func TestDiffRenameCannotHideAChange(t *testing.T) {
	base := gapCfg("", "")
	cases := map[string]string{
		"rename + bypass edge":   strings.NewReplacer(`"name": "receipt", "block": "receipt"`, `"name": "receipt2", "block": "receipt"`, `["charge", "receipt"]`, `["charge", "receipt2"]}, {"edge": ["validate", "receipt2"]`).Replace(base),
		"rename + gate loosened": strings.NewReplacer(`"name": "charge"`, `"name": "pay"`, `["validate", "charge"]`, `["validate", "pay"]`, `["charge", "receipt"]`, `["pay", "receipt"]`, `"need": 2`, `"need": 1`).Replace(base),
		"rename both + bypass":   strings.NewReplacer(`"name": "charge"`, `"name": "pay"`, `"name": "receipt", "block": "receipt"`, `"name": "rcpt", "block": "receipt"`, `["validate", "charge"]`, `["validate", "pay"]`, `["charge", "receipt"]`, `["pay", "rcpt"]}, {"edge": ["validate", "rcpt"]`).Replace(base),
		"rename + gate removed":  strings.NewReplacer(`"name": "charge"`, `"name": "pay"`, `["validate", "charge"]`, `["validate", "pay"]`, `["charge", "receipt"]`, `["pay", "receipt"]`, `, "approval": {"need": 2, "approvers": ["ops", "finance", "risk"]}`, ``).Replace(base),
	}
	for name, to := range cases {
		v := diffOf(t, base, to, nil, nil)
		t.Logf("%s: %q", name, v.Weakened)
		if len(v.Weakened) == 0 {
			t.Errorf("%s: not flagged", name)
		}
	}
}
