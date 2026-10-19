package flowedit

import (
	"encoding/json"
	"fmt"
	"slices"
)

// The join and loop operations of docs/design/visual-editor.md ("Operations"): add and remove a join,
// set its inputs and its merge, and set an arm's loop_max. A join is not a declared node: it is a
// wiring element {join, inputs, merge}, so its gate and safety are edited at that element (see
// ApplyOperation). The editor cannot check that the merge block is registered or that its arity and
// input types fit the inputs (plan.Load does, with the Registry); it checks what the config alone says.

func isJoinOp(op string) bool {
	switch op {
	case "add_join", "remove_join", "set_join_inputs", "set_join_merge", "set_loop_max":
		return true
	}
	return false
}

// maxLoop bounds a loop_max the editor will write: a runaway-loop guard, not a throughput setting.
const maxLoop = 100000

// joinAt is the position in the wiring of the join named name.
func (c *structDoc) joinAt(name string) (int, error) {
	at := -1
	for i, w := range c.Wiring {
		if w.Join != name || name == "" {
			continue
		}
		if at >= 0 {
			return 0, refuse("more than one join is named %q; edit it in the source", name)
		}
		at = i
	}
	if at < 0 {
		return 0, refuse("no join is named %q", name)
	}
	return at, nil
}

// checkInputs checks the producers a join fans in: at least one, each a declared node or a join other
// than the join itself, none twice.
func (c *structDoc) checkInputs(self string, inputs []string) error {
	if len(inputs) == 0 {
		return refuse("a join needs at least one input")
	}
	seen := map[string]bool{}
	for _, in := range inputs {
		switch {
		case in == self:
			return refuse("a join cannot take its own output as an input")
		case !c.isNode(in) && !c.isJoin(in):
			return refuse("no node is named %q", in)
		case seen[in]:
			return refuse("the input %q is named twice", in)
		}
		seen[in] = true
	}
	return nil
}

func planJoin(cfg *structDoc, top map[string]json.RawMessage, op Operation) ([]Op, error) {
	switch op.Op {
	case "add_join":
		if err := checkName(op.Node); err != nil {
			return nil, err
		}
		if cfg.isNode(op.Node) || cfg.isJoin(op.Node) {
			return nil, refuse("a node or join named %q is already there", op.Node)
		}
		if op.Value == "" {
			return nil, refuse("a join needs a merge block")
		}
		if err := checkName(op.Value); err != nil {
			return nil, err
		}
		if err := cfg.checkInputs(op.Node, op.Inputs); err != nil {
			return nil, err
		}
		inputs := make([]any, len(op.Inputs))
		for i, in := range op.Inputs {
			inputs[i] = in
		}
		v := Map{{"join", op.Node}, {"inputs", inputs}, {"merge", op.Value}}
		raw, has := top["wiring"]
		switch {
		case !has:
			return []Op{{Kind: Insert, Path: nil, Key: "wiring", Value: []any{v}}}, nil
		case string(raw) == "null":
			return []Op{{Kind: Delete, Path: []any{"wiring"}}, {Kind: Insert, Path: nil, Key: "wiring", Value: []any{v}}}, nil
		}
		return []Op{{Kind: Insert, Path: []any{"wiring"}, Index: len(cfg.Wiring), Value: v}}, nil
	case "set_join_inputs":
		at, err := cfg.joinAt(op.Node)
		if err != nil {
			return nil, err
		}
		if err := cfg.checkInputs(op.Node, op.Inputs); err != nil {
			return nil, err
		}
		w := cfg.Wiring[at]
		path := []any{"wiring", at, "inputs"}
		if w.Inputs == nil {
			list := make([]any, len(op.Inputs))
			for i, in := range op.Inputs {
				list[i] = in
			}
			if raw, has := cfg.wireRaw[at]["inputs"]; has && string(raw) == "null" {
				return []Op{{Kind: Delete, Path: path}, {Kind: Insert, Path: []any{"wiring", at}, Key: "inputs", Value: list}}, nil
			}
			return []Op{{Kind: Insert, Path: []any{"wiring", at}, Key: "inputs", Value: list}}, nil
		}
		return listSteps(path, w.Inputs, op.Inputs), nil
	case "set_join_merge":
		at, err := cfg.joinAt(op.Node)
		if err != nil {
			return nil, err
		}
		if op.Value == "" {
			return nil, refuse("a join needs a merge block")
		}
		if err := checkName(op.Value); err != nil {
			return nil, err
		}
		switch w := cfg.Wiring[at]; {
		case w.Merge == op.Value:
			return nil, nil
		case w.Merge != "":
			return []Op{{Kind: Set, Path: []any{"wiring", at, "merge"}, Value: op.Value}}, nil
		}
		if raw, has := cfg.wireRaw[at]["merge"]; has && string(raw) == "null" {
			return []Op{{Kind: Delete, Path: []any{"wiring", at, "merge"}}, {Kind: Insert, Path: []any{"wiring", at}, Key: "merge", Value: op.Value}}, nil
		}
		return []Op{{Kind: Insert, Path: []any{"wiring", at}, Key: "merge", Value: op.Value}}, nil
	case "set_loop_max":
		at, err := cfg.switchAt(op.Node)
		if err != nil {
			return nil, err
		}
		w := cfg.Wiring[at]
		if op.Index < 0 || op.Index >= len(w.When) {
			return nil, refuse("the switch on %q has no arm at position %d (it has %d)", op.Node, op.Index, len(w.When))
		}
		if op.Max < 0 || op.Max > maxLoop {
			return nil, refuse("loop_max %d is not between 0 and %d (0 makes the arm an ordinary forward arm)", op.Max, maxLoop)
		}
		var arms []map[string]json.RawMessage
		if raw, ok := cfg.wireRaw[at]["when"]; ok {
			if json.Unmarshal(raw, &arms) != nil || len(arms) != len(w.When) {
				return nil, refuse("the arms of the switch on %q cannot be read; edit it in the source", op.Node)
			}
		}
		arm := []any{"wiring", at, "when", op.Index}
		cur, has := arms[op.Index]["loop_max"]
		switch {
		case op.Max == 0 && !has:
			return nil, nil
		case op.Max == 0:
			return []Op{{Kind: Delete, Path: append(slices.Clone(arm), "loop_max")}}, nil
		case has && string(cur) == fmt.Sprint(op.Max):
			return nil, nil
		case has && string(cur) != "null":
			return []Op{{Kind: Set, Path: append(slices.Clone(arm), "loop_max"), Value: op.Max}}, nil
		case has:
			return []Op{{Kind: Delete, Path: append(slices.Clone(arm), "loop_max")}, {Kind: Insert, Path: arm, Key: "loop_max", Value: op.Max}}, nil
		}
		return []Op{{Kind: Insert, Path: arm, Key: "loop_max", Value: op.Max}}, nil
	}
	return nil, refuse("operation %q is not one the editor offers", op.Op)
}

// planRemoveJoin removes a join and what names it, listing it like a node removal.
func planRemoveJoin(cfg *structDoc, op Operation) ([]Op, []string, error) {
	if _, err := cfg.joinAt(op.Node); err != nil {
		return nil, nil, err
	}
	if cfg.isNode(op.Node) {
		return nil, nil, refuse("%q is both a node and a join; edit it in the source", op.Node)
	}
	return cascadeWiring(cfg, op.Node, true)
}

// expectJoin is the document a join or loop operation should produce from old, computed on the data alone.
func expectJoin(doc map[string]any, op Operation) error {
	wiring, _ := doc["wiring"].([]any)
	strs := func(in []string) []any {
		out := make([]any, len(in))
		for i, s := range in {
			out[i] = s
		}
		return out
	}
	join := func() map[string]any {
		for _, w := range wiring {
			if m := asMap(w); m["join"] == op.Node {
				return m
			}
		}
		return nil
	}
	switch op.Op {
	case "add_join":
		doc["wiring"] = append(wiring, map[string]any{"join": op.Node, "inputs": strs(op.Inputs), "merge": op.Value})
	case "set_join_inputs":
		m := join()
		if m == nil {
			return fmt.Errorf("no join")
		}
		m["inputs"] = strs(op.Inputs)
	case "set_join_merge":
		m := join()
		if m == nil {
			return fmt.Errorf("no join")
		}
		m["merge"] = op.Value
	case "set_loop_max":
		for _, w := range wiring {
			m := asMap(w)
			if m["switch"] != op.Node {
				continue
			}
			arms, _ := m["when"].([]any)
			if op.Index < 0 || op.Index >= len(arms) {
				return fmt.Errorf("no arm")
			}
			arm := asMap(arms[op.Index])
			if op.Max == 0 {
				delete(arm, "loop_max")
			} else {
				arm["loop_max"] = float64(op.Max)
			}
			return nil
		}
		return fmt.Errorf("no switch")
	}
	return nil
}
