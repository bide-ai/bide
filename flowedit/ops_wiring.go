package flowedit

import (
	"encoding/json"
	"fmt"
	"slices"
)

// The wiring operations of docs/design/visual-editor.md ("Operations"): add and remove an edge, add
// and remove a switch, add, change, move and remove an arm, set or clear the else. Node names the
// node an edge leaves or a switch is on; To is where it goes. They are planned as engine splices and
// checked against the data the operation should produce (expectWiring), like the node operations.
//
// What the editor will not write, because plan.Load refuses it ("Load-time validation" in
// docs/guides/plan-flows.md): an edge or an arm to a node that is not declared, an edge into a join
// (a join is entered only by its inputs), an edge from a node that has a switch (a node is switched
// over or has an edge out, not both), a switch on a node that has an edge out or a switch already, a
// switch with no arm, and an else without a switch. The report still catches all of these if they
// arrive through the source pane.

func isWiringOp(op string) bool {
	switch op {
	case "add_edge", "remove_edge", "add_switch", "remove_switch", "add_arm", "remove_arm", "set_arm", "move_arm", "set_else":
		return true
	}
	return false
}

func (c *structDoc) isNode(name string) bool {
	return slices.ContainsFunc(c.Nodes, func(n struct {
		Name string `json:"name"`
	}) bool {
		return n.Name == name
	})
}

// hasEdgeOut reports whether an edge in the wiring leaves name.
func (c *structDoc) hasEdgeOut(name string) bool {
	return slices.ContainsFunc(c.Wiring, func(w wireDoc) bool { return len(w.Edge) == 2 && w.Edge[0] == name })
}

// switchAt is the position in the wiring of the switch on name, and an error when there is none or more
// than one.
func (c *structDoc) switchAt(name string) (int, error) {
	at := -1
	for i, w := range c.Wiring {
		if w.Switch != name {
			continue
		}
		if at >= 0 {
			return 0, refuse("more than one switch is on %q; edit it in the source", name)
		}
		at = i
	}
	if at < 0 {
		return 0, refuse("no switch is on %q", name)
	}
	return at, nil
}

// target checks a name an edge or an arm goes to: a declared node, not a join.
func (c *structDoc) target(name string) error {
	switch {
	case name == "":
		return refuse("where to? a target node is needed")
	case c.isJoin(name):
		return refuse("%q is a join, which is entered only by its inputs", name)
	case !c.isNode(name):
		return refuse("no node is named %q", name)
	}
	return nil
}

func checkPred(pred string) error {
	if pred == "" {
		return refuse("an arm needs a predicate name")
	}
	return checkName(pred)
}

func armMap(pred, to string) Map { return Map{{"pred", pred}, {"to", to}} }

func planWiring(cfg *structDoc, top map[string]json.RawMessage, op Operation) ([]Op, error) {
	appendWire := func(v Map) ([]Op, error) {
		raw, has := top["wiring"]
		switch {
		case !has:
			return []Op{{Kind: Insert, Path: nil, Key: "wiring", Value: []any{v}}}, nil
		case string(raw) == "null":
			return []Op{{Kind: Delete, Path: []any{"wiring"}}, {Kind: Insert, Path: nil, Key: "wiring", Value: []any{v}}}, nil
		}
		return []Op{{Kind: Insert, Path: []any{"wiring"}, Index: len(cfg.Wiring), Value: v}}, nil
	}
	switch op.Op {
	case "add_edge":
		if !cfg.isNode(op.Node) && !cfg.isJoin(op.Node) {
			return nil, refuse("no node is named %q", op.Node)
		}
		if err := cfg.target(op.To); err != nil {
			return nil, err
		}
		if op.Node == op.To {
			return nil, refuse("an edge from %q to itself is a cycle; a loop is an arm with loop_max", op.Node)
		}
		for _, w := range cfg.Wiring {
			if len(w.Edge) == 2 && w.Edge[0] == op.Node && w.Edge[1] == op.To {
				return nil, refuse("the edge %s -> %s is already there", op.Node, op.To)
			}
			if w.Switch == op.Node {
				return nil, refuse("%q is switched over; a node has a switch or an edge out, not both", op.Node)
			}
		}
		return appendWire(Map{{"edge", []any{op.Node, op.To}}})
	case "remove_edge":
		for i, w := range cfg.Wiring {
			if len(w.Edge) == 2 && w.Edge[0] == op.Node && w.Edge[1] == op.To {
				return []Op{{Kind: Delete, Path: []any{"wiring", i}}}, nil
			}
		}
		return nil, refuse("there is no edge %s -> %s", op.Node, op.To)
	case "add_switch":
		if !cfg.isNode(op.Node) {
			return nil, refuse("no node is named %q", op.Node)
		}
		if err := checkPred(op.Pred); err != nil {
			return nil, err
		}
		if err := cfg.target(op.To); err != nil {
			return nil, err
		}
		if cfg.hasEdgeOut(op.Node) {
			return nil, refuse("%q has an edge out; a node has a switch or an edge out, not both", op.Node)
		}
		if _, err := cfg.switchAt(op.Node); err == nil {
			return nil, refuse("%q already has a switch", op.Node)
		}
		return appendWire(Map{{"switch", op.Node}, {"when", []any{armMap(op.Pred, op.To)}}})
	case "remove_switch":
		at, err := cfg.switchAt(op.Node)
		if err != nil {
			return nil, err
		}
		return []Op{{Kind: Delete, Path: []any{"wiring", at}}}, nil
	}
	at, err := cfg.switchAt(op.Node)
	if err != nil {
		return nil, err
	}
	w := cfg.Wiring[at]
	arm := func() error {
		if op.Index < 0 || op.Index >= len(w.When) {
			return refuse("the switch on %q has no arm at position %d (it has %d)", op.Node, op.Index, len(w.When))
		}
		return nil
	}
	when := []any{"wiring", at, "when"}
	switch op.Op {
	case "add_arm":
		if err := checkPred(op.Pred); err != nil {
			return nil, err
		}
		if err := cfg.target(op.To); err != nil {
			return nil, err
		}
		for _, a := range w.When {
			if a.Pred == op.Pred && a.To == op.To {
				return nil, refuse("the switch on %q already has the arm %s -> %s", op.Node, op.Pred, op.To)
			}
		}
		// A switch whose when is absent or null gets the key written (the null one is replaced); one with a
		// list gets the arm appended.
		raw, has := cfg.wireRaw[at]["when"]
		switch {
		case !has:
			return []Op{{Kind: Insert, Path: []any{"wiring", at}, Key: "when", Value: []any{armMap(op.Pred, op.To)}}}, nil
		case string(raw) == "null":
			return []Op{{Kind: Delete, Path: slices.Clone(when)}, {Kind: Insert, Path: []any{"wiring", at}, Key: "when", Value: []any{armMap(op.Pred, op.To)}}}, nil
		}
		return []Op{{Kind: Insert, Path: when, Index: len(w.When), Value: armMap(op.Pred, op.To)}}, nil
	case "remove_arm":
		if err := arm(); err != nil {
			return nil, err
		}
		if len(w.When) == 1 {
			return nil, refuse("that is the only arm of the switch on %q; remove the switch to remove it", op.Node)
		}
		return []Op{{Kind: Delete, Path: append(slices.Clone(when), op.Index)}}, nil
	case "set_arm":
		if err := arm(); err != nil {
			return nil, err
		}
		if err := checkPred(op.Pred); err != nil {
			return nil, err
		}
		if err := cfg.target(op.To); err != nil {
			return nil, err
		}
		var steps []Op
		if w.When[op.Index].Pred != op.Pred {
			steps = append(steps, Op{Kind: Set, Path: append(slices.Clone(when), op.Index, "pred"), Value: op.Pred})
		}
		if w.When[op.Index].To != op.To {
			steps = append(steps, Op{Kind: Set, Path: append(slices.Clone(when), op.Index, "to"), Value: op.To})
		}
		return steps, nil
	case "move_arm":
		delta, err := direction(op.Value)
		if err != nil {
			return nil, err
		}
		if err := arm(); err != nil {
			return nil, err
		}
		j := op.Index
		if delta < 0 {
			j--
		}
		if j < 0 || j+1 >= len(w.When) {
			return nil, refuse("that arm is already the %s", map[int]string{-1: "first", 1: "last"}[delta])
		}
		return []Op{{Kind: Swap, Path: when, Index: j}}, nil
	case "set_else":
		if op.To == "" {
			if w.Else == "" {
				return nil, nil
			}
			return []Op{{Kind: Delete, Path: []any{"wiring", at, "else"}}}, nil
		}
		if err := cfg.target(op.To); err != nil {
			return nil, err
		}
		switch {
		case w.Else == op.To:
			return nil, nil
		case w.Else != "":
			return []Op{{Kind: Set, Path: []any{"wiring", at, "else"}, Value: op.To}}, nil
		}
		if raw, has := cfg.wireRaw[at]["else"]; has && string(raw) == "null" {
			return []Op{{Kind: Delete, Path: []any{"wiring", at, "else"}}, {Kind: Insert, Path: []any{"wiring", at}, Key: "else", Value: op.To}}, nil
		}
		return []Op{{Kind: Insert, Path: []any{"wiring", at}, Key: "else", Value: op.To}}, nil
	}
	return nil, refuse("operation %q is not one the editor offers", op.Op)
}

// expectWiring is the document a wiring operation should produce from old, computed on the data alone.
func expectWiring(doc map[string]any, op Operation) error {
	wiring, _ := doc["wiring"].([]any)
	find := func(pred func(m map[string]any) bool) int {
		for i, w := range wiring {
			if pred(asMap(w)) {
				return i
			}
		}
		return -1
	}
	isEdge := func(m map[string]any) bool {
		e, _ := m["edge"].([]any)
		return len(e) == 2 && e[0] == op.Node && e[1] == op.To
	}
	onNode := func(m map[string]any) bool { s, ok := m["switch"]; return ok && s == op.Node }
	switch op.Op {
	case "add_edge":
		doc["wiring"] = append(wiring, map[string]any{"edge": []any{op.Node, op.To}})
	case "remove_edge":
		i := find(isEdge)
		if i < 0 {
			return fmt.Errorf("no edge")
		}
		doc["wiring"] = slices.Delete(wiring, i, i+1)
	case "add_switch":
		doc["wiring"] = append(wiring, map[string]any{"switch": op.Node, "when": []any{map[string]any{"pred": op.Pred, "to": op.To}}})
	case "remove_switch":
		i := find(onNode)
		if i < 0 {
			return fmt.Errorf("no switch")
		}
		doc["wiring"] = slices.Delete(wiring, i, i+1)
	default:
		i := find(onNode)
		if i < 0 {
			return fmt.Errorf("no switch")
		}
		sw := asMap(wiring[i])
		when, _ := sw["when"].([]any)
		switch op.Op {
		case "add_arm":
			sw["when"] = append(when, map[string]any{"pred": op.Pred, "to": op.To})
		case "remove_arm":
			sw["when"] = slices.Delete(when, op.Index, op.Index+1)
		case "set_arm":
			asMap(when[op.Index])["pred"] = op.Pred
			asMap(when[op.Index])["to"] = op.To
		case "move_arm":
			j := op.Index
			if op.Value == "up" {
				j--
			}
			when[j], when[j+1] = when[j+1], when[j]
		case "set_else":
			if op.To == "" {
				delete(sw, "else")
			} else {
				sw["else"] = op.To
			}
		}
	}
	return nil
}
