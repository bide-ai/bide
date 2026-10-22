package flowedit

import "testing"

// An assert line the grammar cannot read mentions a node when the node's name stands in it as a whole span,
// with a rune that is not a letter, digit or mark (or the end of the line) on each side. Punctuation,
// symbols and spaces of any kind all end a word, so no shape of punctuation around the name hides it.
func TestUnreadableAssertionMentions(t *testing.T) {
	const nbsp = " "
	for _, c := range []struct {
		line, name string
		want       bool
	}{
		// each shape reported as inconsistent
		{"[audit]", "audit", true},
		{"(audit)", "audit", true},
		{"audit" + nbsp + "x", "audit", true},
		{"audit-", "audit", true},
		{"audit/x", "audit", true},
		{"audit=1", "audit", true},
		{"audit’s", "audit", true},
		{"*audit*", "audit", true},
		{"_audit_", "audit", true},
		{"audit|", "audit", true},
		{"audit…", "audit", true},
		{"«audit»", "audit", true},
		{"audit\\", "audit", true},
		// the ones that were already found
		{"something odd: audit, really", "audit", true},
		{"this is about the audit trail", "audit", true},
		{`see "audit" for the detail`, "audit", true},
		{"audit", "audit", true},
		{"about audit", "audit", true},
		// words that only contain the name are other words
		{"the audits", "audit", false},
		{"the audits and the audit", "audit", true}, // a later occurrence counts
		{"audits audit2 unaudit", "audit", false},
		{"unaudited", "audit", false},
		{"audit2", "audit", false},
		{"éaudit", "audit", false},
		{"audit́", "audit", false}, // a combining mark continues the word
		{"something odd about charge", "audit", false},
		{"", "audit", false},
		{"audit", "", false},
		// names with spaces and punctuation are matched as a whole span
		{`no side effect after "manual review"`, "manual review", true},
		{"after manual review, then", "manual review", true},
		{"manual", "manual review", false},
		{"manual  review", "manual review", false},
		{"check (a.b) twice", "a.b", true},
		{"check a.b.c twice", "a.b", true}, // punctuation ends a word, so this is refused rather than guessed at
		{"check a.b. twice", "a.b", true},
		{"try x-y!", "x-y", true},
		{"try x-yz", "x-y", false},
		{"[charge:v2]", "charge:v2", true},
		{"«café bar»", "café bar", true},
		{"cafés", "café", false},
	} {
		if got := lineMentions(c.line, c.name); got != c.want {
			t.Errorf("lineMentions(%q, %q) = %v, want %v", c.line, c.name, got, c.want)
		}
	}
}

// The readable forms are still read by the grammar, not by words: a line that parses names exactly the
// nodes it holds.
func TestAssertionMentionsReadableLinesByTheirNodes(t *testing.T) {
	for _, c := range []struct {
		line, name string
		want       bool
	}{
		{"audit reachable", "audit", true},
		{"audit reachable", "reachable", false},
		{"every path to receipt passes audit", "audit", true},
		{"every path to receipt passes audit", "receipt", true},
		{"every path to receipt passes audit", "path", false},
		{`"manual review" reachable`, "manual review", true},
	} {
		if got := assertionMentions(c.line, c.name); got != c.want {
			t.Errorf("assertionMentions(%q, %q) = %v, want %v", c.line, c.name, got, c.want)
		}
	}
}
