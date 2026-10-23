package flowedit

import (
	"errors"
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
// by either node, a quoted name with a space; two lines name nothing removed.
const shapesHead = `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a"},{"name":"manual review","block":"m"},{"name":"c","block":"c"}],"wiring":[{"edge":["a","manual review"]},{"edge":["manual review","c"]}],`

var shapes = map[string][2]string{
	"f.json": {
		shapesHead + `"assert":["c reachable","every path to c passes \"manual review\"","no side effect after \"manual review\"","\"manual review\" reachable","a reachable"]}`,
		shapesHead + `"assert":["\"manual review\" reachable","every path to c passes \"manual review\"","no side effect after \"manual review\""]}`,
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
		if n != 3 {
			t.Errorf("%s: %d assertions listed, want 3: %q", file, n, list)
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

// An assert line the grammar cannot read is neither kept nor removed on a guess: if a word of it is the node's
// name (punctuation trimmed), the removal is refused and says which line to fix, whatever readable
// assertions are beside it; a line with no such word is left alone.
func TestRemoveNodeRefusesAnUnreadableAssertionThatNamesIt(t *testing.T) {
	for name, line := range map[string]string{
		"trailing comma":    "something odd: audit, really",
		"the word in prose": "this is about the audit trail",
		"parenthesised":     "(audit) must be checked",
		"colon":             "audit: needs a look",
		"square brackets":   "[audit] must hold",
		"no-break space":    "audit\u00a0then",
		"trailing dash":     "audit- and more",
		"slash":             "audit/x",
		"curly apostrophe":  "audit\u2019s limit",
		"asterisks":         "*audit* matters",
		"underscores":       "_audit_",
		"pipe":              "audit|",
		"ellipsis":          "audit\u2026",
		"guillemets":        "\u00abaudit\u00bb",
		"backslash":         "audit\\",
		"quoted":            `see "audit" for the detail`,
	} {
		// Single-quoted, so no line is read as YAML syntax of its own.
		src := strings.Replace(removeFlow, "  - receipt reachable\n", "  - receipt reachable\n  - '"+strings.ReplaceAll(line, "'", "''")+"'\n", 1)
		for _, op := range []Operation{{Op: "remove_node", Node: "audit"}, {Op: "remove_node", Node: "audit", DryRun: true}} {
			_, err := ApplyOperation("f.yaml", []byte(src), op)
			var rf *Refusal
			if !errors.As(err, &rf) || !strings.Contains(rf.Reason, "assert 4 is not one of the six forms") || !strings.Contains(rf.Reason, "fix it or remove that line first") {
				t.Errorf("%s (dry run %v): %v", name, op.DryRun, err)
			}
		}
	}
	// A line with no word that is the name is left alone, and the removal goes on.
	src := strings.Replace(removeFlow, "  - receipt reachable\n", "  - receipt reachable\n  - something odd about the audits\n", 1)
	list := dryRun(t, "f.yaml", src, Operation{Op: "remove_node", Node: "audit"})
	got := mustApply(t, "f.yaml", src, Operation{Op: "remove_node", Node: "audit", Confirm: list})
	if !strings.Contains(got, "something odd about the audits") || strings.Contains(got, "audit reachable") {
		t.Errorf("an unrelated unreadable line, or the readable one, was mishandled:\n%s", got)
	}
	// Once the line is fixed the removal goes through.
	fixed := strings.Replace(removeFlow, "  - receipt reachable\n", "  - receipt reachable\n  - something odd\n", 1)
	list = dryRun(t, "f.yaml", fixed, Operation{Op: "remove_node", Node: "audit"})
	mustApply(t, "f.yaml", fixed, Operation{Op: "remove_node", Node: "audit", Confirm: list})
}

// Removing a node with its assertions, and undoing it, is byte for byte: the text after Undo is the text
// before, and the removal made again (Redo) gives the text the removal gave, over comments, flow-style
// and block-style lists, and the last assertion going with the list.
func TestRemoveNodeUndoAndRedoAreByteExact(t *testing.T) {
	const commented = `# the refund flow
version: 1
flow: refund
entry: validate
nodes:
  # reads the request
  - {name: validate, block: validate}

  # the gated one
  - name: charge       # gated
    block: charge
    approval:
      need: 2
      approvers: [ops, finance, risk]
  # the receipt, last
  - {name: receipt, block: receipt}   # trailing
wiring:
  - edge: [validate, charge]
  # then the receipt
  - edge: [charge, receipt]
# what must hold
assert:
  # about the gate
  - every path to charge passes approval >= 2 of 3
  - receipt reachable    # the receipt is the end
  - "no side effect after receipt"
`
	const onlyOne = `version: 1
flow: f
nodes:
  - {name: a, block: a}
  - {name: b, block: b}
wiring:
  - edge: [a, b]
# the only assertion
assert:
  - b reachable   # about b
`
	for name, flow := range map[string]string{"commented": commented, "flow-style": removeFlow, "only assertion": onlyOne} {
		node := map[string]string{"commented": "receipt", "flow-style": "audit", "only assertion": "b"}[name]
		t.Run(name, func(t *testing.T) {
			s, _ := newTestServer(t, "f.yaml", flow)
			e := newSession(t, s, flow)
			var dry struct {
				Edited
				Refused string `json:"refused"`
			}
			e.post("/api/apply", map[string]any{"text": e.text, "operation": Operation{Op: "remove_node", Node: node, DryRun: true}}, &dry)
			if !slices.ContainsFunc(dry.Cascade, func(x string) bool { return strings.HasPrefix(x, "assert ") }) {
				t.Fatalf("the dry run lists no assertion: %q", dry.Cascade)
			}
			remove := Operation{Op: "remove_node", Node: node, Confirm: dry.Cascade}
			e.do(remove)
			removed := e.text
			if removed == flow || strings.Contains(removed, "name: "+node) {
				t.Fatalf("the removal did nothing:\n%s", removed)
			}
			e.back()
			if e.text != flow {
				t.Errorf("undo is not byte for byte:\n%s\nwant\n%s", e.text, flow)
			}
			e.do(remove)
			if e.text != removed {
				t.Errorf("redo is not the text the removal gave:\n%s\nwant\n%s", e.text, removed)
			}
		})
	}
}
