package agent_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// p14Turn is one scripted model turn of a p14Model: tool calls, or a text answer, and a hook the
// model runs before it answers (to land a journal write mid-turn, say).
type p14Turn struct {
	calls []agent.ToolUse
	text  string
	err   error
	hook  func(req agent.Request)
}

// p14Model answers turn i of a run with turns[i], i being the number of assistant messages in the
// request (so a resumed run lines up with its live turns), and records every request.
type p14Model struct {
	turns []p14Turn
	calls atomic.Int32
	mu    sync.Mutex
	reqs  []agent.Request
}

func (m *p14Model) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	m.mu.Unlock()
	i := 0
	for _, msg := range req.Messages {
		if msg.Role == agent.RoleAssistant {
			i++
		}
	}
	if i >= len(m.turns) {
		i = len(m.turns) - 1
	}
	t := m.turns[i]
	if t.hook != nil {
		t.hook(req)
	}
	ch := make(chan agent.Emit, len(t.calls)+2)
	switch {
	case t.err != nil:
		ch <- agent.Emit{Err: t.err}
	case len(t.calls) > 0:
		for k, c := range t.calls {
			ch <- agent.Emit{Event: agent.ToolCallDelta{Index: k, ID: c.ID, Name: c.Name, ArgsFragment: c.Args}}
		}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	default:
		ch <- agent.Emit{Event: agent.TextDelta{Text: t.text}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// lastReq returns the last request the model was sent.
func (m *p14Model) lastReq(t *testing.T) agent.Request {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.reqs) == 0 {
		t.Fatal("the model was never called")
	}
	return m.reqs[len(m.reqs)-1]
}

func call(id, name string) agent.ToolUse {
	return agent.ToolUse{ID: id, Name: name, Args: json.RawMessage(`{}`)}
}

// p14Journal returns a journal over a fresh MemStore, and the store.
func p14Journal(t *testing.T) (*agent.Journal, *agent.MemStore) {
	t.Helper()
	m := agent.NewMemStore()
	j, err := agent.NewJournal(m)
	if err != nil {
		t.Fatal(err)
	}
	return j, m
}

// p14Build builds an agent over j, failing the test on an error.
func p14Build(t *testing.T, model agent.Model, j *agent.Journal, opts ...agent.Option) *agent.Agent {
	t.Helper()
	a, err := agent.New(model, j, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// writeMarker records the value marker name in runID's journal directly through the store, as
// another process (Cancel, a second driver) would.
func writeMarker(t *testing.T, s agent.Store, runID, name string, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	data, err := agent.JournalEntry(name, agent.Record{Kind: agent.StepValue, Result: b})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(context.Background(), runID, name, data); err != nil {
		t.Fatal(err)
	}
}

// has reports whether runID's journal holds name.
func has(t *testing.T, s agent.Store, runID, name string) bool {
	t.Helper()
	_, ok, err := s.Get(context.Background(), runID, name)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// counter is a tool that counts its calls.
type counter struct{ n atomic.Int32 }

func (c *counter) tool(name string, safety agent.Safety, opts ...agent.ToolOption) agent.Tool {
	return agent.MustFunc(name, "", func(context.Context, struct{}) (string, error) {
		c.n.Add(1)
		return "ok", nil
	}, append([]agent.ToolOption{agent.WithSafety(safety)}, opts...)...)
}

// reason is the value Cancel journals.
type reason struct {
	Reason string `json:"reason"`
}
