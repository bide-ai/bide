package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

// lastToolResultModel records the tool results each request carries, then delegates to a script.
type lastToolResultModel struct {
	script *ScriptedModel
	mu     sync.Mutex
	seen   []json.RawMessage
}

func (m *lastToolResultModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	m.mu.Lock()
	for _, msg := range req.Messages {
		for _, p := range msg.Parts {
			if tr, ok := p.(ToolResult); ok {
				m.seen = append(m.seen, tr.Result)
			}
		}
	}
	m.mu.Unlock()
	return m.script.Stream(ctx, req)
}

func (m *lastToolResultModel) results() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.seen))
	for i, r := range m.seen {
		out[i] = string(r)
	}
	return out
}

// JSON the library writes on a tool's behalf reaches the model as the text it encodes, without
// HTML escaping: an adapter forwards a tool result's JSON text to the model, and json.Marshal's
// default escapes would turn "<b>" into escape sequences the model then reads literally.
func TestModelVisibleJSON_NotHTMLEscaped(t *testing.T) {
	ctx := context.Background()
	script := func() *ScriptedModel { return NewScriptedModel(ToolTurn("c1", "t", `{}`), TextTurn("done")) }

	t.Run("typed tool result", func(t *testing.T) {
		type out struct {
			HTML string `json:"html"`
		}
		m := &lastToolResultModel{script: script()}
		tool := Func("t", "", Safety{ReadOnly: true}, func(context.Context, struct{}) (out, error) {
			return out{HTML: "<b>a & b</b>"}, nil
		})
		if _, err := mustNew(m, memJournal(), WithTools(tool)).Run(ctx, "r", UserText("go")); err != nil {
			t.Fatal(err)
		}
		if got := m.results(); len(got) != 1 || got[0] != `{"html":"<b>a & b</b>"}` {
			t.Fatalf("model read tool results %q", got)
		}
	})

	t.Run("tool error text", func(t *testing.T) {
		m := &lastToolResultModel{script: script()}
		tool := Func("t", "", Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) {
			return "", errors.New("bad <input> & more")
		})
		if _, err := mustNew(m, memJournal(), WithTools(tool)).Run(ctx, "r", UserText("go")); err != nil {
			t.Fatal(err)
		}
		if got := m.results(); len(got) != 1 || got[0] != `"bad <input> & more"` {
			t.Fatalf("model read tool results %q", got)
		}
	})

	t.Run("sub-agent answer", func(t *testing.T) {
		m := &lastToolResultModel{script: NewScriptedModel(ToolTurn("c1", "t", `{"task":"go"}`), TextTurn("done"))}
		child := mustNew(NewScriptedModel(TextTurn("<ok> & done")), memJournal())
		if _, err := mustNew(m, memJournal(), WithTools(SubAgent("t", "", child))).Run(ctx, "r", UserText("go")); err != nil {
			t.Fatal(err)
		}
		if got := m.results(); len(got) != 1 || got[0] != `"<ok> & done"` {
			t.Fatalf("model read tool results %q", got)
		}
	})

	t.Run("resolved halt", func(t *testing.T) {
		store := memJournal()
		m := &lastToolResultModel{script: script()}
		tool := Func("t", "", Safety{}, func(context.Context, struct{}) (string, error) { return "", nil })
		if _, err := store.do(ctx, "r", toolAttemptStep("c1"), func(context.Context) (Record, error) {
			return Record{Kind: StepAttempt, ToolUseID: "c1"}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.do(ctx, "r", "@llm/0", func(context.Context) (Record, error) {
			msg := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "t", Args: json.RawMessage(`{}`)}}}
			return Record{Kind: StepModel, Message: &msg}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := ResolveHalt(ctx, store, "r", "c1", "charged <id=7> & sent", false); err != nil {
			t.Fatal(err)
		}
		if _, err := mustNew(m, store, WithTools(tool)).Run(ctx, "r", UserText("go")); err != nil {
			t.Fatal(err)
		}
		if got := m.results(); len(got) != 1 || got[0] != `"charged <id=7> & sent"` {
			t.Fatalf("model read tool results %q", got)
		}
	})
}
