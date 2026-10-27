package escalation

import "testing"

func TestSubject(t *testing.T) {
	if got := ID("c1"); got != "c1#taint" {
		t.Fatalf("ID = %q", got)
	}
	if b, ok := Base("c1#taint"); !ok || b != "c1" {
		t.Fatalf("Base(c1#taint) = %q %v", b, ok)
	}
	for _, s := range []string{"c1", "", "taint", "c1#taint2", "c1#refund_total", "attempt:tool:c1#refund_total", "c1#", "node:foo#taint", "node:iter:2:foo#taint"} {
		if IsSubject(s) {
			t.Errorf("IsSubject(%q)", s)
		}
	}
	if b, ok := Base("node:foo#taint"); ok || b != "" {
		t.Errorf("Base(node:foo#taint) = %q %v, want a node, not an instance", b, ok)
	}
	if !IsSubject("#taint") || !IsSubject(ID("call_1")) {
		t.Error("an escalation subject was not recognised")
	}
}

// A plan flow node's escalation instance is the node's key and ":taint". A node name holds no ':', so no
// node can have that name, and the form is read by the one parser, not by a suffix test.
func TestNodeInstance(t *testing.T) {
	for node, want := range map[string]string{
		"node:x":           "node:x:taint",
		"node:iter:2:x":    "node:iter:2:x:taint",
		"node:iter":        "node:iter:taint", // a node named iter
		"node:iter:3:iter": "node:iter:3:iter:taint",
	} {
		if got := ID(node); got != want {
			t.Errorf("ID(%q) = %q, want %q", node, got, want)
		}
		if b, ok := Base(want); !ok || b != node {
			t.Errorf("Base(%q) = %q %v, want %q", want, b, ok, node)
		}
		if !IsSubject(want) || IsSubject(node) {
			t.Errorf("IsSubject: instance %q = %v, node %q = %v", want, IsSubject(want), node, IsSubject(node))
		}
	}
	// What is not an instance: a node whose name is "taint", a nested Step named taint, a second suffix,
	// a key with no node, and a ':taint' on something that is not a plan key.
	for _, s := range []string{
		"node:taint",         // the node named taint
		"node:iter:3:taint",  // the node named taint, iteration 3
		"node:x:step:taint",  // a Step named taint, run by node x
		"node:x:taint:taint", // the suffix twice: not a node key before it
		"node::taint",        // no node name
		"node:iter:3:x:step:y:taint",
		"x:taint", // not a plan key
		":taint",
	} {
		if b, ok := Base(s); ok {
			t.Errorf("Base(%q) = %q, want not an instance", s, b)
		}
		if IsSubject(s) {
			t.Errorf("IsSubject(%q)", s)
		}
	}
	// node:iter:taint is the instance of the node named iter (iter: followed by a non-number is the node
	// itself), and node:iter:3:x:taint the instance of x in iteration 3.
	if b, ok := Base("node:iter:taint"); !ok || b != "node:iter" {
		t.Errorf("Base(node:iter:taint) = %q %v", b, ok)
	}
	if b, ok := Base("node:iter:3:x:taint"); !ok || b != "node:iter:3:x" {
		t.Errorf("Base(node:iter:3:x:taint) = %q %v", b, ok)
	}
}

// PlanKey is the one parser of the keys package plan runs its nodes under: a node, or a Step a node's body runs.
func TestPlanKey(t *testing.T) {
	type k struct{ node, step string }
	good := map[string]k{
		"node:x":                   {"node:x", ""},
		"node:iter:4:x":            {"node:iter:4:x", ""},
		"node:iter:0:x":            {"node:iter:0:x", ""},
		"node:iter":                {"node:iter", ""}, // iter: needs a number after it
		"node:x:step:fetch":        {"node:x", "fetch"},
		"node:iter:2:x:step:fetch": {"node:iter:2:x", "fetch"},
		"node:taint":               {"node:taint", ""},
		"node:x:step:taint":        {"node:x", "taint"},
	}
	for key, want := range good {
		node, step, ok := PlanKey(key)
		if !ok || node != want.node || step != want.step {
			t.Errorf("PlanKey(%q) = %q, %q, %v; want %q, %q", key, node, step, ok, want.node, want.step)
		}
	}
	for _, bad := range []string{"", "node:", "x", "node:x:", "node:x:y", "node:x:step:", "node::x", "node:iter:x:y", "node:iter:02:x", "node:iter:-1:x", "node:x:taint"} {
		if node, step, ok := PlanKey(bad); ok {
			t.Errorf("PlanKey(%q) = %q, %q, true; want not a plan key", bad, node, step)
		}
	}
}
