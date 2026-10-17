package plan

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAssertQuotedEscapeMessage pins what the warning for a quoted assert name with a backslash
// sequence says: the line, the node it refers to now (the name as written), and the fix (the
// character itself).
func TestAssertQuotedEscapeMessage(t *testing.T) {
	cfg := map[string]any{"version": 1, "flow": "f", "entry": "n",
		"nodes":  []any{map[string]any{"name": "n", "block": "b", "safety": "readonly"}},
		"wiring": []any{}, "assert": []string{"\"caf\\u00e9\" reachable"}}
	data, _ := json.Marshal(cfg)
	var msg string
	for _, p := range InspectConfig(data, nil).Problems {
		if p.Severity == "warning" && strings.Contains(p.Message, "backslash sequence") {
			msg = p.Message
		}
	}
	for _, want := range []string{
		`assert[0] "\"caf\\u00e9\" reachable"`,
		"refers to the node named `caf\\u00e9`",
		"if you meant \"caf\u00e9\", write that character itself",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the warning %q does not contain %q", msg, want)
		}
	}
	for _, alarm := range []string{"as a name with", "would have been read before"} {
		if strings.Contains(msg, alarm) {
			t.Errorf("the warning still has the old wording %q: %s", alarm, msg)
		}
	}
}
