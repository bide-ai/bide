package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// requestModel answers with a final text and keeps the tool results of the request it was sent.
type requestModel struct{ results []string }

func (m *requestModel) Stream(_ context.Context, req Request) (*Stream, error) {
	for _, msg := range req.Messages {
		for _, p := range msg.Parts {
			if tr, ok := p.(ToolResult); ok {
				m.results = append(m.results, tr.ToolUseID+"="+string(tr.Result))
			}
		}
	}
	ch := make(chan Emit, 2)
	ch <- Emit{Event: TextDelta{Text: "done"}}
	ch <- Emit{Event: Finish{Reason: "stop"}}
	close(ch)
	return NewStream(ch), nil
}

func journal(t *testing.T, store *Journal, runID string, recs ...Record) {
	t.Helper()
	for _, r := range recs {
		if _, err := store.do(context.Background(), runID, r.Name, func(context.Context) (Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
}

func assistantCalls(ids ...string) *Message {
	m := &Message{Role: RoleAssistant}
	for _, id := range ids {
		m.Parts = append(m.Parts, ToolUse{ID: id, Name: "lookup", Args: json.RawMessage(`{}`)})
	}
	return m
}

// A resumed run rebuilds the conversation in call order, whatever order the journal recorded the
// results in, and places each result once, after the first turn that made the call (a journal
// written before tool-use IDs were checked may reuse one). A result no call made is left out.
func TestResume_ConversationFromJournal(t *testing.T) {
	store := memJournal()
	journal(t, store, "r",
		Record{Name: "@llm/0", Kind: StepModel, Message: assistantCalls("c1", "c2")},
		Record{Name: "c2", Kind: StepToolResult, ToolUseID: "c2", Result: json.RawMessage(`2`)},
		Record{Name: "c1", Kind: StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`1`)},
		Record{Name: "stray", Kind: StepToolResult, ToolUseID: "stray", Result: json.RawMessage(`0`)},
		Record{Name: "@llm/1", Kind: StepModel, Message: assistantCalls("c1")},
	)
	var calls int
	m := &requestModel{}
	if _, err := mustNew(
		m,
		store,
		WithTools(&countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}),
	).Run(context.Background(), "r", "go"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"c1=1", "c2=2"}; !reflect.DeepEqual(m.results, want) || calls != 0 {
		t.Fatalf("the model was shown results %v and the tool ran %d times; want %v and 0", m.results, calls, want)
	}
}
