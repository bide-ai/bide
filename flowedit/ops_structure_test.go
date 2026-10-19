package flowedit

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const structFlow = `# the refund flow
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
  - {name: receipt, block: receipt}
  - {name: manual review, block: review}
wiring:
  - edge: [validate, charge]
  # routes by outcome
  - switch: charge
    when:
      - {pred: ok, to: receipt}
      - {pred: odd, to: manual review}
    else: receipt
  - edge: [receipt, manual review]
assert:
  - every path to charge passes approval >= 2 of 3
  - receipt reachable
  - no side effect after "manual review"
`

func refusalOf(t *testing.T, name, src string, op Operation) string {
	t.Helper()
	_, err := ApplyOperation(name, []byte(src), op)
	var rf *Refusal
	if !errors.As(err, &rf) {
		t.Fatalf("%+v: want a refusal, got %v", op, err)
	}
	return rf.Reason
}

// Each structure operation changes the lines it is about and no others.
func TestStructureOperationsAreSplices(t *testing.T) {
	for _, c := range []struct {
		name     string
		op       Operation
		from, to string
	}{
		{"add a node in the style of the last one", Operation{Op: "add_node", Node: "audit", Value: "audit"},
			"  - {name: manual review, block: review}\n", "  - {name: manual review, block: review}\n  - {name: audit, block: audit}\n"},
		{"rename a node everywhere it is named",
			Operation{Op: "rename_node", Node: "receipt", Value: "email"}, "", ""},
		{"make the entry explicit", Operation{Op: "set_entry", Node: "", Value: "validate"}, "", ""},
	} {
		got := mustApply(t, "f.yaml", structFlow, c.op)
		if c.from == "" {
			continue
		}
		if want := strings.Replace(structFlow, c.from, c.to, 1); got != want {
			t.Errorf("%s:\n--- got\n%s\n--- want\n%s", c.name, got, want)
		}
	}
	got := mustApply(t, "f.yaml", structFlow, Operation{Op: "rename_node", Node: "receipt", Value: "email"})
	want := strings.NewReplacer(
		"  - {name: receipt, block: receipt}\n", "  - {name: email, block: receipt}\n",
		"{pred: ok, to: receipt}", "{pred: ok, to: email}",
		"    else: receipt\n", "    else: email\n",
		"  - edge: [receipt, manual review]\n", "  - edge: [email, manual review]\n",
		"  - receipt reachable\n", "  - email reachable\n",
	).Replace(structFlow)
	if got != want {
		t.Errorf("rename:\n--- got\n%s\n--- want\n%s", got, want)
	}
	// A name with a space is quoted where the assertion grammar needs it, and the entry follows.
	got = mustApply(t, "f.yaml", structFlow, Operation{Op: "rename_node", Node: "validate", Value: "check input"})
	for _, s := range []string{"entry: check input\n", "name: check input, block: validate", "edge: [check input, charge]"} {
		if !strings.Contains(got, s) {
			t.Errorf("rename to a name with a space: missing %q in\n%s", s, got)
		}
	}
	got = mustApply(t, "f.yaml", structFlow, Operation{Op: "rename_node", Node: "manual review", Value: "triage"})
	if !strings.Contains(got, "  - no side effect after triage\n") || strings.Contains(got, "manual review") {
		t.Errorf("rename out of a quoted assertion name:\n%s", got)
	}
	got = mustApply(t, "f.yaml", structFlow, Operation{Op: "rename_node", Node: "receipt", Value: "a b"})
	if !strings.Contains(got, `  - '"a b" reachable'`+"\n") {
		t.Errorf("rename into a name that needs quotes in an assertion:\n%s", got)
	}
}

// Names YAML would read as another type, or that need quoting, are written as the strings they are.
func TestStructureNamesThatAreNotPlainStrings(t *testing.T) {
	for _, name := range []string{"123", "true", "null", "yes", "a: b", "#x", "- y", "é", "a'b", "[x]", "{y}", "1e3", "~"} {
		for _, op := range []Operation{
			{Op: "add_node", Node: name, Value: "blk"},
			{Op: "rename_node", Node: "receipt", Value: name},
		} {
			r, err := ApplyOperation("f.yaml", []byte(structFlow), op)
			if err != nil {
				var rf *Refusal
				if errors.As(err, &rf) {
					t.Logf("%+v refused: %s", op, rf.Reason)
					continue
				}
				t.Fatal(err)
			}
			data, err := ToJSON(r.Text)
			if err != nil {
				t.Fatalf("%+v: %v", op, err)
			}
			var doc struct {
				Nodes []struct{ Name string } `json:"nodes"`
			}
			_ = json.Unmarshal(data, &doc)
			found := false
			for _, n := range doc.Nodes {
				found = found || n.Name == name
			}
			if !found {
				t.Errorf("%+v: the node %q is not in\n%s", op, name, r.Text)
			}
		}
	}
}

func TestRemoveNodeListsWhatGoes(t *testing.T) {
	op := Operation{Op: "remove_node", Node: "receipt"}
	dry := op
	dry.DryRun = true
	r, err := ApplyOperation("f.yaml", []byte(structFlow), dry)
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Text) != structFlow || r.Changed {
		t.Fatalf("a dry run changed the text")
	}
	want := []string{
		"wiring 2: arm ok -> receipt of the switch on charge",
		"wiring 2: else -> receipt of the switch on charge",
		"wiring 3: edge receipt -> manual review",
		`assert 2: "receipt reachable" stays and is reported as naming a missing node`,
	}
	for _, w := range want {
		if !slices.Contains(r.Cascade, w) {
			t.Errorf("the list lacks %q: %q", w, r.Cascade)
		}
	}
	// Comments the removal takes are in the list, so they are not lost in a long diff.
	// Without the list the remove is refused, with a stale one too, with the list it applies.
	if why := refusalOf(t, "f.yaml", structFlow, op); !strings.Contains(why, "confirm") {
		t.Errorf("an unconfirmed remove says %q", why)
	}
	stale := op
	stale.Confirm = r.Cascade[:len(r.Cascade)-1]
	refusalOf(t, "f.yaml", structFlow, stale)
	op.Confirm = r.Cascade
	got := mustApply(t, "f.yaml", structFlow, op)
	for _, gone := range []string{"{name: receipt, block: receipt}", "to: receipt}", "else: receipt", "edge: [receipt,"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q is still in\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "  - receipt reachable\n") || !strings.Contains(got, "{pred: odd, to: manual review}") {
		t.Errorf("the remove took more or less than it listed:\n%s", got)
	}
}

// A node with nothing wired to it and no comment goes alone, with no list to confirm.
func TestRemoveNodeNeedsNoConfirmationWhenNothingElseGoes(t *testing.T) {
	src := strings.Replace(structFlow, "  - {name: manual review, block: review}\n", "  - {name: manual review, block: review}\n  - {name: lone, block: lone}\n", 1)
	got := mustApply(t, "f.yaml", src, Operation{Op: "remove_node", Node: "lone"})
	if got != structFlow {
		t.Errorf("add then remove:\n%s", got)
	}
}

// A switch left with no arm goes whole, and is listed.
func TestRemoveNodeTakesASwitchLeftWithNoArm(t *testing.T) {
	src := `version: 1
flow: f
nodes:
  - {name: a, block: a}
  - {name: b, block: b}
  - {name: c, block: c}
wiring:
  - edge: [a, b]
  - switch: b
    when:
      - {pred: p, to: c}
    else: a
`
	r, err := ApplyOperation("f.yaml", []byte(src), Operation{Op: "remove_node", Node: "c", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Cascade) != 1 || !strings.Contains(r.Cascade[0], "left with no arm") {
		t.Fatalf("%q", r.Cascade)
	}
	got := mustApply(t, "f.yaml", src, Operation{Op: "remove_node", Node: "c", Confirm: r.Cascade})
	if strings.Contains(got, "switch") || strings.Contains(got, "name: c") || !strings.Contains(got, "edge: [a, b]") {
		t.Errorf("\n%s", got)
	}
}

// Removing the last wiring element or the last node leaves a config that still reads.
func TestRemoveLastElements(t *testing.T) {
	src := "version: 1\nflow: f\nnodes:\n  - {name: a, block: a}\nwiring:\n  - edge: [a, a]\n"
	r, err := ApplyOperation("f.yaml", []byte(src), Operation{Op: "remove_node", Node: "a", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	got := mustApply(t, "f.yaml", src, Operation{Op: "remove_node", Node: "a", Confirm: r.Cascade})
	if _, err := ToJSON([]byte(got)); err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	t.Logf("\n%s", got)
}

func TestStructureRefusals(t *testing.T) {
	noEntry := strings.Replace(structFlow, "entry: validate\n", "", 1)
	withJoin := structFlow + "  - join: merge\n    inputs: [receipt, charge]\n    merge: m\n"
	// the join goes under wiring, not after assert
	withJoin = strings.Replace(structFlow, "assert:\n", "  - join: merge\n    inputs: [receipt, charge]\n    merge: m\nassert:\n", 1)
	for name, c := range map[string]struct {
		src string
		op  Operation
		why string
	}{
		"add: empty name":              {structFlow, Operation{Op: "add_node", Value: "b"}, "cannot be empty"},
		"add: padded name":             {structFlow, Operation{Op: "add_node", Node: " x", Value: "b"}, "starts or ends"},
		"add: name with a line break":  {structFlow, Operation{Op: "add_node", Node: "a\nb", Value: "b"}, "control, invisible"},
		"add: long name":               {structFlow, Operation{Op: "add_node", Node: strings.Repeat("x", 201), Value: "b"}, "more than"},
		"add: empty block":             {structFlow, Operation{Op: "add_node", Node: "x"}, "block cannot be empty"},
		"add: duplicate":               {structFlow, Operation{Op: "add_node", Node: "charge", Value: "b"}, "already there"},
		"add: a join's name":           {withJoin, Operation{Op: "add_node", Node: "merge", Value: "b"}, "name of a join"},
		"rename: unknown":              {structFlow, Operation{Op: "rename_node", Node: "nope", Value: "x"}, "no node is named"},
		"rename: onto another node":    {structFlow, Operation{Op: "rename_node", Node: "charge", Value: "receipt"}, "already there"},
		"rename: empty":                {structFlow, Operation{Op: "rename_node", Node: "charge"}, "cannot be empty"},
		"rename: a join's name":        {withJoin, Operation{Op: "rename_node", Node: "charge", Value: "merge"}, "name of a join"},
		"rename: quote in a named one": {structFlow, Operation{Op: "rename_node", Node: "receipt", Value: `a"b`}, "double quote"},
		"rename: unparsable assertion": {strings.Replace(structFlow, "  - receipt reachable\n", "  - something about receipt\n", 1), Operation{Op: "rename_node", Node: "receipt", Value: "x"}, "not one of the six forms"},
		"remove: unknown":              {structFlow, Operation{Op: "remove_node", Node: "nope"}, "no node is named"},
		"remove: the entry":            {structFlow, Operation{Op: "remove_node", Node: "validate"}, "is the entry node"},
		"remove: the implicit entry":   {noEntry, Operation{Op: "remove_node", Node: "validate"}, "set_entry"},
		"remove: a join input":         {withJoin, Operation{Op: "remove_node", Node: "receipt", Confirm: nil}, "input of the join"},
		"entry: unknown":               {structFlow, Operation{Op: "set_entry", Value: "nope"}, "no node is named"},
		"entry: clear to another node": {structFlow, Operation{Op: "set_entry", Value: ""}, ""},
		"duplicate node names":         {strings.Replace(structFlow, "name: receipt", "name: charge", 1), Operation{Op: "rename_node", Node: "charge", Value: "x"}, "more than one node"},
	} {
		if name == "entry: clear to another node" {
			// the entry is "validate", the first node: clearing is allowed and changes nothing in meaning
			if _, err := ApplyOperation("f.yaml", []byte(c.src), c.op); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			src := strings.Replace(structFlow, "entry: validate", "entry: charge", 1)
			if !strings.Contains(refusalOf(t, "f.yaml", src, c.op), "would make") {
				t.Errorf("%s: clearing an entry that is not the first node was not refused", name)
			}
			continue
		}
		why := refusalOf(t, "f.yaml", c.src, c.op)
		if c.why != "" && !strings.Contains(why, c.why) {
			t.Errorf("%s: the refusal says %q, want %q", name, why, c.why)
		}
	}
}

// The implicit entry is never changed in silence: adding after it is fine, and set_entry makes it
// explicit so a later remove or move is the author's.
func TestImplicitEntry(t *testing.T) {
	noEntry := strings.Replace(structFlow, "entry: validate\n", "", 1)
	mustApply(t, "f.yaml", noEntry, Operation{Op: "add_node", Node: "x", Value: "x"})
	mustApply(t, "f.yaml", noEntry, Operation{Op: "remove_node", Node: "receipt", Confirm: removeList(t, noEntry, "receipt")})
	explicit := mustApply(t, "f.yaml", noEntry, Operation{Op: "set_entry", Value: "validate"})
	if !strings.Contains(explicit, "entry: validate") {
		t.Fatalf("\n%s", explicit)
	}
	if again := mustApply(t, "f.yaml", explicit, Operation{Op: "set_entry", Value: "validate"}); again != explicit {
		t.Error("setting the entry to itself changed the text")
	}
	if cleared := mustApply(t, "f.yaml", explicit, Operation{Op: "set_entry"}); strings.Contains(cleared, "entry:") {
		t.Errorf("\n%s", cleared)
	}
}

func removeList(t *testing.T, src, node string) []string {
	t.Helper()
	r, err := ApplyOperation("f.yaml", []byte(src), Operation{Op: "remove_node", Node: node, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	return r.Cascade
}

func TestStructureOnJSON(t *testing.T) {
	src := "{\n  \"version\": 1,\n  \"flow\": \"f\",\n  \"nodes\": [\n    {\"name\": \"a\", \"block\": \"a\"},\n    {\"name\": \"b\", \"block\": \"b\"}\n  ],\n  \"wiring\": [\n    {\"edge\": [\"a\", \"b\"]}\n  ],\n  \"assert\": [\"b reachable\"]\n}\n"
	got := mustApply(t, "f.json", src, Operation{Op: "add_node", Node: "c", Value: "c"})
	if !json.Valid([]byte(got)) || !strings.Contains(got, `"name": "c"`) {
		t.Errorf("\n%s", got)
	}
	got = mustApply(t, "f.json", src, Operation{Op: "rename_node", Node: "b", Value: "bb"})
	if !json.Valid([]byte(got)) || strings.Contains(got, `"b"`) && strings.Contains(got, `"block": "b"`) == false || !strings.Contains(got, `"bb reachable"`) {
		t.Errorf("\n%s", got)
	}
	got = mustApply(t, "f.json", src, Operation{Op: "remove_node", Node: "b", Confirm: func() []string {
		r, _ := ApplyOperation("f.json", []byte(src), Operation{Op: "remove_node", Node: "b", DryRun: true})
		return r.Cascade
	}()})
	if !json.Valid([]byte(got)) || strings.Contains(got, `"name": "b"`) {
		t.Errorf("\n%s", got)
	}
}

// Over every config in the corpus: an add, a rename and a remove either refuse or give text that
// reads back, and the inverse operation restores the exact bytes where it can (a file with
// assertions is compared as data, because the rename writes them through the grammar).
func TestStructureOperationsOverCorpus(t *testing.T) {
	files := append(collect(t), generated(150)...)
	var applied, refused, restored int
	reasons := map[string]int{}
	why := func(err error) {
		var rf *Refusal
		if errors.As(err, &rf) {
			r := rf.Reason
			if i := strings.IndexAny(r, ":\""); i > 0 {
				r = r[:i]
			}
			reasons[r]++
		}
	}
	for _, f := range files {
		name := "f.yaml"
		if json.Valid([]byte(f.text)) {
			name = "f.json"
		}
		data := []byte(f.text)
		if name == "f.yaml" {
			var err error
			if data, err = ToJSON([]byte(f.text)); err != nil {
				continue
			}
		}
		var cfg structDoc
		if json.Unmarshal(data, &cfg) != nil {
			continue
		}
		// add then remove restores the exact bytes
		add := Operation{Op: "add_node", Node: "zz_new_node", Value: "zz_block"}
		r, err := ApplyOperation(name, []byte(f.text), add)
		if err != nil {
			refused++
			why(err)
			continue
		}
		applied++
		if _, err := ApplyOperation(name, r.Text, add); err == nil {
			t.Errorf("%s: adding the same node twice was not refused", f.name)
		}
		dry, err := ApplyOperation(name, r.Text, Operation{Op: "remove_node", Node: "zz_new_node", DryRun: true})
		if err != nil {
			t.Errorf("%s: the node just added cannot be dry-run removed: %v", f.name, err)
			continue
		}
		back, err := ApplyOperation(name, r.Text, Operation{Op: "remove_node", Node: "zz_new_node", Confirm: dry.Cascade})
		switch {
		case err != nil:
			t.Errorf("%s: the node just added cannot be removed: %v", f.name, err)
		case string(back.Text) != f.text && strings.HasSuffix(f.text, "\n"): // the engine ends a file with a newline
			t.Errorf("%s: add then remove did not restore the bytes\n--- before\n%s\n--- after\n%s", f.name, f.text, back.Text)
		default:
			restored++
		}
		// rename each node and back
		for _, n := range cfg.Nodes {
			if n.Name == "" {
				continue
			}
			ren := Operation{Op: "rename_node", Node: n.Name, Value: "zz_renamed"}
			r1, err := ApplyOperation(name, []byte(f.text), ren)
			if err != nil {
				refused++
				why(err)
				continue
			}
			applied++
			r2, err := ApplyOperation(name, r1.Text, Operation{Op: "rename_node", Node: "zz_renamed", Value: n.Name})
			if err != nil {
				t.Errorf("%s: rename %q back: %v", f.name, n.Name, err)
				continue
			}
			// A name YAML would read as another type ("y") comes back quoted: the engine's choice.
			quoted := strings.Contains(string(r2.Text), `"`+n.Name+`"`) && !strings.Contains(f.text, `"`+n.Name+`"`)
			ascii := !strings.ContainsFunc(f.text, func(r rune) bool { return r > 127 })
			if len(cfg.Assert) == 0 && !quoted && ascii && string(r2.Text) != f.text {
				t.Errorf("%s: rename %q and back did not restore the bytes\n--- before\n%s\n--- after\n%s", f.name, n.Name, f.text, r2.Text)
			}
			d1, _ := ToJSONIf(name, []byte(f.text))
			d2, _ := ToJSONIf(name, r2.Text)
			if !sameJSON(d1, d2) {
				t.Errorf("%s: rename %q and back changed the data", f.name, n.Name)
			}
			// remove (with its list) reads back and drops the node
			dry, err := ApplyOperation(name, []byte(f.text), Operation{Op: "remove_node", Node: n.Name, DryRun: true})
			if err != nil {
				refused++
				continue
			}
			rm, err := ApplyOperation(name, []byte(f.text), Operation{Op: "remove_node", Node: n.Name, Confirm: dry.Cascade})
			if err != nil {
				t.Errorf("%s: remove %q with its own list: %v", f.name, n.Name, err)
				continue
			}
			applied++
			var after structDoc
			d, _ := ToJSONIf(name, rm.Text)
			if json.Unmarshal(d, &after) != nil || len(after.Nodes) != len(cfg.Nodes)-1 {
				t.Errorf("%s: remove %q left %d nodes of %d", f.name, n.Name, len(after.Nodes), len(cfg.Nodes))
			}
		}
	}
	t.Logf("refusals: %v", reasons)
	t.Logf("%d applied, %d refused, %d add/remove restored the bytes", applied, refused, restored)
	if applied < 100 || restored == 0 {
		t.Errorf("the corpus exercised too little: %d applied, %d restored", applied, restored)
	}
}

func ToJSONIf(name string, text []byte) ([]byte, error) {
	if formatOf(name) == "yaml" {
		return ToJSON(text)
	}
	return text, nil
}

func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// A join's inputs are names too: a rename follows them, and the join keeps the rest of its element.
func TestRenameFollowsJoinInputs(t *testing.T) {
	src := `version: 1
flow: f
nodes:
  - {name: a, block: a}
  - {name: b, block: b}
  - {name: c, block: c}
wiring:
  - edge: [a, b]
  - edge: [a, c]
  - join: m
    inputs: [b, c]   # both branches
    merge: mm
`
	got := mustApply(t, "f.yaml", src, Operation{Op: "rename_node", Node: "c", Value: "cc"})
	if want := strings.NewReplacer("name: c,", "name: cc,", "edge: [a, c]", "edge: [a, cc]", "inputs: [b, c]", "inputs: [b, cc]").Replace(src); got != want {
		t.Errorf("\n%s", got)
	}
}

// What a removal takes with it includes the comments attached to the node, so they are not lost in a
// long diff, and the list must be confirmed even when nothing else is wired to the node.
func TestRemoveListsTheCommentsItTakes(t *testing.T) {
	src := "version: 1\nflow: f\nentry: a\nnodes:\n  - {name: a, block: a}\n  # the extra one\n  - {name: lone, block: l}   # trailing\nwiring: []\n"
	r, err := ApplyOperation("f.yaml", []byte(src), Operation{Op: "remove_node", Node: "lone", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Cascade, []string{"comment: # the extra one", "comment: # trailing"}) {
		t.Fatalf("%q", r.Cascade)
	}
	refusalOf(t, "f.yaml", src, Operation{Op: "remove_node", Node: "lone"})
	got := mustApply(t, "f.yaml", src, Operation{Op: "remove_node", Node: "lone", Confirm: r.Cascade})
	if strings.Contains(got, "lone") || strings.Contains(got, "extra") {
		t.Errorf("\n%s", got)
	}
}

// An assertion left naming a removed node is loud: the check reports an error for it, the view's
// verdict counts it, and the weakening classifier sees nothing silently pass.
func TestRemoveLeavesALoudAssertionError(t *testing.T) {
	op := Operation{Op: "remove_node", Node: "receipt", Confirm: removeList(t, structFlow, "receipt")}
	got := mustApply(t, "f.yaml", structFlow, op)
	v := BuildView("f.yaml", []byte(got))
	if v.Errors == 0 {
		t.Fatalf("no error after removing a node an assertion names: %+v", v.Verdict)
	}
	found := false
	for _, p := range v.Problems {
		if strings.Contains(p.Message, "receipt") {
			found = true
		}
	}
	for _, a := range v.Assertions {
		if strings.Contains(a.Text, "receipt") && !a.Holds {
			found = true
		}
	}
	if !found {
		t.Errorf("nothing names the missing node: problems %+v assertions %+v", v.Problems, v.Assertions)
	}
	if v.Verdict.Class == "good" {
		t.Errorf("the verdict is %q", v.Verdict.Word)
	}
	t.Logf("%s | %s", v.Verdict.Word, v.Verdict.Line)
}

// A name cannot be made to read as another: invisible, bidirectional and other non-printing
// characters are refused in a new name, by add and by rename.
func TestNamesCannotSpoof(t *testing.T) {
	for what, name := range map[string]string{
		"right-to-left override": "ch\u202egra",
		"left-to-right isolate":  "a\u2066b",
		"left-to-right mark":     "a\u200eb",
		"zero-width space only":  "\u200b",
		"zero-width joiner":      "a\u200db",
		"word joiner":            "a\u2060b",
		"soft hyphen":            "a\u00adb",
		"no-break space":         "a\u00a0b",
		"line separator":         "a\u2028b",
		"private use":            "a\ue000b",
		"unassigned":             "a\U000e0fffb",
		"tab":                    "a\tb",
		"byte order mark":        "a\ufeffb",
	} {
		for _, op := range []Operation{{Op: "add_node", Node: name, Value: "b"}, {Op: "rename_node", Node: "receipt", Value: name}} {
			if why := refusalOf(t, "f.yaml", structFlow, op); !strings.Contains(why, "invisible") && !strings.Contains(why, "starts or ends") {
				t.Errorf("%s with %s: %q", op.Op, what, why)
			}
		}
	}
	// Names people use stay: other scripts, accents, symbols, a plain inner space.
	for _, name := range []string{"é", "ノード", "🚀 rocket", "a-b_c.d", "manual review"} {
		if name == "manual review" {
			continue // already a node in structFlow
		}
		mustApply(t, "f.yaml", structFlow, Operation{Op: "add_node", Node: name, Value: "b"})
	}
}
