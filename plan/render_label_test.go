package plan

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// mermaidLine is every statement renderMermaid writes: the header, the start node, a node with a
// quoted label (box or diamond), and an edge with an optional label. A quoted label holds no '"',
// which would end it.
var mermaidLine = regexp.MustCompile(`^(flowchart TD|  start\(\[user\]\)|  n\d+\["([^"\n]*)"\]|  n\d+\{"([^"\n]*)"\}|  (start|n\d+) --> n\d+|  n\d+ -->\|[^|\n]*\| n\d+)$`)

// decodeEntities undoes Mermaid's numeric entity codes (#<decimal>;).
func decodeEntities(s string) string {
	return regexp.MustCompile(`#(\d+);`).ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.Atoi(m[1 : len(m)-1])
		return string(rune(n))
	})
}

// A node name is written into the diagram as data: whatever it holds (a quote, a bracket, an
// arrow, a Mermaid comment, a line break, an entity), it stays inside its own label, so a
// config cannot draw nodes or edges the flow does not have. The label reads back as the name.
func TestRenderMermaid_NodeNamesCannotAddStatements(t *testing.T) {
	for _, name := range []string{
		`x"] --> fake[ok] %%`,
		"x\"]\n  fake --> n0\n  n9[\"",
		`x #quot; y`,
		`x <b>bold</b> & y`,
		`x\" y`,
	} {
		b := New[int, int]("f")
		a := b.Step(name, func(_ context.Context, n int) (int, error) { return n, nil })
		c := b.Step("next", func(_ context.Context, n int) (int, error) { return n, nil })
		b.Edge(a, c)
		f, err := b.Build()
		if err != nil {
			t.Fatalf("%q: Build: %v", name, err)
		}
		got := f.RenderMermaid()
		var labels []string
		for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
			m := mermaidLine.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("%q: line %q is not a statement the renderer writes; output:\n%s", name, line, got)
				continue
			}
			if m[2] != "" {
				labels = append(labels, decodeEntities(m[2]))
			}
		}
		if want := name + " : int -> int"; len(labels) == 0 || labels[0] != want {
			t.Errorf("%q: first label %q; want %q", name, labels, want)
		}
	}
}
