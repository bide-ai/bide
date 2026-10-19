package flowedit

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	vBlock = "  # reads the request\n  - {name: validate, block: validate}\n"
	cBlock = "  # the gated one\n  - name: charge       # gated\n    block: charge\n    approval:\n      need: 2\n      approvers: [ops, finance, risk]\n"
)

// Each item keeps its own comments, its trailing comment and its style; the blank line between the
// items stays between them.
func TestMoveNodeKeepsEachItemsText(t *testing.T) {
	if !strings.Contains(structFlow, vBlock+"\n"+cBlock) {
		t.Fatal("the sample is not what the test expects")
	}
	got := mustApply(t, "f.yaml", structFlow, Operation{Op: "move_node", Node: "charge", Value: "up"})
	if want := strings.Replace(structFlow, vBlock+"\n"+cBlock, cBlock+"\n"+vBlock, 1); got != want {
		t.Errorf("up:\n%s", got)
	}
	back := mustApply(t, "f.yaml", got, Operation{Op: "move_node", Node: "charge", Value: "down"})
	if back != structFlow {
		t.Errorf("down did not undo up:\n%s", back)
	}
	// down from the validate node is the same exchange
	if again := mustApply(t, "f.yaml", structFlow, Operation{Op: "move_node", Node: "validate", Value: "down"}); again != got {
		t.Errorf("validate down differs from charge up")
	}
	// the last two: a node after a node, no blank between, the last without a comment
	got = mustApply(t, "f.yaml", structFlow, Operation{Op: "move_node", Node: "manual review", Value: "up"})
	want := strings.Replace(structFlow, "  - {name: receipt, block: receipt}\n  - {name: manual review, block: review}\n", "  - {name: manual review, block: review}\n  - {name: receipt, block: receipt}\n", 1)
	if got != want {
		t.Errorf("last up:\n%s", got)
	}
}

func TestMoveWiringCarriesItsComment(t *testing.T) {
	got := mustApply(t, "f.yaml", structFlow, Operation{Op: "move_wiring", Index: 1, Value: "down"})
	sw := "  # routes by outcome\n  - switch: charge\n    when:\n      - {pred: ok, to: receipt}\n      - {pred: odd, to: manual review}\n    else: receipt\n"
	edge := "  - edge: [receipt, manual review]\n"
	if !strings.Contains(structFlow, sw+edge) {
		t.Fatal("the sample is not what the test expects")
	}
	if want := strings.Replace(structFlow, sw+edge, edge+sw, 1); got != want {
		t.Errorf("\n%s", got)
	}
}

func TestMoveInFlowAndJSONLists(t *testing.T) {
	flow := "version: 1\nflow: f\nnodes: [{name: a, block: a}, {name: b, block: b}, {name: c, block: c}]\nwiring: [{edge: [a, b]}, {edge: [b, c]}]\nentry: a\n"
	got := mustApply(t, "f.yaml", flow, Operation{Op: "move_node", Node: "c", Value: "up"})
	if want := strings.Replace(flow, "{name: b, block: b}, {name: c, block: c}", "{name: c, block: c}, {name: b, block: b}", 1); got != want {
		t.Errorf("flow:\n%s", got)
	}
	got = mustApply(t, "f.yaml", flow, Operation{Op: "move_wiring", Index: 0, Value: "down"})
	if want := strings.Replace(flow, "{edge: [a, b]}, {edge: [b, c]}", "{edge: [b, c]}, {edge: [a, b]}", 1); got != want {
		t.Errorf("flow wiring:\n%s", got)
	}
	js := "{\n  \"version\": 1,\n  \"flow\": \"f\",\n  \"entry\": \"a\",\n  \"nodes\": [\n    {\"name\": \"a\", \"block\": \"a\"},\n    {\n      \"name\": \"b\",\n      \"block\": \"b\"\n    }\n  ],\n  \"wiring\": []\n}\n"
	got = mustApply(t, "f.json", js, Operation{Op: "move_node", Node: "b", Value: "up"})
	if !json.Valid([]byte(got)) || strings.Index(got, `"name": "b"`) > strings.Index(got, `"name": "a"`) {
		t.Errorf("json:\n%s", got)
	}
	if back := mustApply(t, "f.json", got, Operation{Op: "move_node", Node: "b", Value: "down"}); back != js {
		t.Errorf("json down did not undo up:\n%s", back)
	}
}

func TestMoveRefusals(t *testing.T) {
	noEntry := strings.Replace(structFlow, "entry: validate\n", "", 1)
	for name, c := range map[string]struct {
		src string
		op  Operation
		why string
	}{
		"the first node up":                {structFlow, Operation{Op: "move_node", Node: "validate", Value: "up"}, "already the first"},
		"the last node down":               {structFlow, Operation{Op: "move_node", Node: "manual review", Value: "down"}, "already the last"},
		"an unknown node":                  {structFlow, Operation{Op: "move_node", Node: "nope", Value: "up"}, "no node is named"},
		"a direction that is not":          {structFlow, Operation{Op: "move_node", Node: "charge", Value: "left"}, "want up or down"},
		"into the first place, implicit":   {noEntry, Operation{Op: "move_node", Node: "charge", Value: "up"}, "set_entry"},
		"out of the first place, implicit": {noEntry, Operation{Op: "move_node", Node: "validate", Value: "down"}, "set_entry"},
		"wiring past the end":              {structFlow, Operation{Op: "move_wiring", Index: 2, Value: "down"}, "already the last"},
		"wiring before the start":          {structFlow, Operation{Op: "move_wiring", Index: 0, Value: "up"}, "already the first"},
		"wiring that is not there":         {structFlow, Operation{Op: "move_wiring", Index: 9, Value: "up"}, "no wiring element"},
		"an anchored item":                 {strings.Replace(structFlow, "  - {name: receipt,", "  - &r {name: receipt,", 1), Operation{Op: "move_node", Node: "receipt", Value: "up"}, ""},
	} {
		why := refusalOf(t, "f.yaml", c.src, c.op)
		if c.why != "" && !strings.Contains(why, c.why) {
			t.Errorf("%s: %q, want %q", name, why, c.why)
		}
	}
	// With an explicit entry, a move into the first place is fine, and moves the nodes only.
	mustApply(t, "f.yaml", structFlow, Operation{Op: "move_node", Node: "charge", Value: "up"})
	// An implicit entry is fine to move around away from the first place.
	mustApply(t, "f.yaml", noEntry, Operation{Op: "move_node", Node: "receipt", Value: "up"})
}

func TestSetFlowInOut(t *testing.T) {
	got := mustApply(t, "f.yaml", structFlow, Operation{Op: "set_flow", Value: "refund v2"})
	if want := strings.Replace(structFlow, "flow: refund\n", "flow: refund v2\n", 1); got != want {
		t.Errorf("flow:\n%s", got)
	}
	got = mustApply(t, "f.yaml", structFlow, Operation{Op: "set_in", Value: "Order"})
	if !strings.Contains(got, "in: Order\n") {
		t.Errorf("in added:\n%s", got)
	}
	got2 := mustApply(t, "f.yaml", got, Operation{Op: "set_in", Value: "Order2"})
	if !strings.Contains(got2, "in: Order2\n") || strings.Contains(got2, "in: Order\n") {
		t.Errorf("in changed:\n%s", got2)
	}
	if cleared := mustApply(t, "f.yaml", got2, Operation{Op: "set_in"}); strings.Contains(cleared, "in:") {
		t.Errorf("in cleared:\n%s", cleared)
	}
	if same := mustApply(t, "f.yaml", got, Operation{Op: "set_in", Value: "Order"}); same != got {
		t.Error("setting in to itself changed the text")
	}
	if same := mustApply(t, "f.yaml", structFlow, Operation{Op: "set_out"}); same != structFlow {
		t.Error("clearing an out that is not there changed the text")
	}
	for name, op := range map[string]Operation{
		"an empty flow name":      {Op: "set_flow"},
		"a flow with an override": {Op: "set_flow", Value: "a\u202eb"},
		"in with a line break":    {Op: "set_in", Value: "a\nb"},
		"out with padding":        {Op: "set_out", Value: " x"},
	} {
		refusalOf(t, "f.yaml", structFlow, op)
		_ = name
	}
}

// Hostile outputs of an exchange are refused: a comment left behind, a comment moved to the wrong
// item, an item changed, a line elsewhere changed.
func TestVerifySwapRefusesWrongResults(t *testing.T) {
	op := Op{Kind: Swap, Path: []any{"nodes"}, Index: 0}
	good := strings.Replace(structFlow, vBlock+"\n"+cBlock, cBlock+"\n"+vBlock, 1)
	if err := Verify([]byte(structFlow), []byte(good), op); err != nil {
		t.Fatalf("the right result was refused: %v", err)
	}
	for name, bad := range map[string]string{
		"the comments stayed behind":      strings.Replace(structFlow, vBlock+"\n"+cBlock, "  # reads the request\n  # the gated one\n"+"  - name: charge       # gated\n    block: charge\n    approval:\n      need: 2\n      approvers: [ops, finance, risk]\n\n  - {name: validate, block: validate}\n", 1),
		"the comments swapped owners":     strings.Replace(good, "  # the gated one\n  - name: charge", "  # reads the request\n  - name: charge", 1),
		"an item changed":                 strings.Replace(good, "need: 2", "need: 3", 1),
		"a line elsewhere changed":        strings.Replace(good, "flow: refund", "flow: refund2", 1),
		"the items not exchanged":         structFlow,
		"a trailing comment dropped":      strings.Replace(good, "# gated", "# fated", 1),
		"the blank line moved to the top": strings.Replace(good, "# the refund flow\n", "# the refund flow\n\n", 1),
	} {
		if err := Verify([]byte(structFlow), []byte(bad), op); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Over the corpus: every exchange is a refusal or a verified result, and moving back restores the
// exact bytes.
func TestMoveOverCorpus(t *testing.T) {
	files := append(collect(t), generated(150)...)
	applied, refused, restored := 0, 0, 0
	reasons := map[string]int{}
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
		if json.Unmarshal(data, &cfg) != nil {
			continue
		}
		try := func(op, undo Operation) {
			r, err := ApplyOperation(name, []byte(f.text), op)
			var rf *Refusal
			if errors.As(err, &rf) {
				refused++
				r := rf.Reason
				if i := strings.IndexAny(r, ":\"("); i > 0 {
					r = r[:i]
				}
				reasons[r]++
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
				if strings.HasSuffix(f.text, "\n") {
					t.Errorf("%s %+v: moving back did not restore the bytes\n--- before\n%s\n--- after\n%s", f.name, op, f.text, back.Text)
				}
				return
			}
			restored++
		}
		for _, n := range cfg.Nodes {
			try(Operation{Op: "move_node", Node: n.Name, Value: "down"}, Operation{Op: "move_node", Node: n.Name, Value: "up"})
		}
		for i := range cfg.Wiring {
			try(Operation{Op: "move_wiring", Index: i, Value: "down"}, Operation{Op: "move_wiring", Index: i + 1, Value: "up"})
		}
	}
	t.Logf("refusals: %v", reasons)
	t.Logf("%d applied, %d refused, %d restored the bytes", applied, refused, restored)
	if applied < 100 || restored < applied/2 {
		t.Errorf("the corpus exercised too little: %d applied, %d restored", applied, restored)
	}
}

// Each of Verify's checks of an exchange has a hostile output only it refuses.
func TestVerifySwapEachCheck(t *testing.T) {
	old := "nodes:\n  - name: a\n    k: 1\n    z: 9\n  - name: b\n    k: 2\n    z: 8\n"
	op := Op{Kind: Swap, Path: []any{"nodes"}, Index: 0}
	if err := Verify([]byte(old), []byte("nodes:\n  - name: b\n    k: 2\n    z: 8\n  - name: a\n    k: 1\n    z: 9\n"), op); err != nil {
		t.Fatalf("the right result: %v", err)
	}
	for name, bad := range map[string]string{
		// lines, first and last lines, comments and the lines around all as a swap leaves them; the data is not
		"the data: a value moved between the items": "nodes:\n  - name: b\n    k: 1\n    z: 8\n  - name: a\n    k: 2\n    z: 9\n",
		// the data and the first and last lines are right; a line inside an item is not a line of the old text
		"the lines: a middle line rewritten": "nodes:\n  - name: b\n    k:  2\n    z: 8\n  - name: a\n    k: 1\n    z: 9\n",
	} {
		if err := Verify([]byte(old), []byte(bad), op); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Comment lines elsewhere that trade places are a permutation of the old lines, with the data and the
// two items as they should be: only the check of the lines before and after the pair refuses them.
func TestVerifySwapLinesAroundThePair(t *testing.T) {
	old := "# one\n# two\n\nnodes:\n  - name: a\n    k: 1\n  - name: b\n    k: 2\n\n# three\n\n# four\n"
	op := Op{Kind: Swap, Path: []any{"nodes"}, Index: 0}
	good := "# one\n# two\n\nnodes:\n  - name: b\n    k: 2\n  - name: a\n    k: 1\n\n# three\n\n# four\n"
	if err := Verify([]byte(old), []byte(good), op); err != nil {
		t.Fatalf("the right result: %v", err)
	}
	for name, bad := range map[string]string{
		"comments before the pair traded": "# two\n# one\n\nnodes:\n  - name: b\n    k: 2\n  - name: a\n    k: 1\n\n# three\n\n# four\n",
		"comments after the pair traded":  "# one\n# two\n\nnodes:\n  - name: b\n    k: 2\n  - name: a\n    k: 1\n\n# four\n\n# three\n",
	} {
		if err := Verify([]byte(old), []byte(bad), op); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
