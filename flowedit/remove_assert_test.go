package flowedit

import (
	"slices"
	"strings"
	"testing"
)

// removeFlow is renameFlow with an assertion about the middle step, which a removal of that step takes.
const removeFlow = `version: 1
flow: refund
entry: validate
nodes:
  - {name: validate, block: validate, safety: readonly}
  - name: charge
    block: charge
    safety: side_effect
    approval: {need: 2, approvers: [ops, finance, risk], exclude_principal: true, timeout: 24h}
  - {name: audit, block: plain, safety: idempotent}
  - {name: receipt, block: receipt, safety: idempotent}
wiring:
  - edge: [validate, charge]
  - edge: [charge, audit]
  - edge: [audit, receipt]
assert:
  - every path to charge passes approval >= 2 of 3
  - audit reachable
  - receipt reachable
`

func dryRun(t *testing.T, name, src string, op Operation) []string {
	t.Helper()
	op.DryRun = true
	r, err := ApplyOperation(name, []byte(src), op)
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Text) != src || r.Changed {
		t.Fatalf("a dry run changed the text")
	}
	return r.Cascade
}

// The dry run lists the assertions that name the node, a confirmed removal takes them, and what it
// takes is what it listed.
func TestRemoveNodeTakesTheAssertionsThatNameIt(t *testing.T) {
	list := dryRun(t, "f.yaml", removeFlow, Operation{Op: "remove_node", Node: "audit"})
	want := `assert 2: "audit reachable" is removed (removing an assertion weakens the policy)`
	if !slices.Contains(list, want) {
		t.Fatalf("the list lacks %q: %q", want, list)
	}
	for _, x := range list {
		if strings.Contains(x, "receipt reachable") || strings.Contains(x, "passes approval") {
			t.Errorf("the list takes an assertion that does not name the node: %q", x)
		}
	}
	// Unconfirmed, and confirmed with a list that leaves the assertion out, it is refused.
	if why := refusalOf(t, "f.yaml", removeFlow, Operation{Op: "remove_node", Node: "audit"}); !strings.Contains(why, "confirm") {
		t.Errorf("an unconfirmed removal says %q", why)
	}
	refusalOf(t, "f.yaml", removeFlow, Operation{Op: "remove_node", Node: "audit", Confirm: slices.DeleteFunc(slices.Clone(list), func(x string) bool { return strings.HasPrefix(x, "assert") })})
	got := mustApply(t, "f.yaml", removeFlow, Operation{Op: "remove_node", Node: "audit", Confirm: list})
	if strings.Contains(got, "audit") {
		t.Errorf("something still names audit:\n%s", got)
	}
	for _, kept := range []string{"  - every path to charge passes approval >= 2 of 3\n", "  - receipt reachable\n"} {
		if !strings.Contains(got, kept) {
			t.Errorf("the removal took %q too:\n%s", kept, got)
		}
	}
}

// shapesYAML and shapesJSON hold every shape of an assertion that names "manual review": the six forms
// by either node, a quoted name with a space, and a line the grammar cannot read; two lines name nothing
// removed.
const shapesHead = `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a"},{"name":"manual review","block":"m"},{"name":"c","block":"c"}],"wiring":[{"edge":["a","manual review"]},{"edge":["manual review","c"]}],`

var shapes = map[string][2]string{
	"f.json": {
		shapesHead + `"assert":["c reachable","every path to c passes \"manual review\"","no side effect after \"manual review\"","\"manual review\" reachable","something odd about \"manual review\"","a reachable"]}`,
		shapesHead + `"assert":["\"manual review\" reachable","something odd about \"manual review\"","every path to c passes \"manual review\"","no side effect after \"manual review\""]}`,
	},
	"f.yaml": {
		`version: 1
flow: f
nodes:
  - {name: a, block: a}
  - {name: manual review, block: m}
  - {name: c, block: c}
wiring:
  - edge: [a, manual review]
  - edge: [manual review, c]
assert:
  - c reachable
  - every path to c passes "manual review"
  - no side effect after "manual review"
  - '"manual review" reachable'
  - something odd about "manual review"
  - a reachable
`, `version: 1
flow: f
nodes:
  - {name: a, block: a}
  - {name: manual review, block: m}
  - {name: c, block: c}
wiring:
  - edge: [a, manual review]
  - edge: [manual review, c]
assert:
  - '"manual review" reachable'
  - something odd about "manual review"
  - every path to c passes "manual review"
  - no side effect after "manual review"
`,
	},
}

func TestRemoveNodeTakesEveryAssertionShape(t *testing.T) {
	for file, srcs := range shapes {
		list := dryRun(t, file, srcs[0], Operation{Op: "remove_node", Node: "manual review"})
		n := 0
		for _, x := range list {
			if strings.HasPrefix(x, "assert ") {
				n++
			}
		}
		if n != 4 {
			t.Errorf("%s: %d assertions listed, want 4: %q", file, n, list)
		}
		got := mustApply(t, file, srcs[0], Operation{Op: "remove_node", Node: "manual review", Confirm: list})
		if strings.Contains(got, "manual review") || !strings.Contains(got, "c reachable") || !strings.Contains(got, "a reachable") {
			t.Errorf("%s:\n%s", file, got)
		}
		// All of them: no empty list is left behind.
		list = dryRun(t, file, srcs[1], Operation{Op: "remove_node", Node: "manual review"})
		got = mustApply(t, file, srcs[1], Operation{Op: "remove_node", Node: "manual review", Confirm: list})
		if strings.Contains(got, "assert") || strings.Contains(got, "reachable") {
			t.Errorf("%s: an empty or leftover assert list:\n%s", file, got)
		}
		if v := BuildView(file, []byte(got)); v.ParseError != "" {
			t.Errorf("%s: the result does not parse: %s", file, v.ParseError)
		}
	}
}

// Removing the node takes its assertions, so the text is weaker than the baseline by the assertion, and the
// weakened verdict and the save confirmation both say which one; undo brings back the node and the assertion.
func TestRemoveNodeWeakensByTheRemovedAssertion(t *testing.T) {
	eachMode(t, removeFlow, func(t *testing.T, kind string, s *Server) {
		e := newSession(t, s, removeFlow)
		var dry struct {
			Edited
			Refused string `json:"refused"`
		}
		e.post("/api/apply", map[string]any{"text": e.text, "operation": Operation{Op: "remove_node", Node: "audit", DryRun: true}}, &dry)
		if !slices.ContainsFunc(dry.Cascade, func(x string) bool { return strings.Contains(x, `"audit reachable" is removed`) }) {
			t.Fatalf("the dry run lists %q", dry.Cascade)
		}
		e.do(Operation{Op: "remove_node", Node: "audit", Confirm: dry.Cascade})
		// Rewire so the rest of the flow is as it was.
		w := e.do(Operation{Op: "add_edge", Node: "charge", To: "receipt"})
		weakenedWith(t, "the removed assertion", w, "audit reachable")
		for _, line := range strings.Split(e.text, "\n") {
			if strings.Contains(line, "audit") {
				t.Errorf("a line still names audit: %q", line)
			}
		}
		// The save asks for the confirmation, and names the assertion in it.
		var pv Preview
		e.post("/api/preview", map[string]any{"text": e.text, "hash": hashOf([]byte(removeFlow))}, &pv)
		if len(pv.NeedsConfirmation) == 0 || !strings.Contains(strings.Join(pv.NeedsConfirmation, "\n"), "audit reachable") || !slices.Contains(pv.NeedsKinds, ConfirmWeakens) {
			t.Errorf("the save asks for %q (%q)", pv.NeedsConfirmation, pv.NeedsKinds)
		}
		// Undo walks back to the exact text, with the node and the assertion.
		e.back()
		v := e.back()
		if e.text != removeFlow || !strings.Contains(e.text, "audit reachable") {
			t.Errorf("undo did not restore the node and its assertion:\n%s", e.text)
		}
		notWeakened(t, "after undo", v)
	})
}
