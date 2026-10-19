package flowedit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// FuzzApplyOperation: whatever the text and whichever operation the page could send for it,
// ApplyOperation either refuses or returns text that parses as the format it was in, and never
// panics. Every splice in it is verified by Apply; this checks the sequences and the planning
// around them (the list diffs, the keys that may or may not be there, the assertion list).
func FuzzApplyOperation(f *testing.F) {
	f.Add([]byte(opsFlow), uint16(0))
	f.Add([]byte(opsFlow+"assert:\n  - receipt reachable\n"), uint16(7))
	f.Add([]byte(`{"version":1,"nodes":[{"name":"a","block":"a","approval":{"need":1,"approvers":["x"]}}],"wiring":[],"assert":["a reachable"]}`), uint16(3))
	for i, g := range generated(10) {
		f.Add([]byte(g.text), uint16(i*13))
	}
	gate := &Approval{Need: 2, Approvers: []string{"x", "y", "z"}, ExcludePrincipal: true, Timeout: "1h"}
	f.Fuzz(func(t *testing.T, text []byte, pick uint16) {
		name := "f.yaml"
		if json.Valid(text) {
			name = "f.json"
		}
		data := text
		if name == "f.yaml" {
			var err error
			if data, err = ToJSON(text); err != nil {
				return
			}
		}
		var cfg struct {
			Nodes []struct {
				Name string `json:"name"`
			} `json:"nodes"`
		}
		if json.Unmarshal(data, &cfg) != nil || len(cfg.Nodes) == 0 {
			return
		}
		node := cfg.Nodes[int(pick)%len(cfg.Nodes)].Name
		other := cfg.Nodes[int(pick/3)%len(cfg.Nodes)].Name
		ops := []Operation{
			{Op: "set_safety", Node: node, Value: "readonly"},
			{Op: "set_safety", Node: node},
			{Op: "set_block", Node: node, Value: "b2"},
			{Op: "set_approval", Node: node, Approval: gate},
			{Op: "set_approval", Node: node, Approval: &Approval{Need: 1, Approvers: []string{"only"}}},
			{Op: "clear_approval", Node: node},
			{Op: "add_assertion", Assertion: &Assertion{Form: "reachable", Node: node}},
			{Op: "add_assertion", Assertion: &Assertion{Form: "passes", Node: node, Via: other}},
			{Op: "set_assertion", Index: 0, Assertion: &Assertion{Form: "no_side_effect", Node: node}},
			{Op: "remove_assertion", Index: 0},
			{Op: "remove_assertion", Index: 1},
			{Op: "add_node", Node: "zz new", Value: "zz"},
			{Op: "add_node", Node: other, Value: "dup"},
			{Op: "rename_node", Node: node, Value: "zz renamed"},
			{Op: "rename_node", Node: node, Value: other},
			{Op: "remove_node", Node: node, DryRun: true},
			{Op: "remove_node", Node: node},
			{Op: "set_entry", Value: node},
			{Op: "set_entry"},
			{Op: "move_node", Node: node, Value: "up"},
			{Op: "move_node", Node: node, Value: "down"},
			{Op: "move_wiring", Index: int(pick) % 4, Value: "up"},
			{Op: "move_wiring", Index: int(pick) % 4, Value: "down"},
			{Op: "set_flow", Value: "zz flow"},
			{Op: "set_in", Value: "T"},
			{Op: "set_out"},
		}
		op := ops[int(pick)%len(ops)]
		r, err := ApplyOperation(name, text, op)
		if err != nil {
			return
		}
		out := r.Text
		if name == "f.yaml" {
			if _, err := ToJSON(out); err != nil {
				t.Fatalf("%+v produced text that does not parse: %v\n%s", op, err, out)
			}
		} else if !json.Valid(out) {
			t.Fatalf("%+v produced invalid JSON:\n%s", op, out)
		}
		// Undo the same operation's effect on bytes it did not touch: the result is a splice, so a
		// no-op reapplication changes nothing more.
		if strings.HasPrefix(op.Op, "move_") {
			// A move is not idempotent; its inverse is the move the other way, which restores the bytes.
			inv := op
			inv.Value = map[string]string{"up": "down", "down": "up"}[op.Value]
			if op.Op == "move_wiring" {
				inv.Index += map[string]int{"up": -1, "down": 1}[op.Value]
			}
			if back, err := ApplyOperation(name, out, inv); err == nil && string(back.Text) != string(text) && r.Changed && bytes.HasSuffix(text, []byte("\n")) {
				t.Fatalf("%+v: moving back did not restore the bytes:\n%s\n=====\n%s", op, text, back.Text)
			}
			return
		}
		again, err := ApplyOperation(name, out, op)
		if err == nil && string(again.Text) != string(out) && op.Op != "add_assertion" && op.Op != "remove_assertion" && !isStructureOp(op.Op) {
			t.Fatalf("%+v is not idempotent:\n%s\n=====\n%s", op, out, again.Text)
		}
	})
}
