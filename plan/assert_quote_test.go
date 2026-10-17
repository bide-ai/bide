package plan

import (
	"encoding/json"
	"testing"
)

// A node name in double quotes in an assert line is exactly what is between the quotes: the grammar
// has no escapes. A name with a backslash and a space (which must be quoted) names the node it says,
// not an undeclared node with the quotes in its name or one with a newline in it, and a person's line
// and the one the flow editor writes are read alike.
func TestAssertQuotedNamesAreRaw(t *testing.T) {
	names := []string{`my\ node`, `x\n y`, `tab\t here`, `ué x`, `a b`, `back\\slash x`, `plain`, `bare\back`}
	for _, name := range names {
		quoted := `"` + name + `"`
		if name == "plain" || name == `bare\back` {
			quoted = name // a bare name: read raw too
		}
		for _, line := range []string{
			quoted + " reachable",
			"every path to " + quoted + " passes approval >= 1",
			"every path to a terminal passes " + quoted,
			"every path to " + quoted + " passes " + quoted,
			"no side effect after " + quoted,
		} {
			cfg := map[string]any{"version": 1, "flow": "f", "entry": name,
				"nodes":  []any{map[string]any{"name": name, "block": "b", "safety": "readonly", "approval": map[string]any{"need": 1, "approvers": []string{"ops"}}}},
				"wiring": []any{}, "assert": []string{line}}
			data, _ := json.Marshal(cfg)
			r := InspectConfig(data, nil)
			if len(r.Assertions) != 1 || r.Assertions[0].Invalid {
				t.Errorf("%q: %+v", line, r.Assertions)
				continue
			}
			if !r.Assertions[0].Holds && r.Assertions[0].Undecided {
				t.Errorf("%q is undecided: %+v", line, r.Assertions[0])
			}
		}
	}
	// The name is the same whichever way the line is read: compared, and renamed.
	if got := RenameAssertion(`"my\ node" reachable`, nil); got != `"my\ node" reachable` {
		t.Errorf("rename keeps the name: %q", got)
	}
	if got := RenameAssertion(`"my\ node" reachable`, func(n string) string { return n + `\ two` }); got != `"my\ node\ two" reachable` {
		t.Errorf("rename: %q", got)
	}
	if got := RenameAssertion(`x\n reachable`, nil); got != `"x\n" reachable` {
		t.Errorf("a bare name with a backslash is raw: %q", got)
	}
	if CompareAssertions(`"x\n y" reachable`, `"x\n y"   reachable`) != "equal" {
		t.Error("the same line with other spaces is not equal")
	}
	if CompareAssertions(`"x\n y" reachable`, `"x<LF> y" reachable`) == "equal" {
		t.Error("a name with an escape is a different name from one with a newline")
	}
}
