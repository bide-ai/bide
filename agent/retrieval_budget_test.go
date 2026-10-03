package agent_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// staticRetriever returns fixed documents.
type staticRetriever []agent.Doc

func (r staticRetriever) Retrieve(context.Context, string, int) ([]agent.Doc, error) { return r, nil }

// WithRetrieval's step costs one step per run (its probes and its write), however many model calls the run
// makes: a drive reads the documents once, at its first model call, and its later calls reuse them.
func TestWithRetrieval_OneStepPerDrive(t *testing.T) {
	j, cs, _ := countingJournal(t)
	noop := agent.Func("noop", "", agent.Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { return "ok", nil })
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "noop", `{}`), agent.ToolTurn("c2", "noop", `{}`), agent.TextTurn("done"))
	a, err := agent.New(m, j, agent.WithTools(noop), agent.WithRetrieval(staticRetriever{{Text: "doc"}}, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r", agent.UserText("q")); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range cs.Counts().Names {
		if strings.HasSuffix(n, "@retrieval/0") {
			got = append(got, n)
		}
	}
	if want := []string{"get @retrieval/0", "get attempt:step:@retrieval/0", "insert @retrieval/0"}; !slices.Equal(got, want) {
		t.Errorf("retrieval round trips over three model calls = %v, want %v", got, want)
	}
}
