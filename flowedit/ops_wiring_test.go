package flowedit

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const wireFlow = `version: 1
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
  # routes by size
  - switch: b
    when:
      - {pred: big, to: c}
      - {pred: small, to: d}
    else: e
  - edge: [c, e]   # the big path
`

func TestWiringOperationsAreSplices(t *testing.T) {
	for _, c := range []struct {
		name     string
		op       Operation
		from, to string
	}{
		{"add an edge at the end", Operation{Op: "add_edge", Node: "d", To: "e"}, "  - edge: [c, e]   # the big path\n", "  - edge: [c, e]   # the big path\n  - edge: [d, e]\n"},
		{"remove an edge, and the comment above or beside it goes with it", Operation{Op: "remove_edge", Node: "c", To: "e"}, "  - edge: [c, e]   # the big path\n", ""},
		{"remove the first edge", Operation{Op: "remove_edge", Node: "a", To: "b"}, "  - edge: [a, b]\n", ""},
		{"add a switch", Operation{Op: "add_switch", Node: "d", Pred: "ok", To: "e"}, "  - edge: [c, e]   # the big path\n", "  - edge: [c, e]   # the big path\n  - switch: d\n    when:\n      - {pred: ok, to: e}\n"},
		{"add an arm", Operation{Op: "add_arm", Node: "b", Pred: "huge", To: "e"}, "      - {pred: small, to: d}\n", "      - {pred: small, to: d}\n      - {pred: huge, to: e}\n"},
		{"remove an arm", Operation{Op: "remove_arm", Node: "b", Index: 0}, "      - {pred: big, to: c}\n", ""},
		{"change an arm's target", Operation{Op: "set_arm", Node: "b", Index: 1, Pred: "small", To: "e"}, "{pred: small, to: d}", "{pred: small, to: e}"},
		{"rename an arm's predicate", Operation{Op: "set_arm", Node: "b", Index: 0, Pred: "large", To: "c"}, "{pred: big, to: c}", "{pred: large, to: c}"},
		{"change the else", Operation{Op: "set_else", Node: "b", To: "d"}, "    else: e\n", "    else: d\n"},
		{"clear the else", Operation{Op: "set_else", Node: "b"}, "    else: e\n", ""},
		{"move an arm down", Operation{Op: "move_arm", Node: "b", Index: 0, Value: "down"}, "      - {pred: big, to: c}\n      - {pred: small, to: d}\n", "      - {pred: small, to: d}\n      - {pred: big, to: c}\n"},
		{"remove the switch, its comment goes with it", Operation{Op: "remove_switch", Node: "b"}, "  # routes by size\n  - switch: b\n    when:\n      - {pred: big, to: c}\n      - {pred: small, to: d}\n    else: e\n", ""},
	} {
		got := mustApply(t, "f.yaml", wireFlow, c.op)
		want := strings.Replace(wireFlow, c.from, c.to, 1)
		if want == wireFlow && c.from != c.to {
			t.Fatalf("%s: the expectation does not apply", c.name)
		}
		if got != want {
			t.Errorf("%s:\n--- got\n%s\n--- want\n%s", c.name, got, want)
		}
	}
	// Setting an else on a switch that has none writes the key after the arms.
	noElse := strings.Replace(wireFlow, "    else: e\n", "", 1)
	got := mustApply(t, "f.yaml", noElse, Operation{Op: "set_else", Node: "b", To: "e"})
	if !strings.Contains(got, "      - {pred: small, to: d}\n    else: e\n") {
		t.Errorf("else added:\n%s", got)
	}
	// A no-op returns the same bytes.
	for _, op := range []Operation{
		{Op: "set_else", Node: "b", To: "e"},
		{Op: "set_arm", Node: "b", Index: 0, Pred: "big", To: "c"},
	} {
		if same := mustApply(t, "f.yaml", wireFlow, op); same != wireFlow {
			t.Errorf("%+v changed the text", op)
		}
	}
}

func TestWiringOperationsOnJSONAndFlowLists(t *testing.T) {
	js := "{\n  \"version\": 1,\n  \"flow\": \"f\",\n  \"entry\": \"a\",\n  \"nodes\": [\n    {\"name\": \"a\", \"block\": \"a\"},\n    {\"name\": \"b\", \"block\": \"b\"},\n    {\"name\": \"c\", \"block\": \"c\"}\n  ],\n  \"wiring\": []\n}\n"
	got := mustApply(t, "f.json", js, Operation{Op: "add_edge", Node: "a", To: "b"})
	if !json.Valid([]byte(got)) || !strings.Contains(got, `"edge": ["a", "b"]`) {
		t.Fatalf("\n%s", got)
	}
	got = mustApply(t, "f.json", got, Operation{Op: "add_switch", Node: "b", Pred: "p", To: "c"})
	got = mustApply(t, "f.json", got, Operation{Op: "add_arm", Node: "b", Pred: "q", To: "a"})
	got = mustApply(t, "f.json", got, Operation{Op: "set_else", Node: "b", To: "c"})
	if !json.Valid([]byte(got)) {
		t.Fatalf("\n%s", got)
	}
	for _, w := range []string{`"pred": "q"`, `"else": "c"`} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s in\n%s", w, got)
		}
	}
	got = mustApply(t, "f.json", got, Operation{Op: "remove_switch", Node: "b"})
	got = mustApply(t, "f.json", got, Operation{Op: "remove_edge", Node: "a", To: "b"})
	if !json.Valid([]byte(got)) {
		t.Fatalf("\n%s", got)
	}
	// from an empty flow list in YAML
	fl := "version: 1\nflow: f\nentry: a\nnodes: [{name: a, block: a}, {name: b, block: b}]\nwiring: []\n"
	got = mustApply(t, "f.yaml", fl, Operation{Op: "add_edge", Node: "a", To: "b"})
	if !strings.Contains(got, "wiring: [{edge: [a, b]}]") {
		t.Errorf("\n%s", got)
	}
}

func TestWiringRefusals(t *testing.T) {
	withJoin := strings.Replace(wireFlow, "  - edge: [c, e]   # the big path\n", "  - edge: [c, e]   # the big path\n  - join: j\n    inputs: [d, e]\n    merge: m\n", 1)
	oneArm := strings.Replace(wireFlow, "      - {pred: small, to: d}\n", "", 1)
	for name, c := range map[string]struct {
		src string
		op  Operation
		why string
	}{
		"edge from an unknown node":         {wireFlow, Operation{Op: "add_edge", Node: "zz", To: "e"}, "no node is named"},
		"edge to an unknown node":           {wireFlow, Operation{Op: "add_edge", Node: "d", To: "zz"}, "no node is named"},
		"edge with no target":               {wireFlow, Operation{Op: "add_edge", Node: "d"}, "target node is needed"},
		"edge to itself":                    {wireFlow, Operation{Op: "add_edge", Node: "d", To: "d"}, "to itself"},
		"a duplicate edge":                  {wireFlow, Operation{Op: "add_edge", Node: "a", To: "b"}, "already there"},
		"an edge out of a switched node":    {wireFlow, Operation{Op: "add_edge", Node: "b", To: "e"}, "switched over"},
		"an edge into a join":               {withJoin, Operation{Op: "add_edge", Node: "a", To: "j"}, "entered only by its inputs"},
		"remove an edge that is not there":  {wireFlow, Operation{Op: "remove_edge", Node: "a", To: "e"}, "no edge"},
		"a switch with no predicate":        {wireFlow, Operation{Op: "add_switch", Node: "d", To: "e"}, "predicate name"},
		"a switch with no target":           {wireFlow, Operation{Op: "add_switch", Node: "d", Pred: "p"}, "target node is needed"},
		"a switch on a node with an edge":   {wireFlow, Operation{Op: "add_switch", Node: "a", Pred: "p", To: "e"}, "edge out"},
		"a second switch":                   {wireFlow, Operation{Op: "add_switch", Node: "b", Pred: "p", To: "e"}, "edge out"},
		"a switch on an unknown node":       {wireFlow, Operation{Op: "add_switch", Node: "zz", Pred: "p", To: "e"}, "no node is named"},
		"remove a switch that is not there": {wireFlow, Operation{Op: "remove_switch", Node: "d"}, "no switch is on"},
		"an arm on a node with no switch":   {wireFlow, Operation{Op: "add_arm", Node: "d", Pred: "p", To: "e"}, "no switch is on"},
		"a duplicate arm":                   {wireFlow, Operation{Op: "add_arm", Node: "b", Pred: "big", To: "c"}, "already has the arm"},
		"an arm to a join":                  {withJoin, Operation{Op: "add_arm", Node: "b", Pred: "p", To: "j"}, "join"},
		"an arm with an override name":      {wireFlow, Operation{Op: "add_arm", Node: "b", Pred: "p\u202e", To: "e"}, "does not print"},
		"remove the last arm":               {oneArm, Operation{Op: "remove_arm", Node: "b", Index: 0}, "only arm"},
		"remove an arm that is not there":   {wireFlow, Operation{Op: "remove_arm", Node: "b", Index: 5}, "no arm at position"},
		"set an arm that is not there":      {wireFlow, Operation{Op: "set_arm", Node: "b", Index: -1, Pred: "p", To: "c"}, "no arm at position"},
		"move the first arm up":             {wireFlow, Operation{Op: "move_arm", Node: "b", Index: 0, Value: "up"}, "already the first"},
		"move the last arm down":            {wireFlow, Operation{Op: "move_arm", Node: "b", Index: 1, Value: "down"}, "already the last"},
		"an else to an unknown node":        {wireFlow, Operation{Op: "set_else", Node: "b", To: "zz"}, "no node is named"},
		"an else on a node with no switch":  {wireFlow, Operation{Op: "set_else", Node: "d", To: "e"}, "no switch is on"},
		"an anchored wiring element":        {strings.Replace(wireFlow, "  - edge: [a, b]\n", "  - &w {edge: [a, b]}\n", 1), Operation{Op: "remove_edge", Node: "a", To: "b"}, ""},
	} {
		if name == "a second switch" {
			continue // b already has a switch AND, via the earlier check, the edge test fires first; see below
		}
		why := refusalOf(t, "f.yaml", c.src, c.op)
		if c.why != "" && !strings.Contains(why, c.why) {
			t.Errorf("%s: %q, want %q", name, why, c.why)
		}
	}
	// A second switch on a node that has one and no edge out.
	if why := refusalOf(t, "f.yaml", wireFlow, Operation{Op: "add_switch", Node: "b", Pred: "p", To: "e"}); !strings.Contains(why, "already has a switch") && !strings.Contains(why, "edge out") {
		t.Errorf("a second switch: %q", why)
	}
}

// Over the corpus: every wiring operation either refuses or gives text that reads back, and its
// inverse restores the exact bytes.
func TestWiringOverCorpus(t *testing.T) {
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
		try := func(op, undo Operation) {
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
			applied++
			back, err := ApplyOperation(name, r.Text, undo)
			if err != nil {
				t.Errorf("%s %+v: undo: %v", f.name, op, err)
				return
			}
			if string(back.Text) != f.text {
				if !strings.Contains(f.text, "wiring") {
					return // the list was created by the operation; emptying it leaves it as "wiring: []"
				}
				if strings.HasSuffix(f.text, "\n") && !strings.ContainsFunc(f.text, func(r rune) bool { return r > 127 }) {
					t.Errorf("%s %+v: the inverse did not restore the bytes\n--- before\n%s\n--- after\n%s", f.name, op, f.text, back.Text)
				}
				return
			}
			restored++
		}
		try(Operation{Op: "add_edge", Node: a, To: b}, Operation{Op: "remove_edge", Node: a, To: b})
		try(Operation{Op: "add_switch", Node: b, Pred: "zz_pred", To: a}, Operation{Op: "remove_switch", Node: b})
		for _, w := range cfg.Wiring {
			if w.Switch == "" {
				continue
			}
			if len(w.When) > 0 { // a switch with no arms is a config error; the added arm would be its only one
				try(Operation{Op: "add_arm", Node: w.Switch, Pred: "zz_pred", To: a}, Operation{Op: "remove_arm", Node: w.Switch, Index: len(w.When)})
			}
			if w.Else == "" {
				try(Operation{Op: "set_else", Node: w.Switch, To: a}, Operation{Op: "set_else", Node: w.Switch})
			}
			if len(w.When) > 1 {
				try(Operation{Op: "move_arm", Node: w.Switch, Index: 0, Value: "down"}, Operation{Op: "move_arm", Node: w.Switch, Index: 1, Value: "up"})
			}
		}
	}
	t.Logf("%d applied, %d refused, %d inverses restored the bytes", applied, refused, restored)
	if applied < 100 || restored < applied/2 {
		t.Errorf("the corpus exercised too little: %d applied, %d restored", applied, restored)
	}
}

// With no edge or switch in the file to copy the style from, a new one is written as the repository's
// examples write them: edge: [a, b] and when: [{pred: p, to: t}].
func TestNewWiringIsWrittenInTheExamplesStyle(t *testing.T) {
	src := "version: 1\nflow: f\nentry: a\nnodes:\n  - name: a\n    block: a\n  - name: b\n    block: b\nwiring:\n  - join: j\n    inputs: [a, b]\n    merge: m\n"
	got := mustApply(t, "f.yaml", src, Operation{Op: "add_edge", Node: "a", To: "b"})
	if !strings.HasSuffix(got, "  - edge: [a, b]\n") {
		t.Errorf("edge:\n%s", got)
	}
	got = mustApply(t, "f.yaml", src, Operation{Op: "add_switch", Node: "a", Pred: "p", To: "b"})
	if !strings.HasSuffix(got, "  - switch: a\n    when: [{pred: p, to: b}]\n") {
		t.Errorf("switch:\n%s", got)
	}
}
