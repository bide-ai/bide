package plan

import (
	"encoding/json"
	"strings"
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

// A quoted name whose old reading (strconv.Unquote) differs from the new (as written) is warned about,
// so no existing assertion silently starts naming a different node; a name the old reading left alone
// (no escape it could read) is not.
func TestAssertQuotedEscapeWarning(t *testing.T) {
	for line, warn := range map[string]bool{
		`"a\tb" reachable`:                           true,
		`"caf\u00e9" reachable`:                      true,
		`every path to "x\x41" passes approval >= 1`: true,
		`"my\ node" reachable`:                       false, // the old reading failed: it already meant the raw name
		`"a b" reachable`:                            false,
		`plain reachable`:                            false,
		`"back\\slash" reachable`:                    true, // the old reading was one backslash
	} {
		cfg := map[string]any{"version": 1, "flow": "f", "entry": "n",
			"nodes":  []any{map[string]any{"name": "n", "block": "b", "safety": "readonly"}},
			"wiring": []any{}, "assert": []string{line}}
		data, _ := json.Marshal(cfg)
		var warned bool
		for _, p := range InspectConfig(data, nil).Problems {
			if p.Severity == "warning" && strings.Contains(p.Message, "backslash sequence") {
				warned = true
			}
		}
		if warned != warn {
			t.Errorf("%q: warned %v, want %v", line, warned, warn)
		}
	}
	// A warning does not fail the check (without -strict); the assertion is still decided as written.
	cfg := map[string]any{"version": 1, "flow": "f", "entry": `a\tb`,
		"nodes":  []any{map[string]any{"name": `a\tb`, "block": "b", "safety": "readonly"}},
		"wiring": []any{}, "assert": []string{`"a\tb" reachable`}}
	data, _ := json.Marshal(cfg)
	r := InspectConfig(data, nil)
	if r.Errors() {
		t.Errorf("a warning is an error: %+v", r.Problems)
	}
}
