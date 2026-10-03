package gemini

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// sseServer answers the i-th request (from 0) with turns[i] framed as one SSE data line; the
// last turn repeats. It records every request body.
func sseServer(t *testing.T, turns ...string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "data: "+turns[min(n, len(turns)-1)]+"\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// Gemini sends no tool-call ids, so the adapter makes them up. A later call to the same tool
// must not get the id of an earlier one: the agent keys results by id, and before the fix the
// second lookup was skipped as already done while the run reported success.
func TestStream_ToolCallIDsDifferAcrossTurns(t *testing.T) {
	srv, _ := sseServer(t,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"first"}}}]},"finishReason":"STOP"}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"second"}}}]},"finishReason":"STOP"}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}]}`,
	)
	var calls []string
	tool := agent.MustFunc("lookup", "l", func(_ context.Context, in struct {
		Q string `json:"q"`
	}) (string, error) {
		calls = append(calls, in.Q)
		return "res:" + in.Q, nil
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	a := agenttest.MustNew(New("k", WithBaseURL(srv.URL)), agenttest.MemJournal(), agent.WithTools(tool))
	if _, err := a.Run(context.Background(), "r1", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("tool calls executed = %v, want [first second]", calls)
	}
}

// The synthesized id must not be built from the name and index: tool "a" at index 10 and tool
// "a1" at index 0 both gave "call_a10".
func TestStreamSSE_ToolCallIDsAreUnambiguous(t *testing.T) {
	var parts string
	for i := range 10 {
		parts += fmt.Sprintf(`{"functionCall":{"name":"f%d","args":{}}},`, i)
	}
	first := `data: {"candidates":[{"content":{"parts":[` + parts + `{"functionCall":{"name":"a","args":{}}}]},"finishReason":"STOP"}]}` + "\n\n"
	second := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"a1","args":{}}}]},"finishReason":"STOP"}]}` + "\n\n"
	m1, _, err := testStream(first).Message()
	if err != nil {
		t.Fatal(err)
	}
	m2, _, err := testStream(second).Message()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range []agent.Message{m1, m2} {
		for _, p := range m.Parts {
			tu := p.(agent.ToolUse)
			if seen[tu.ID] {
				t.Fatalf("tool-call id %q issued twice", tu.ID)
			}
			seen[tu.ID] = true
		}
	}
}

// When Gemini does send an id with the call, the adapter keeps it.
func TestStreamSSE_KeepsProviderToolCallID(t *testing.T) {
	msg, _, err := testStream(`data: {"candidates":[{"content":{"parts":[{"functionCall":{"id":"g-7","name":"a","args":{}}}]},"finishReason":"STOP"}]}` + "\n\n").Message()
	if err != nil {
		t.Fatal(err)
	}
	if id := msg.Parts[0].(agent.ToolUse).ID; id != "g-7" {
		t.Fatalf("id = %q, want g-7", id)
	}
}
