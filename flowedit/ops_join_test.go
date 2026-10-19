package flowedit

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

const joinFlow = `version: 1
flow: f
entry: a
nodes:
  - {name: a, block: a}
  - {name: b, block: b}
  - {name: c, block: c}
  - {name: d, block: d}
  - {name: e, block: e}
wiring:
  - edge: [a, b]
  - edge: [a, c]
  # fan in
  - join: j
    inputs: [b, c]
    merge: mm
    safety: readonly   # no effect
  - edge: [j, d]
  - switch: d
    when:
      - {pred: again, to: b, loop_max: 3}
      - {pred: done, to: e}
assert:
  - e reachable
`

func TestJoinOperationsAreSplices(t *testing.T) {
	for _, c := range []struct {
		name     string
		op       Operation
		from, to string
	}{
		{"add a join", Operation{Op: "add_join", Node: "k", Inputs: []string{"d", "e"}, Value: "m2"}, "      - {pred: done, to: e}\n", "      - {pred: done, to: e}\n  - join: k\n    inputs: [d, e]\n    merge: m2\n"},
		{"add an input at the end", Operation{Op: "set_join_inputs", Node: "j", Inputs: []string{"b", "c", "d"}}, "inputs: [b, c]", "inputs: [b, c, d]"},
		{"remove an input", Operation{Op: "set_join_inputs", Node: "j", Inputs: []string{"c"}}, "inputs: [b, c]", "inputs: [c]"},
		{"reorder the inputs", Operation{Op: "set_join_inputs", Node: "j", Inputs: []string{"c", "b"}}, "inputs: [b, c]", "inputs: [c, b]"},
		{"change the merge", Operation{Op: "set_join_merge", Node: "j", Value: "mm2"}, "    merge: mm\n", "    merge: mm2\n"},
		{"set a loop bound", Operation{Op: "set_loop_max", Node: "d", Index: 1, Max: 5}, "{pred: done, to: e}", "{pred: done, to: e, loop_max: 5}"},
		{"change a loop bound", Operation{Op: "set_loop_max", Node: "d", Index: 0, Max: 7}, "loop_max: 3}", "loop_max: 7}"},
		{"remove a loop bound", Operation{Op: "set_loop_max", Node: "d", Index: 0, Max: 0}, "{pred: again, to: b, loop_max: 3}", "{pred: again, to: b}"},
		{"gate a join", Operation{Op: "set_approval", Node: "j", Approval: &Approval{Need: 1, Approvers: []string{"ops"}}}, "    safety: readonly   # no effect\n", "    safety: readonly   # no effect\n    approval:\n      need: 1\n      approvers:\n        - ops\n"},
		{"change a join's safety", Operation{Op: "set_safety", Node: "j", Value: "idempotent"}, "safety: readonly   # no effect", "safety: idempotent   # no effect"},
		{"remove a join's safety", Operation{Op: "set_safety", Node: "j"}, "    safety: readonly   # no effect\n", ""},
	} {
		got := mustApply(t, "f.yaml", joinFlow, c.op)
		want := strings.Replace(joinFlow, c.from, c.to, 1)
		if want == joinFlow && c.from != c.to {
			t.Fatalf("%s: the expectation does not apply", c.name)
		}
		if got != want {
			t.Errorf("%s:\n--- got\n%s\n--- want\n%s", c.name, got, want)
		}
	}
	for _, same := range []Operation{
		{Op: "set_join_inputs", Node: "j", Inputs: []string{"b", "c"}},
		{Op: "set_join_merge", Node: "j", Value: "mm"},
		{Op: "set_loop_max", Node: "d", Index: 0, Max: 3},
		{Op: "set_loop_max", Node: "d", Index: 1, Max: 0},
	} {
		if got := mustApply(t, "f.yaml", joinFlow, same); got != joinFlow {
			t.Errorf("%+v changed the text", same)
		}
	}
}

func TestRemoveJoinListsWhatGoes(t *testing.T) {
	dry, err := ApplyOperation("f.yaml", []byte(joinFlow), Operation{Op: "remove_join", Node: "j", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(dry.Text) != joinFlow {
		t.Fatal("a dry run changed the text")
	}
	for _, w := range []string{"wiring 4: edge j -> d"} {
		if !slices.Contains(dry.Cascade, w) {
			t.Errorf("the list lacks %q: %q", w, dry.Cascade)
		}
	}
	if !slices.ContainsFunc(dry.Cascade, func(s string) bool { return strings.HasPrefix(s, "comment: # fan in") }) {
		t.Errorf("the join's comment is not listed: %q", dry.Cascade)
	}
	refusalOf(t, "f.yaml", joinFlow, Operation{Op: "remove_join", Node: "j"})
	got := mustApply(t, "f.yaml", joinFlow, Operation{Op: "remove_join", Node: "j", Confirm: dry.Cascade})
	for _, gone := range []string{"join: j", "fan in", "edge: [j, d]"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q is still in\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "edge: [a, b]") || !strings.Contains(got, "switch: d") {
		t.Errorf("the removal took more than it listed:\n%s", got)
	}
}

func TestJoinRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		op  Operation
		why string
	}{
		"add: empty name":               {Operation{Op: "add_join", Inputs: []string{"a"}, Value: "m"}, "cannot be empty"},
		"add: a node's name":            {Operation{Op: "add_join", Node: "a", Inputs: []string{"b"}, Value: "m"}, "already there"},
		"add: a join's name":            {Operation{Op: "add_join", Node: "j", Inputs: []string{"b"}, Value: "m"}, "already there"},
		"add: no merge":                 {Operation{Op: "add_join", Node: "k", Inputs: []string{"a"}}, "needs a merge"},
		"add: no inputs":                {Operation{Op: "add_join", Node: "k", Value: "m"}, "at least one input"},
		"add: unknown input":            {Operation{Op: "add_join", Node: "k", Inputs: []string{"zz"}, Value: "m"}, "no node is named"},
		"add: input twice":              {Operation{Op: "add_join", Node: "k", Inputs: []string{"a", "a"}, Value: "m"}, "named twice"},
		"add: its own output":           {Operation{Op: "add_join", Node: "k", Inputs: []string{"k"}, Value: "m"}, "its own output"},
		"inputs of an unknown join":     {Operation{Op: "set_join_inputs", Node: "zz", Inputs: []string{"a"}}, "no join is named"},
		"no inputs left":                {Operation{Op: "set_join_inputs", Node: "j"}, "at least one input"},
		"merge of an unknown join":      {Operation{Op: "set_join_merge", Node: "zz", Value: "m"}, "no join is named"},
		"an empty merge":                {Operation{Op: "set_join_merge", Node: "j"}, "needs a merge"},
		"a merge with an override":      {Operation{Op: "set_join_merge", Node: "j", Value: "m\u202e"}, "does not print"},
		"remove an unknown join":        {Operation{Op: "remove_join", Node: "zz"}, "no join is named"},
		"remove a node as a join":       {Operation{Op: "remove_join", Node: "a"}, "no join is named"},
		"a loop bound that is negative": {Operation{Op: "set_loop_max", Node: "d", Index: 0, Max: -1}, "not between"},
		"a loop bound that is huge":     {Operation{Op: "set_loop_max", Node: "d", Index: 0, Max: maxLoop + 1}, "not between"},
		"a loop bound on no arm":        {Operation{Op: "set_loop_max", Node: "d", Index: 9, Max: 2}, "no arm at position"},
		"a loop bound with no switch":   {Operation{Op: "set_loop_max", Node: "a", Index: 0, Max: 2}, "no switch is on"},
		"a join's block":                {Operation{Op: "set_block", Node: "j", Value: "x"}, "set_join_merge"},
	} {
		why := refusalOf(t, "f.yaml", joinFlow, c.op)
		if !strings.Contains(why, c.why) {
			t.Errorf("%s: %q, want %q", name, why, c.why)
		}
	}
	// A node that is an input of a join is not removed, and nor is it renamed to a join's name.
	if why := refusalOf(t, "f.yaml", joinFlow, Operation{Op: "remove_node", Node: "b"}); !strings.Contains(why, "input of the join") {
		t.Errorf("%q", why)
	}
}

func TestJoinOperationsOnJSON(t *testing.T) {
	js := "{\n  \"version\": 1,\n  \"flow\": \"f\",\n  \"entry\": \"a\",\n  \"nodes\": [\n    {\"name\": \"a\", \"block\": \"a\"},\n    {\"name\": \"b\", \"block\": \"b\"}\n  ],\n  \"wiring\": []\n}\n"
	got := mustApply(t, "f.json", js, Operation{Op: "add_join", Node: "j", Inputs: []string{"a", "b"}, Value: "m"})
	if !json.Valid([]byte(got)) || !strings.Contains(got, `"inputs": ["a", "b"]`) {
		t.Fatalf("\n%s", got)
	}
	got = mustApply(t, "f.json", got, Operation{Op: "set_safety", Node: "j", Value: "readonly"})
	got = mustApply(t, "f.json", got, Operation{Op: "set_join_inputs", Node: "j", Inputs: []string{"b", "a"}})
	got = mustApply(t, "f.json", got, Operation{Op: "set_join_merge", Node: "j", Value: "m2"})
	if !json.Valid([]byte(got)) || !strings.Contains(got, `"safety": "readonly"`) || !strings.Contains(got, `"inputs": ["b", "a"]`) {
		t.Fatalf("\n%s", got)
	}
	r, _ := ApplyOperation("f.json", []byte(got), Operation{Op: "remove_join", Node: "j", DryRun: true})
	got = mustApply(t, "f.json", got, Operation{Op: "remove_join", Node: "j", Confirm: r.Cascade})
	if !json.Valid([]byte(got)) || strings.Contains(got, `"join"`) {
		t.Fatalf("\n%s", got)
	}
}

// Over the corpus: every join and loop operation refuses or gives text that reads back, and the
// inverse restores the exact bytes where it is one.
func TestJoinOverCorpus(t *testing.T) {
	files := append(collect(t), generated(150)...)
	applied, refused, restored := 0, 0, 0
	for _, f := range files {
		name := "f.yaml"
		if json.Valid([]byte(f.text)) {
			name = "f.json"
		}
		data, err := ToJSONIf(name, []byte(f.text))
		if err != nil {
			continue
		}
		var cfg structDoc
		if json.Unmarshal(data, &cfg) != nil || len(cfg.Nodes) < 2 {
			continue
		}
		a, b := cfg.Nodes[0].Name, cfg.Nodes[len(cfg.Nodes)-1].Name
		try := func(op Operation, undo func(r *Applied) Operation) {
			r, err := ApplyOperation(name, []byte(f.text), op)
			var rf *Refusal
			if errors.As(err, &rf) {
				refused++
				return
			}
			if err != nil {
				t.Errorf("%s %+v: %v", f.name, op, err)
				return
			}
			if !r.Changed {
				return // it set what was already there; its "inverse" would remove it
			}
			applied++
			u := undo(r)
			back, err := ApplyOperation(name, r.Text, u)
			if err != nil {
				t.Errorf("%s %+v: undo %+v: %v", f.name, op, u, err)
				return
			}
			if string(back.Text) != f.text {
				d1, _ := ToJSONIf(name, []byte(f.text))
				d2, _ := ToJSONIf(name, back.Text)
				if !sameJSON(d1, d2) || len(RemovedComments(back.Text, []byte(f.text))) != 0 || len(RemovedComments([]byte(f.text), back.Text)) != 0 {
					t.Errorf("%s %+v: the inverse changed the data or the comments\n--- before\n%s\n--- after\n%s", f.name, op, f.text, back.Text)
				}
				return
			}
			restored++
		}
		remove := func(r *Applied) Operation {
			dry, err := ApplyOperation(name, r.Text, Operation{Op: "remove_join", Node: "zz_join", DryRun: true})
			if err != nil {
				t.Errorf("%s: dry-run remove of the join just added: %v", f.name, err)
				return Operation{Op: "remove_join", Node: "zz_join"}
			}
			return Operation{Op: "remove_join", Node: "zz_join", Confirm: dry.Cascade}
		}
		try(Operation{Op: "add_join", Node: "zz_join", Inputs: []string{a, b}, Value: "zz_merge"}, remove)
		for _, w := range cfg.Wiring {
			if w.Switch != "" && len(w.When) > 0 && !strings.Contains(f.text, "loop_max") {
				w := w
				try(Operation{Op: "set_loop_max", Node: w.Switch, Index: 0, Max: 9}, func(*Applied) Operation {
					return Operation{Op: "set_loop_max", Node: w.Switch, Index: 0, Max: 0}
				})
			}
			if w.Join != "" && w.Inputs != nil && !strings.Contains(f.text, "approval") && !strings.Contains(f.text, "safety") {
				w := w
				try(Operation{Op: "set_safety", Node: w.Join, Value: "readonly"}, func(*Applied) Operation { return Operation{Op: "set_safety", Node: w.Join} })
				try(Operation{Op: "set_approval", Node: w.Join, Approval: &Approval{Need: 1, Approvers: []string{"zz"}}}, func(*Applied) Operation { return Operation{Op: "clear_approval", Node: w.Join} })
			}
		}
	}
	t.Logf("%d applied, %d refused, %d inverses restored the bytes", applied, refused, restored)
	if applied < 100 {
		t.Errorf("the corpus exercised too little: %d applied", applied)
	}
}

// With no join in the file to copy the style from, inputs are written as the examples write them.
func TestNewJoinInputsAreWrittenAsAFlowList(t *testing.T) {
	src := "version: 1\nflow: f\nentry: a\nnodes:\n  - name: a\n    block: a\n  - name: b\n    block: b\nwiring:\n  - edge: [a, b]\n"
	got := mustApply(t, "f.yaml", src, Operation{Op: "add_join", Node: "j", Inputs: []string{"a", "b"}, Value: "m"})
	if !strings.HasSuffix(got, "  - join: j\n    inputs: [a, b]\n    merge: m\n") {
		t.Errorf("\n%s", got)
	}
}

// Items a list edit keeps stay as they are: their comments, and the style of the list.
func TestListEditsKeepTheItemsTheyKeep(t *testing.T) {
	joinSrc := "version: 1\nflow: f\nentry: a\nnodes:\n  - {name: a, block: a}\n  - {name: b, block: b}\n  - {name: c, block: c}\nwiring:\n  - join: j\n    inputs:\n      - a   # first\n      # second\n      - b\n    merge: m\n"
	got := mustApply(t, "f.yaml", joinSrc, Operation{Op: "set_join_inputs", Node: "j", Inputs: []string{"b", "c"}})
	want := "    inputs:\n      # second\n      - b\n      - c\n"
	if !strings.Contains(got, want) || strings.Contains(got, "inputs: [") {
		t.Errorf("join inputs:\n%s", got)
	}
	gateSrc := "version: 1\nflow: f\nentry: a\nnodes:\n  - name: a\n    block: a\n    approval:\n      need: 1\n      approvers:\n        - x   # first\n        # about y\n        - y\n"
	got = mustApply(t, "f.yaml", gateSrc, Operation{Op: "set_approval", Node: "a", Approval: &Approval{Need: 1, Approvers: []string{"y", "z"}}})
	if !strings.Contains(got, "        # about y\n        - y\n        - z\n") || strings.Contains(got, "approvers: [") || strings.Contains(got, "# first") && strings.Contains(got, "- x") {
		t.Errorf("approvers:\n%s", got)
	}
	// Removing one approver is exactly one deleted item; adding one is exactly one inserted item.
	got = mustApply(t, "f.yaml", gateSrc, Operation{Op: "set_approval", Node: "a", Approval: &Approval{Need: 1, Approvers: []string{"y"}}})
	if want := strings.Replace(gateSrc, "        - x   # first\n", "", 1); got != want {
		t.Errorf("removing x:\n%s", got)
	}
	got = mustApply(t, "f.yaml", gateSrc, Operation{Op: "set_approval", Node: "a", Approval: &Approval{Need: 1, Approvers: []string{"x", "y", "z"}}})
	if want := gateSrc + "        - z\n"; got != want {
		t.Errorf("adding z:\n%s", got)
	}
	// A whole new list into a block list never leaves it empty in between.
	got = mustApply(t, "f.yaml", gateSrc, Operation{Op: "set_approval", Node: "a", Approval: &Approval{Need: 1, Approvers: []string{"p", "q"}}})
	if !strings.Contains(got, "- p") || !strings.Contains(got, "- q") || strings.Contains(got, "- x") {
		t.Errorf("replacing all:\n%s", got)
	}
}

// plan.Load registers a join when it reaches its wiring element, so a join that reads a join defined
// after it is refused, and so is a wiring order that would make one; a cycle of joins cannot be made.
func TestJoinInputsMustBeDefinedBefore(t *testing.T) {
	src := "version: 1\nflow: f\nentry: a\nnodes:\n  - {name: a, block: a}\n  - {name: b, block: b}\n  - {name: c, block: c}\nwiring:\n  - join: j1\n    inputs: [a, b]\n    merge: m\n  - join: j2\n    inputs: [j1, c]\n    merge: m\n"
	if why := refusalOf(t, "f.yaml", src, Operation{Op: "set_join_inputs", Node: "j1", Inputs: []string{"a", "j2"}}); !strings.Contains(why, "comes after") {
		t.Errorf("a later join as an input: %q", why)
	}
	if why := refusalOf(t, "f.yaml", src, Operation{Op: "move_wiring", Index: 0, Value: "down"}); !strings.Contains(why, "comes after it") && !strings.Contains(why, "must come first") {
		t.Errorf("a move that puts a join before its input: %q", why)
	}
	// An earlier join is fine, as an input of a later one, and in a new join.
	mustApply(t, "f.yaml", src, Operation{Op: "set_join_inputs", Node: "j2", Inputs: []string{"c", "j1"}})
	mustApply(t, "f.yaml", src, Operation{Op: "add_join", Node: "j3", Inputs: []string{"j1", "j2"}, Value: "m"})
}

func TestRenamingOrMovingAJoinSaysItIsAJoin(t *testing.T) {
	for _, op := range []Operation{
		{Op: "rename_node", Node: "j", Value: "k"},
		{Op: "move_node", Node: "j", Value: "up"},
		{Op: "remove_node", Node: "j"},
	} {
		if why := refusalOf(t, "f.yaml", joinFlow, op); !strings.Contains(why, "is a join, not a node") {
			t.Errorf("%+v: %q", op, why)
		}
	}
}
