package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/bide-ai/bide/internal/mermaid"
)

// RenderMermaid returns a Mermaid flowchart of a run's journaled steps — the "graph as
// derived OUTPUT" (Option B): you write plain Go, and the graph is rendered from what
// actually ran, not hand-authored. Feed it to the dev UI, a trace viewer, or a PR.
func RenderMermaid(ctx context.Context, d Durable, runID string) (string, error) {
	recs, err := d.History(ctx, runID)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("flowchart TD\n")
	b.WriteString("  start([user])\n")

	prev := "start"
	i := 0
	for _, r := range recs {
		var label string
		switch r.Kind {
		case StepModel:
			label = "LLM"
		case StepToolResult:
			name, _ := toolNameFor(recs, r.ToolUseID)
			if name == "" {
				name = r.ToolUseID
			}
			label = "tool: " + name
			if r.IsError {
				label += " ✗"
			}
		case StepApproval:
			if r.Approved {
				label = "approved ✓"
			} else {
				label = "denied ✗"
			}
		case StepValue:
			label = "step: " + r.Name
		case StepAttempt:
			continue // internal side-effect-safety marker; not part of the visual flow
		default:
			continue
		}
		id := fmt.Sprintf("n%d", i)
		b.WriteString(fmt.Sprintf("  %s[%s]\n", id, mermaid.Label(label)))
		b.WriteString(fmt.Sprintf("  %s --> %s\n", prev, id))
		prev = id
		i++
	}
	b.WriteString("  " + prev + " --> done([done])\n")
	return b.String(), nil
}
