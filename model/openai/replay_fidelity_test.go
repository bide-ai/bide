package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// htmlTool echoes a result whose JSON carries HTML-significant characters and extra whitespace,
// the bytes a journal encoder is most tempted to rewrite.
type htmlTool struct{}

func (htmlTool) Name() string { return "html" }

// Spec describes the tool to the agent (see agent.Tool).
func (t htmlTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: t.Name(), Description: t.Description(), Input: t.ArgsSchema(), Safety: t.Safety()}
}

func (htmlTool) Description() string         { return "returns markup" }
func (htmlTool) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (htmlTool) Safety() agent.Safety        { return agent.Safety{ReadOnly: true} }
func (htmlTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{ "html" : "<b>a & b</b>",  "cmp": "x > y" }`), nil
}

// wireModel records the exact bytes this adapter would send for every model call, then delegates
// to a script. failOn makes the given call (1-based) fail, standing in for a crash before the
// turn is journaled.
type wireModel struct {
	m      *Model
	script *agenttest.ScriptedModel
	failOn int

	mu    sync.Mutex
	calls int
	wire  [][]byte
}

func (w *wireModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	b, err := w.m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.calls++
	n := w.calls
	w.wire = append(w.wire, b)
	w.mu.Unlock()
	if n == w.failOn {
		return nil, errors.New("crash before the turn is journaled")
	}
	return w.script.Stream(ctx, req)
}

// A resumed run must put the same bytes on the wire as the live run did at the same turn. The
// tool's arguments and result carry <, >, & and extra whitespace; if the journal rewrote them
// (HTML escaping, say) the model would read different text after a crash than it would have
// read without one.
func TestResumedTurnSendsLiveBytes(t *testing.T) {
	ctx := context.Background()
	script := func() *agenttest.ScriptedModel {
		return agenttest.NewScriptedModel(
			agenttest.ToolTurn("c1", "html", `{ "q" : "a<b && c>d" ,  "n": 1.50 }`),
			agenttest.TextTurn("done"),
		)
	}

	live := &wireModel{m: New("k"), script: script()}
	if _, err := agenttest.MustNew(live, agenttest.MemJournal(), agent.WithTools(htmlTool{})).Run(ctx, "r", agent.UserText("hi")); err != nil {
		t.Fatalf("live run: %v", err)
	}
	if len(live.wire) != 2 {
		t.Fatalf("live run made %d model calls, want 2", len(live.wire))
	}

	store := agenttest.MemJournal()
	crashed := &wireModel{m: New("k"), script: script(), failOn: 2}
	if _, err := agenttest.MustNew(crashed, store, agent.WithTools(htmlTool{})).Run(ctx, "r", agent.UserText("hi")); err == nil {
		t.Fatal("the crashing run should fail on its second model call")
	}
	resumed := &wireModel{m: New("k"), script: script()}
	if _, err := agenttest.MustNew(resumed, store, agent.WithTools(htmlTool{})).Run(ctx, "r", agent.UserText("hi")); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if len(resumed.wire) != 1 {
		t.Fatalf("resumed run made %d model calls, want 1", len(resumed.wire))
	}
	if !bytes.Equal(live.wire[1], resumed.wire[0]) {
		t.Fatalf("turn 2 differs after a resume\nlive:    %s\nresumed: %s", live.wire[1], resumed.wire[0])
	}
	// The crashed run's second call is the live turn 2 as well: it saw the journaled turn 1.
	if !bytes.Equal(crashed.wire[1], resumed.wire[0]) {
		t.Fatalf("turn 2 differs between the first attempt and the resume\nfirst:   %s\nresumed: %s", crashed.wire[1], resumed.wire[0])
	}
}
