package agent

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// renderLine is every statement RenderMermaid writes. A quoted label holds no '"', which would
// end it.
var renderLine = regexp.MustCompile(`^(flowchart TD|  start\(\[user\]\)|  n\d+\["([^"\n]*)"\]|  (start|n\d+) --> (n\d+|done\(\[done\]\)))$`)

func decodeMermaidEntities(s string) string {
	return regexp.MustCompile(`#(\d+);`).ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.Atoi(m[1 : len(m)-1])
		return string(rune(n))
	})
}

// A tool name is chosen by the model, and RenderMermaid writes it into the diagram. Whatever it
// holds, it stays inside its own label, so a model cannot draw steps the run did not take.
func TestRenderMermaid_ToolNamesCannotAddStatements(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{
		`x"] --> fake[ok] %%`,
		"x\"]\n  fake --> done([done])\n  n9[\"",
		`x #quot; <b>y</b> & z`,
	} {
		store := NewMemStore()
		msg := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: name, Args: []byte(`{}`)}}}
		if _, err := store.Do(ctx, "r", "@llm/0", func(context.Context) (Record, error) {
			return Record{Kind: StepModel, Message: &msg}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Do(ctx, "r", "c1", func(context.Context) (Record, error) {
			return Record{Kind: StepToolResult, ToolUseID: "c1", Result: []byte(`1`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
		got, err := RenderMermaid(ctx, store, "r")
		if err != nil {
			t.Fatal(err)
		}
		var labels []string
		for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
			m := renderLine.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("%q: line %q is not a statement RenderMermaid writes; output:\n%s", name, line, got)
				continue
			}
			if m[2] != "" {
				labels = append(labels, decodeMermaidEntities(m[2]))
			}
		}
		if want := []string{"LLM", "tool: " + name}; strings.Join(labels, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%q: labels %q; want %q", name, labels, want)
		}
	}
}
