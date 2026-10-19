package flowedit

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bide-ai/bide/internal/printable"
)

// The structure operations of docs/design/visual-editor.md ("Operations", phase 2): add, remove and
// rename a node, and make the entry explicit. They are planned as engine splices like the others,
// and then checked a second way: the data the result should have is computed from the operation
// alone, on the decoded document (expectStructure), and the result must decode to it. The two share
// no code beyond the assertion grammar, so a plan that is wrong in a way each splice accepts is
// still caught.

func isStructureOp(op string) bool {
	if isWiringOp(op) {
		return true
	}
	switch op {
	case "add_node", "remove_node", "rename_node", "set_entry", "move_node", "move_wiring", "set_flow", "set_in", "set_out":
		return true
	}
	return false
}

// maxNameLen bounds a node name the editor will write.
const maxNameLen = 200

// checkName refuses a name the editor will not write: empty, padded, too long, or holding a character
// that is not printable (control, invisible, bidirectional, private-use, a space other than a plain
// one), so no name can read as another in a diff or the outline. Whether a name is also unique is the caller's check.
func checkName(name string) error {
	switch {
	case name == "":
		return refuse("a node's name cannot be empty")
	case name != strings.TrimSpace(name):
		return refuse("the name %q starts or ends with a space", name)
	case len(name) > maxNameLen:
		return refuse("a name of more than %d bytes", maxNameLen)
	case !utf8.ValidString(name):
		return refuse("the name is not valid UTF-8")
	}
	if r, bad := printable.Bad(name); bad {
		// The rule plan applies at load (internal/printable): control, invisible and direction-changing
		// characters, other spaces, private-use and unassigned code points, everything Unicode marks
		// Default_Ignorable (the fillers, the variation selectors, the joiners), the Braille blank, a
		// leading mark. A name cannot read as another in a diff, and the file the editor writes loads.
		return refuse("the name %q has a character that does not print or draws as nothing (%s), or a space other than a plain one: control, invisible, direction-changing, joiner, variation selector, filler", printable.EscapeName(name), printable.Describe(r))
	}
	return nil
}

type armDoc struct {
	Pred string `json:"pred"`
	To   string `json:"to"`
}

type wireDoc struct {
	Edge   []string `json:"edge"`
	Switch string   `json:"switch"`
	When   []armDoc `json:"when"`
	Else   string   `json:"else"`
	Join   string   `json:"join"`
	Inputs []string `json:"inputs"`
}

type structDoc struct {
	Entry string `json:"entry"`
	Nodes []struct {
		Name string `json:"name"`
	} `json:"nodes"`
	Wiring []wireDoc `json:"wiring"`
	Assert []string  `json:"assert"`
}

func (c *structDoc) nodeIndex(name string) (int, error) {
	idx := -1
	for i, n := range c.Nodes {
		if n.Name != name {
			continue
		}
		if idx >= 0 {
			return 0, refuse("more than one node is named %q", name)
		}
		idx = i
	}
	if idx < 0 {
		return 0, refuse("no node is named %q", name)
	}
	return idx, nil
}

func (c *structDoc) isJoin(name string) bool {
	for _, w := range c.Wiring {
		if w.Join == name && name != "" {
			return true
		}
	}
	return false
}

// applyStructure applies a structure operation. data is src as JSON.
func applyStructure(name string, src, data []byte, op Operation) (*Applied, error) {
	var cfg structDoc
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, refuse("the config cannot be read for this operation: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, refuse("the config cannot be read: %v", err)
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return nil, refuse("the config cannot be read: %v", err)
	}
	var (
		steps   []Op
		cascade []string
		err     error
	)
	switch op.Op {
	case "add_node":
		steps, err = planAddNode(&cfg, top, op)
	case "remove_node":
		steps, cascade, err = planRemoveNode(&cfg, op)
	case "rename_node":
		steps, err = planRename(&cfg, op)
	case "set_entry":
		steps, err = planSetEntry(&cfg, top, op)
	case "move_node":
		steps, err = planMoveNode(&cfg, op)
	case "move_wiring":
		steps, err = planMoveWiring(&cfg, op)
	case "set_flow", "set_in", "set_out":
		steps, err = planSetScalar(&cfg, top, op)
	default:
		if isWiringOp(op.Op) {
			steps, err = planWiring(&cfg, top, op)
		}
	}
	if err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return &Applied{Text: src}, nil
	}
	out := src
	for _, st := range steps {
		r, err := Apply(out, st)
		if err != nil {
			return nil, err
		}
		out = r.Text
	}
	// The model check: what the result should decode to, from the operation and the old data alone.
	want, err := expectStructure(generic, op)
	if err != nil {
		return nil, refuse("internal: the expected result of %s cannot be computed: %v", op.Op, err)
	}
	outData := out
	if formatOf(name) == "yaml" {
		if outData, err = ToJSON(out); err != nil {
			return nil, refuse("the result does not parse: %v", err)
		}
	}
	var got any
	if err := json.Unmarshal(outData, &got); err != nil {
		return nil, refuse("the result does not parse: %v", err)
	}
	if !reflect.DeepEqual(canonical(got), canonical(want)) {
		return nil, refuse("the result of %s does not match what the operation should produce; nothing was changed (edit this in the source)", op.Op)
	}
	if op.Op == "remove_node" {
		for _, c := range RemovedComments(src, out) {
			cascade = append(cascade, "comment: "+c)
		}
		if op.DryRun {
			return &Applied{Text: src, Cascade: cascade}, nil
		}
		if len(cascade) > 0 && !slices.Equal(op.Confirm, cascade) {
			return nil, refuse("removing %q also removes: %s; confirm that list to remove it", op.Node, strings.Join(cascade, "; "))
		}
	}
	return &Applied{Text: out, Changed: string(out) != string(src), Cascade: cascade}, nil
}

// canonical drops the keys whose empty list, null and absence mean the same thing to the config
// (the list of nodes and the wiring), so the comparison does not depend on how the engine leaves a
// list it emptied.
func canonical(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{}
	for k, x := range m {
		if l, isList := x.([]any); (isList && len(l) == 0 || x == nil) && (k == "nodes" || k == "wiring") {
			continue
		}
		out[k] = x
	}
	return out
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// direction reads op.Value as up or down: the position the item moves to is one earlier or one later.
func direction(v string) (delta int, err error) {
	switch v {
	case "up":
		return -1, nil
	case "down":
		return 1, nil
	}
	return 0, refuse("move %q: want up or down", v)
}

// planMoveNode exchanges a node with its neighbour. Order is data (the topology digest commits to the
// order of nodes), and a config with no entry key starts at its first node, so a move that would put a
// different node first is refused until the entry is written.
func planMoveNode(cfg *structDoc, op Operation) ([]Op, error) {
	idx, err := cfg.nodeIndex(op.Node)
	if err != nil {
		return nil, err
	}
	delta, err := direction(op.Value)
	if err != nil {
		return nil, err
	}
	j := idx
	if delta < 0 {
		j = idx - 1
	}
	if j < 0 || j+1 >= len(cfg.Nodes) {
		return nil, refuse("%q is already the %s node", op.Node, map[int]string{-1: "first", 1: "last"}[delta])
	}
	if cfg.Entry == "" && j == 0 {
		return nil, implicitEntryRefusal("moving "+fmt.Sprintf("%q", op.Node), cfg.Nodes[0].Name)
	}
	return []Op{{Kind: Swap, Path: []any{"nodes"}, Index: j}}, nil
}

// planMoveWiring exchanges the wiring element at op.Index with its neighbour.
func planMoveWiring(cfg *structDoc, op Operation) ([]Op, error) {
	delta, err := direction(op.Value)
	if err != nil {
		return nil, err
	}
	if op.Index < 0 || op.Index >= len(cfg.Wiring) {
		return nil, refuse("the config has no wiring element at position %d (it has %d)", op.Index, len(cfg.Wiring))
	}
	j := op.Index
	if delta < 0 {
		j--
	}
	if j < 0 || j+1 >= len(cfg.Wiring) {
		return nil, refuse("wiring element %d is already the %s", op.Index+1, map[int]string{-1: "first", 1: "last"}[delta])
	}
	return []Op{{Kind: Swap, Path: []any{"wiring"}, Index: j}}, nil
}

// planSetScalar sets the flow's name, or the documentation type names in and out ("" removes them).
func planSetScalar(cfg *structDoc, top map[string]json.RawMessage, op Operation) ([]Op, error) {
	key := strings.TrimPrefix(op.Op, "set_")
	if op.Value == "" && key != "flow" {
		if _, has := top[key]; !has {
			return nil, nil
		}
		return []Op{{Kind: Delete, Path: []any{key}}}, nil
	}
	if op.Value == "" {
		return nil, refuse("the flow's name cannot be empty")
	}
	if err := checkName(op.Value); err != nil {
		return nil, err
	}
	raw, has := top[key]
	if !has {
		return []Op{{Kind: Insert, Path: nil, Key: key, Value: op.Value}}, nil
	}
	var cur string
	if json.Unmarshal(raw, &cur) == nil && cur == op.Value {
		return nil, nil
	}
	return []Op{{Kind: Set, Path: []any{key}, Value: op.Value}}, nil
}

func nodeMap(name, block string) Map { return Map{{"name", name}, {"block", block}} }

func planAddNode(cfg *structDoc, top map[string]json.RawMessage, op Operation) ([]Op, error) {
	if err := checkName(op.Node); err != nil {
		return nil, err
	}
	if op.Value == "" {
		return nil, refuse("a node's block cannot be empty")
	}
	for _, n := range cfg.Nodes {
		if n.Name == op.Node {
			return nil, refuse("a node named %q is already there", op.Node)
		}
	}
	if cfg.isJoin(op.Node) {
		return nil, refuse("%q is the name of a join; a node cannot share it", op.Node)
	}
	raw, has := top["nodes"]
	switch {
	case !has:
		return nil, refuse("the config has no nodes list; add `nodes:` in the source first")
	case string(raw) == "null":
		return []Op{{Kind: Delete, Path: []any{"nodes"}}, {Kind: Insert, Path: nil, Key: "nodes", Value: []any{nodeMap(op.Node, op.Value)}}}, nil
	}
	return []Op{{Kind: Insert, Path: []any{"nodes"}, Index: len(cfg.Nodes), Value: nodeMap(op.Node, op.Value)}}, nil
}

func planSetEntry(cfg *structDoc, top map[string]json.RawMessage, op Operation) ([]Op, error) {
	if op.Value == "" {
		// Clearing the entry leaves the first node as the entry: allowed only when it already is.
		if _, has := top["entry"]; !has {
			return nil, nil
		}
		if len(cfg.Nodes) == 0 || cfg.Entry == cfg.Nodes[0].Name || cfg.Entry == "" {
			return []Op{{Kind: Delete, Path: []any{"entry"}}}, nil
		}
		return nil, refuse("clearing the entry would make %q the entry (the first node), not %q; set the entry to it instead", cfg.Nodes[0].Name, cfg.Entry)
	}
	if _, err := cfg.nodeIndex(op.Value); err != nil {
		return nil, err
	}
	if _, has := top["entry"]; has {
		if cfg.Entry == op.Value {
			return nil, nil
		}
		return []Op{{Kind: Set, Path: []any{"entry"}, Value: op.Value}}, nil
	}
	return []Op{{Kind: Insert, Path: nil, Key: "entry", Value: op.Value}}, nil
}

// implicitEntryRefusal is the refusal of an operation that would move the entry the config leaves
// implicit (the first node): the entry is part of what the digest commits to, and the editor never
// changes it without the author saying so.
func implicitEntryRefusal(what string, first string) error {
	return refuse("%s would change the entry node, which this config leaves implicit (it is the first node, %q); make the entry explicit first with set_entry", what, first)
}

func planRemoveNode(cfg *structDoc, op Operation) ([]Op, []string, error) {
	idx, err := cfg.nodeIndex(op.Node)
	if err != nil {
		return nil, nil, err
	}
	if cfg.isJoin(op.Node) {
		return nil, nil, refuse("%q is both a node and a join; edit it in the source", op.Node)
	}
	switch {
	case cfg.Entry == op.Node:
		return nil, nil, refuse("%q is the entry node; make another node the entry first (set_entry)", op.Node)
	case cfg.Entry == "" && idx == 0 && len(cfg.Nodes) > 1:
		return nil, nil, implicitEntryRefusal("removing "+fmt.Sprintf("%q", op.Node), op.Node)
	}
	var steps []Op
	var cascade []string
	// Wiring from the end, so the indexes of the elements still to come stay valid; within an element
	// the arms go from the end for the same reason.
	for i := len(cfg.Wiring) - 1; i >= 0; i-- {
		w := cfg.Wiring[i]
		switch {
		case w.Edge != nil:
			if slices.Contains(w.Edge, op.Node) {
				steps = append(steps, Op{Kind: Delete, Path: []any{"wiring", i}})
				cascade = append(cascade, fmt.Sprintf("wiring %d: edge %s", i+1, strings.Join(w.Edge, " -> ")))
			}
		case w.Switch != "":
			if w.Switch == op.Node {
				steps = append(steps, Op{Kind: Delete, Path: []any{"wiring", i}})
				cascade = append(cascade, fmt.Sprintf("wiring %d: the switch on %s, with its %d arm(s)", i+1, w.Switch, len(w.When)))
				continue
			}
			var hit []int
			for j, a := range w.When {
				if a.To == op.Node {
					hit = append(hit, j)
				}
			}
			elseHit := w.Else == op.Node
			if len(hit) == 0 && !elseHit {
				continue
			}
			if len(hit) == len(w.When) {
				steps = append(steps, Op{Kind: Delete, Path: []any{"wiring", i}})
				cascade = append(cascade, fmt.Sprintf("wiring %d: the switch on %s (it would be left with no arm)", i+1, w.Switch))
				continue
			}
			if elseHit {
				steps = append(steps, Op{Kind: Delete, Path: []any{"wiring", i, "else"}})
				cascade = append(cascade, fmt.Sprintf("wiring %d: else -> %s of the switch on %s", i+1, op.Node, w.Switch))
			}
			for k := len(hit) - 1; k >= 0; k-- {
				steps = append(steps, Op{Kind: Delete, Path: []any{"wiring", i, "when", hit[k]}})
				cascade = append(cascade, fmt.Sprintf("wiring %d: arm %s -> %s of the switch on %s", i+1, w.When[hit[k]].Pred, op.Node, w.Switch))
			}
		case w.Join != "":
			if slices.Contains(w.Inputs, op.Node) {
				return nil, nil, refuse("%q is an input of the join %q; remove or change the join first", op.Node, w.Join)
			}
		}
	}
	steps = append(steps, Op{Kind: Delete, Path: []any{"nodes", idx}})
	// Assertions are left in place: removing one would weaken the policy, and the check reports a
	// line that names a node that is gone.
	for k, line := range cfg.Assert {
		if slices.Contains(assertionNames(line), op.Node) {
			cascade = append(cascade, fmt.Sprintf("assert %d: %q stays and is reported as naming a missing node", k+1, line))
		}
	}
	slices.Reverse(cascade) // wiring order, not the order the splices run in
	return steps, cascade, nil
}

// assertionNames is the node names an assert line mentions: the form's nodes when the line parses,
// and its tokens (quotes stripped) when it does not, so a line the grammar cannot read is not
// silently treated as unrelated.
func assertionNames(line string) []string {
	if a, ok := ParseAssertion(line); ok {
		var out []string
		for _, n := range []string{a.Node, a.Via} {
			if n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	var out []string
	for _, f := range strings.Fields(line) {
		out = append(out, strings.Trim(f, `"`))
	}
	return out
}

// renameAssertion is line with the node old named new, written through the grammar; ok is false when
// the line does not name old. A line that names it but cannot be written back (the grammar cannot
// hold the new name, or the line is none of the six forms) is an error.
func renameAssertion(line, old, new string) (out string, ok bool, err error) {
	a, parsed := ParseAssertion(line)
	if !parsed {
		if slices.Contains(assertionNames(line), old) {
			return "", false, refuse("an assertion that is not one of the six forms names %q: %q; edit it in the source", old, line)
		}
		return line, false, nil
	}
	hit := false
	if a.Node == old {
		a.Node, hit = new, true
	}
	if a.Via == old {
		a.Via, hit = new, true
	}
	if !hit {
		return line, false, nil
	}
	text, err := a.Text()
	if err != nil {
		return "", false, err
	}
	return text, true, nil
}

func planRename(cfg *structDoc, op Operation) ([]Op, error) {
	idx, err := cfg.nodeIndex(op.Node)
	if err != nil {
		return nil, err
	}
	if cfg.isJoin(op.Node) {
		return nil, refuse("%q is both a node and a join; edit it in the source", op.Node)
	}
	if op.Value == op.Node {
		return nil, nil
	}
	if err := checkName(op.Value); err != nil {
		return nil, err
	}
	for _, n := range cfg.Nodes {
		if n.Name == op.Value {
			return nil, refuse("a node named %q is already there", op.Value)
		}
	}
	if cfg.isJoin(op.Value) {
		return nil, refuse("%q is the name of a join; a node cannot share it", op.Value)
	}
	set := func(v string, path ...any) Op { return Op{Kind: Set, Path: path, Value: v} }
	steps := []Op{set(op.Value, "nodes", idx, "name")}
	if cfg.Entry == op.Node {
		steps = append(steps, set(op.Value, "entry"))
	}
	for i, w := range cfg.Wiring {
		for j, e := range w.Edge {
			if e == op.Node {
				steps = append(steps, set(op.Value, "wiring", i, "edge", j))
			}
		}
		if w.Switch == op.Node {
			steps = append(steps, set(op.Value, "wiring", i, "switch"))
		}
		for j, a := range w.When {
			if a.To == op.Node {
				steps = append(steps, set(op.Value, "wiring", i, "when", j, "to"))
			}
		}
		if w.Else == op.Node {
			steps = append(steps, set(op.Value, "wiring", i, "else"))
		}
		for j, in := range w.Inputs {
			if in == op.Node {
				steps = append(steps, set(op.Value, "wiring", i, "inputs", j))
			}
		}
	}
	for k, line := range cfg.Assert {
		text, ok, err := renameAssertion(line, op.Node, op.Value)
		if err != nil {
			return nil, refuse("assert %d cannot be renamed: %v", k+1, strings.TrimPrefix(err.Error(), "flowedit: refused: "))
		}
		if ok {
			steps = append(steps, set(text, "assert", k))
		}
	}
	return steps, nil
}

// expectStructure is the document op should produce from old (the decoded JSON of the old text),
// computed on the data alone. It is the oracle planned splices are checked against; it shares no
// code with the planners.
func expectStructure(old any, op Operation) (any, error) {
	b, _ := json.Marshal(old)
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	nodes, _ := doc["nodes"].([]any)
	wiring, _ := doc["wiring"].([]any)
	if isWiringOp(op.Op) {
		if err := expectWiring(doc, op); err != nil {
			return nil, err
		}
		return doc, nil
	}
	switch op.Op {
	case "add_node":
		doc["nodes"] = append(nodes, map[string]any{"name": op.Node, "block": op.Value})
	case "set_entry":
		if op.Value == "" {
			delete(doc, "entry")
		} else {
			doc["entry"] = op.Value
		}
	case "rename_node":
		for _, n := range nodes {
			if m := asMap(n); m["name"] == op.Node {
				m["name"] = op.Value
			}
		}
		if doc["entry"] == op.Node {
			doc["entry"] = op.Value
		}
		sub := func(v any) any {
			if v == op.Node {
				return op.Value
			}
			return v
		}
		for _, w := range wiring {
			m := asMap(w)
			for _, key := range []string{"switch", "else"} {
				if v, ok := m[key]; ok {
					m[key] = sub(v)
				}
			}
			for _, key := range []string{"edge", "inputs"} {
				if l, ok := m[key].([]any); ok {
					for i := range l {
						l[i] = sub(l[i])
					}
				}
			}
			if l, ok := m["when"].([]any); ok {
				for _, a := range l {
					am := asMap(a)
					am["to"] = sub(am["to"])
				}
			}
		}
		if lines, ok := doc["assert"].([]any); ok {
			for i, l := range lines {
				s, _ := l.(string)
				if t, hit, err := renameAssertion(s, op.Node, op.Value); err != nil {
					return nil, err
				} else if hit {
					lines[i] = t
				}
			}
		}
	case "move_node", "move_wiring":
		list, key := nodes, "nodes"
		j := -1
		if op.Op == "move_wiring" {
			list, key = wiring, "wiring"
			j = op.Index
		} else {
			for i, n := range nodes {
				if asMap(n)["name"] == op.Node {
					j = i
				}
			}
		}
		if op.Value == "up" {
			j--
		}
		if j < 0 || j+1 >= len(list) {
			return nil, fmt.Errorf("no pair at %d", j)
		}
		list[j], list[j+1] = list[j+1], list[j]
		doc[key] = list
	case "set_flow", "set_in", "set_out":
		key := strings.TrimPrefix(op.Op, "set_")
		if op.Value == "" && key != "flow" {
			delete(doc, key)
		} else {
			doc[key] = op.Value
		}
	case "remove_node":
		var keptNodes []any
		for _, n := range nodes {
			if asMap(n)["name"] != op.Node {
				keptNodes = append(keptNodes, n)
			}
		}
		doc["nodes"] = keptNodes
		kept := []any{}
		for _, w := range wiring {
			m := asMap(w)
			if l, ok := m["edge"].([]any); ok && slices.Contains(l, any(op.Node)) {
				continue
			}
			if _, isSwitch := m["switch"]; isSwitch {
				if m["switch"] == op.Node {
					continue
				}
				arms, _ := m["when"].([]any)
				var keptArms []any
				for _, a := range arms {
					if asMap(a)["to"] != op.Node {
						keptArms = append(keptArms, a)
					}
				}
				if len(keptArms) == 0 && len(arms) > 0 {
					continue
				}
				if len(keptArms) != len(arms) {
					m["when"] = keptArms
				}
				if m["else"] == op.Node {
					delete(m, "else")
				}
			}
			kept = append(kept, w)
		}
		doc["wiring"] = kept
	}
	return doc, nil
}
