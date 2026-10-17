package runview

import (
	"strings"
	"testing"

	"github.com/bide-ai/bide/plan"
)

// A count of one reads in the singular and any other count in the plural, in the verdict banner, its
// key numbers and the other strings the pages build around a count.
func TestCountsAgreeWithTheirNouns(t *testing.T) {
	assertionLine := func(n int) string {
		hold := assertions(true)
		var as []plan.AssertionResult
		for i := 0; i < n; i++ {
			as = append(as, hold[0])
		}
		return configVerdict(&ConfigView{Assertions: as}).Line
	}
	for _, c := range []struct {
		n    int
		want string
	}{
		{1, "1 assertion holds on every path the flow declares."},
		{2, "2 assertions hold on every path the flow declares."},
	} {
		if got := assertionLine(c.n); got != c.want {
			t.Errorf("%d held: %q, want %q", c.n, got, c.want)
		}
	}

	labels := func(v ConfigView) map[string]string {
		m := map[string]string{}
		for _, s := range configVerdict(&v).Stats {
			m[s.Label] = s.Value
		}
		return m
	}
	one := ConfigView{
		Assertions: assertions(true)[:1],
		Graph:      &Graph{Nodes: []GraphNode{{Name: "a"}}},
		Approvals:  []string{"x"},
	}
	one.Risk.SideEffects = 1
	for _, want := range []string{"node", "side-effect step", "assertion holds", "approval gate"} {
		if _, ok := labels(one)[want]; !ok {
			t.Errorf("one of each: no stat labelled %q in %v", want, labels(one))
		}
	}
	many := ConfigView{
		Assertions: append(assertions(true), assertions(true)...),
		Graph:      &Graph{Nodes: []GraphNode{{Name: "a"}, {Name: "b"}}},
		Approvals:  []string{"x", "y"},
	}
	many.Risk.SideEffects = 2
	for _, want := range []string{"nodes", "side-effect steps", "assertions hold", "approval gates"} {
		if _, ok := labels(many)[want]; !ok {
			t.Errorf("two of each: no stat labelled %q in %v", want, labels(many))
		}
	}

	for _, c := range []struct {
		name string
		got  string
		want string
	}{
		{"graph one", svg(&Graph{Nodes: []GraphNode{{Name: "a"}}, Edges: []GraphEdge{{From: "a", To: "a"}}}), `aria-label="flow graph: 1 node, 1 edge"`},
		{"graph two", svg(&Graph{Nodes: []GraphNode{{Name: "a"}, {Name: "b"}}}), `aria-label="flow graph: 2 nodes, 0 edges"`},
		{"one record", func() string { p := &Proof{Covers: 1}; finish(p, 1); return p.Summary }(), "verifies: the 1 record is under the signed head"},
		{"all records", func() string { p := &Proof{Covers: 3}; finish(p, 3); return p.Summary }(), "verifies: all 3 records are under the signed head"},
		{"one uncovered", func() string { p := &Proof{Covers: 2}; finish(p, 3); return p.Summary }(), "the 1 after it is not yet covered"},
		{"two uncovered", func() string { p := &Proof{Covers: 1}; finish(p, 3); return p.Summary }(), "the 2 after it are not yet covered"},
		{"one approver", gateStatus(Gate{Need: 1, Approvers: []string{"a"}}), "0 of 1 approver; 1 needed"},
		{"two approvers", gateStatus(Gate{Need: 1, Approvers: []string{"a", "b"}}), "0 of 2 approvers; 1 needed"},
	} {
		if !strings.Contains(c.got, c.want) {
			t.Errorf("%s: %q does not contain %q", c.name, c.got, c.want)
		}
	}
}
