package flowedit

import (
	"encoding/json"
	"slices"
)

// Operation is one edit the page asks for, in the closed vocabulary of docs/design/visual-editor.md
// ("Operations"). Every operation is applied to the config text as a sequence of engine splices
// (Apply), each verified, so the diff of an edit is the edit: comments, ordering and blank lines
// stay. An operation the text cannot take, or that Verify refuses, changes nothing.
type Operation struct {
	// Op names the operation: "set_safety", "set_block", "set_approval", "clear_approval",
	// "add_assertion", "set_assertion" or "remove_assertion".
	Op string `json:"op"`
	// Node is the node the operation is on, by its name in the config.
	Node string `json:"node"`
	// Value is the new safety ("readonly", "idempotent" or "side_effect"; "" removes the key, which
	// leaves the safety to the registered block) for set_safety, and the new block name for
	// set_block.
	Value string `json:"value,omitempty"`
	// Approval is the node's approval gate for set_approval.
	Approval *Approval `json:"approval,omitempty"`
	// Assertion and Index are for the assertion operations: add_assertion appends Assertion to the
	// config's assert list, set_assertion replaces the line at Index with it and remove_assertion
	// removes the line at Index. They name no node: Node is ignored.
	Assertion *Assertion `json:"assertion,omitempty"`
	Index     int        `json:"index,omitempty"`
}

// Approval is a node's approval gate as the config writes it: Need decisions from the Approvers,
// never counting the run's requester when ExcludePrincipal is set, and a deadline (a Go duration
// such as "48h", "" for none). The editor writes what it is given and lets the check say whether it
// is a valid gate: a half-built flow can be held and saved with errors, and the report names each.
type Approval struct {
	Need             int      `json:"need"`
	Approvers        []string `json:"approvers"`
	ExcludePrincipal bool     `json:"exclude_principal"`
	Timeout          string   `json:"timeout,omitempty"`
}

// The safety values a config accepts (plan's safetyFromConfig). The editor offers only these; whether
// one is above what the block declares in Go is Load's check, which the page says it cannot make.
var safeties = []string{"readonly", "idempotent", "side_effect"}

// maxApprovers bounds a gate the editor will write; a gate needs a person for each approver.
const maxApprovers = 100

// Applied is the result of an operation: the new text and whether it differs from the old.
type Applied struct {
	Text    []byte
	Changed bool
}

// nodeDoc is a node as the config text holds it, for finding what to edit.
type nodeDoc struct {
	Name     string         `json:"name"`
	Approval map[string]any `json:"approval"`
	has      map[string]bool
}

// ApplyOperation applies op to the config text src, which is YAML or JSON as name's extension says.
// A *Refusal error means the text is unchanged and says why: the operation is not in the
// vocabulary, names no node (or two), asks for a value the config cannot hold, or the engine would
// not splice the document safely.
func ApplyOperation(name string, src []byte, op Operation) (*Applied, error) {
	data := src
	if formatOf(name) == "yaml" {
		var err error
		if data, err = ToJSON(src); err != nil {
			return nil, refuse("the text does not parse: %v", err)
		}
	}
	if isAssertionOp(op.Op) {
		return applyAssertion(src, data, op)
	}
	var cfg struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, refuse("the config's nodes cannot be read: %v", err)
	}
	idx := -1
	var node nodeDoc
	for i, raw := range cfg.Nodes {
		var n nodeDoc
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, refuse("node %d cannot be read: %v", i+1, err)
		}
		if n.Name != op.Node {
			continue
		}
		if idx >= 0 {
			return nil, refuse("more than one node is named %q", op.Node)
		}
		idx, node = i, n
		var keys map[string]json.RawMessage
		_ = json.Unmarshal(raw, &keys)
		node.has = map[string]bool{}
		for k := range keys {
			node.has[k] = true
		}
	}
	if idx < 0 {
		return nil, refuse("no node is named %q", op.Node)
	}
	steps, err := planSteps(op, idx, node)
	if err != nil {
		return nil, err
	}
	text := src
	for _, st := range steps {
		r, err := Apply(text, st)
		if err != nil {
			return nil, err
		}
		text = r.Text
	}
	return &Applied{Text: text, Changed: string(text) != string(src)}, nil
}

// planSteps turns op on the node at idx into engine operations, in order. Each is applied to the text
// the one before left.
func planSteps(op Operation, idx int, n nodeDoc) ([]Op, error) {
	at := func(rest ...any) []any { return append([]any{"nodes", idx}, rest...) }
	// put sets key of the map at path to v: a Set when the key is there, an Insert when it is not.
	put := func(path []any, has bool, key string, v any) Op {
		if has {
			return Op{Kind: Set, Path: append(slices.Clone(path), key), Value: v}
		}
		return Op{Kind: Insert, Path: path, Key: key, Value: v}
	}
	switch op.Op {
	case "set_safety":
		if op.Value == "" {
			if !n.has["safety"] {
				return nil, nil
			}
			return []Op{{Kind: Delete, Path: at("safety")}}, nil
		}
		if !slices.Contains(safeties, op.Value) {
			return nil, refuse("safety %q is not one of %v", op.Value, safeties)
		}
		return []Op{put(at(), n.has["safety"], "safety", op.Value)}, nil
	case "set_block":
		if op.Value == "" {
			return nil, refuse("a node's block cannot be empty")
		}
		return []Op{{Kind: Set, Path: at("block"), Value: op.Value}}, nil
	case "clear_approval":
		if !n.has["approval"] {
			return nil, nil
		}
		return []Op{{Kind: Delete, Path: at("approval")}}, nil
	case "set_approval":
		return planApproval(op.Approval, idx, n, at, put)
	}
	return nil, refuse("operation %q is not one the editor offers", op.Op)
}

func planApproval(a *Approval, idx int, n nodeDoc, at func(...any) []any, put func([]any, bool, string, any) Op) ([]Op, error) {
	if a == nil {
		return nil, refuse("set_approval needs an approval")
	}
	if len(a.Approvers) > maxApprovers {
		return nil, refuse("a gate of more than %d approvers", maxApprovers)
	}
	approvers := make([]any, len(a.Approvers))
	for i, id := range a.Approvers {
		approvers[i] = id
	}
	if !n.has["approval"] {
		m := Map{{"need", a.Need}, {"approvers", approvers}}
		if a.ExcludePrincipal {
			m = append(m, KV{"exclude_principal", true})
		}
		if a.Timeout != "" {
			m = append(m, KV{"timeout", a.Timeout})
		}
		return []Op{{Kind: Insert, Path: at(), Key: "approval", Value: m}}, nil
	}
	if n.Approval == nil {
		return nil, refuse("node %q has an approval that is not a mapping; edit it in the source", n.Name)
	}
	cur := n.Approval
	var steps []Op
	ap := at("approval")
	curNeed, hasNeed := cur["need"].(float64)
	if !hasNeed || int(curNeed) != a.Need || float64(int(curNeed)) != curNeed {
		steps = append(steps, put(ap, hasNeed, "need", a.Need))
	}
	var curApprovers []string
	if list, ok := cur["approvers"].([]any); ok {
		for _, x := range list {
			s, isStr := x.(string)
			if !isStr {
				return nil, refuse("node %q has an approver that is not a string; edit it in the source", n.Name)
			}
			curApprovers = append(curApprovers, s)
		}
	} else if _, present := cur["approvers"]; present {
		return nil, refuse("node %q has approvers that are not a list; edit them in the source", n.Name)
	}
	if _, present := cur["approvers"]; !present {
		steps = append(steps, Op{Kind: Insert, Path: ap, Key: "approvers", Value: approvers})
	} else {
		steps = append(steps, listSteps(append(slices.Clone(ap), "approvers"), curApprovers, a.Approvers)...)
	}
	curExclude, _ := cur["exclude_principal"].(bool)
	_, hasExclude := cur["exclude_principal"]
	switch {
	case a.ExcludePrincipal && !curExclude:
		steps = append(steps, put(ap, hasExclude, "exclude_principal", true))
	case !a.ExcludePrincipal && hasExclude:
		steps = append(steps, Op{Kind: Delete, Path: append(slices.Clone(ap), "exclude_principal")})
	}
	curTimeout, hasTimeout := cur["timeout"]
	switch {
	case a.Timeout == "" && hasTimeout:
		steps = append(steps, Op{Kind: Delete, Path: append(slices.Clone(ap), "timeout")})
	case a.Timeout != "" && curTimeout != a.Timeout:
		steps = append(steps, put(ap, hasTimeout, "timeout", a.Timeout))
	}
	return steps, nil
}

// listSteps turns the list at path from old into want by the smallest change: it keeps the common
// start and end and replaces what is between them, so adding an approver at the end is one inserted
// item and removing one is one deleted item.
func listSteps(path []any, old, want []string) []Op {
	lo := 0
	for lo < len(old) && lo < len(want) && old[lo] == want[lo] {
		lo++
	}
	oe, we := len(old), len(want)
	for oe > lo && we > lo && old[oe-1] == want[we-1] {
		oe--
		we--
	}
	var steps []Op
	for i := oe - 1; i >= lo; i-- { // from the end, so the indexes of the rest stay valid
		steps = append(steps, Op{Kind: Delete, Path: append(slices.Clone(path), i)})
	}
	for i := lo; i < we; i++ {
		steps = append(steps, Op{Kind: Insert, Path: slices.Clone(path), Index: i, Value: want[i]})
	}
	return steps
}
