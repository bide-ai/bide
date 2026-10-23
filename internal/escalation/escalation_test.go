package escalation

import "testing"

func TestSubject(t *testing.T) {
	if got := ID("c1"); got != "c1#taint" {
		t.Fatalf("ID = %q", got)
	}
	if b, ok := Base("c1#taint"); !ok || b != "c1" {
		t.Fatalf("Base(c1#taint) = %q %v", b, ok)
	}
	for _, s := range []string{"c1", "", "taint", "c1#taint2", "c1#"} {
		if IsSubject(s) {
			t.Errorf("IsSubject(%q)", s)
		}
	}
	if !IsSubject("#taint") || !IsSubject(ID("node:x")) {
		t.Error("an escalation subject was not recognised")
	}
}
