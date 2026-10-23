package escalation

import "testing"

func TestSubject(t *testing.T) {
	if got := ID("c1"); got != "c1#taint" {
		t.Fatalf("ID = %q", got)
	}
	if b, ok := Base("c1#taint"); !ok || b != "c1" {
		t.Fatalf("Base(c1#taint) = %q %v", b, ok)
	}
	for _, s := range []string{"c1", "", "taint", "c1#taint2", "c1#", "node:foo#taint", "node:iter:2:foo#taint"} {
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
