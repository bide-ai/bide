package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeRetriever records the query it was asked and returns canned docs (or an error).
type fakeRetriever struct {
	docs  []Doc
	err   error
	lastQ string
	lastK int
	calls int
}

func (r *fakeRetriever) Retrieve(_ context.Context, query string, k int) ([]Doc, error) {
	r.calls++
	r.lastQ, r.lastK = query, k
	return r.docs, r.err
}

// RetrievalTool lets the model search on demand and returns the docs as its result.
func TestRetrievalTool_SearchesOnDemand(t *testing.T) {
	r := &fakeRetriever{docs: []Doc{{ID: "1", Text: "the sky is blue"}}}
	tool := RetrievalTool(r, 3)

	res, err := tool.Call(context.Background(), json.RawMessage(`{"query":"sky color"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.lastQ != "sky color" || r.lastK != 3 {
		t.Fatalf("retriever got q=%q k=%d, want 'sky color'/3", r.lastQ, r.lastK)
	}
	if !strings.Contains(string(res), "the sky is blue") {
		t.Fatalf("tool result = %s, want the doc text", res)
	}
	if tool.Safety().ReadOnly != true {
		t.Error("retrieval tool should be ReadOnly")
	}
}

// WithRetrieval injects retrieved docs as context when responding to a user turn.
func TestWithRetrieval_InjectsOnUserTurn(t *testing.T) {
	r := &fakeRetriever{docs: []Doc{{Text: "Paris is the capital of France"}}}
	var seen Request
	base := ModelHandler(func(_ context.Context, req Request) (Message, Usage, error) {
		seen = req
		return Message{}, Usage{}, nil
	})
	h := WithRetrieval(r, 2)(base)

	_, _, err := h(context.Background(), Request{Messages: []Message{UserText("what's the capital?")}})
	if err != nil {
		t.Fatal(err)
	}
	if r.lastQ != "what's the capital?" {
		t.Fatalf("retriever query = %q", r.lastQ)
	}
	// A context system message was prepended carrying the doc text.
	if seen.Messages[0].Role != RoleSystem || !strings.Contains(seen.Messages[0].Text(), "Paris is the capital") {
		t.Fatalf("first message = %+v, want injected context", seen.Messages[0])
	}
}

// WithRetrieval does NOT retrieve on a tool-result turn (mid-loop).
func TestWithRetrieval_SkipsOnToolResultTurn(t *testing.T) {
	r := &fakeRetriever{docs: []Doc{{Text: "x"}}}
	base := ModelHandler(func(context.Context, Request) (Message, Usage, error) { return Message{}, Usage{}, nil })
	h := WithRetrieval(r, 2)(base)

	// Last message is a tool result, not a user turn.
	msgs := []Message{
		UserText("q"),
		{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "t"}}},
		{Role: RoleTool, Parts: []Part{ToolResult{ToolUseID: "c1", Result: json.RawMessage(`{}`)}}},
	}
	if _, _, err := h(context.Background(), Request{Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 {
		t.Fatalf("retriever called %d times on a tool-result turn, want 0", r.calls)
	}
}

// A retrieval error aborts the model call (users degrade by returning nil,nil instead).
func TestWithRetrieval_ErrorAborts(t *testing.T) {
	r := &fakeRetriever{err: errors.New("vector store down")}
	base := ModelHandler(func(context.Context, Request) (Message, Usage, error) { return Message{}, Usage{}, nil })
	h := WithRetrieval(r, 2)(base)

	_, _, err := h(context.Background(), Request{Messages: []Message{UserText("q")}})
	if err == nil || !strings.Contains(err.Error(), "vector store down") {
		t.Fatalf("err = %v, want the retrieval error", err)
	}
}

// ExampleRetrievalTool shows bring-your-own-RAG: implement Retriever against your store,
// wire it in as a tool (or with WithRetrieval), and the loop handles the rest.
func ExampleRetrievalTool() {
	// Your store, behind the Retriever port (here a trivial in-memory one).
	docs := map[string]string{"france": "Paris is the capital of France."}
	r := retrieverFunc(func(_ context.Context, query string, _ int) ([]Doc, error) {
		if d, ok := docs[strings.ToLower(query)]; ok {
			return []Doc{{Text: d}}, nil
		}
		return nil, nil
	})

	tool := RetrievalTool(r, 1)
	out, _ := tool.Call(context.Background(), json.RawMessage(`{"query":"france"}`))
	fmt.Println(string(out))
	// Output: [{"text":"Paris is the capital of France."}]
}

// retrieverFunc adapts a function to the Retriever interface (example convenience).
type retrieverFunc func(context.Context, string, int) ([]Doc, error)

func (f retrieverFunc) Retrieve(ctx context.Context, q string, k int) ([]Doc, error) {
	return f(ctx, q, k)
}

// k is how many documents to return, so a k below 1 asks for nothing a Retriever can serve
// consistently (one store returns nothing, another ignores it): both helpers reject it when
// they are built, as New rejects a nil model.
func TestRetrieval_NonPositiveKPanics(t *testing.T) {
	for _, k := range []int{0, -1} {
		for name, build := range map[string]func(){
			"RetrievalTool": func() { RetrievalTool(&fakeRetriever{}, k) },
			"WithRetrieval": func() { WithRetrieval(&fakeRetriever{}, k) },
		} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("%s(k=%d) did not panic", name, k)
					}
				}()
				build()
			}()
		}
	}
}

// Both helpers promise the top-k documents, so a Retriever that returns more than k is cut to
// its first k, in the order it ranked them: the model is not sent, and the journal does not
// hold, more than was asked for.
func TestRetrieval_CapsAtK(t *testing.T) {
	r := &fakeRetriever{docs: []Doc{{ID: "1", Text: "one"}, {ID: "2", Text: "two"}, {ID: "3", Text: "three"}}}

	res, err := RetrievalTool(r, 2).Call(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got []Doc
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
		t.Fatalf("RetrievalTool(k=2) returned %s, want docs 1 and 2", res)
	}

	var seen Request
	base := ModelHandler(func(_ context.Context, req Request) (Message, Usage, error) {
		seen = req
		return Message{}, Usage{}, nil
	})
	if _, _, err := WithRetrieval(r, 2)(base)(context.Background(), Request{Messages: []Message{UserText("q")}}); err != nil {
		t.Fatal(err)
	}
	if block := seen.Messages[0].Text(); !strings.Contains(block, "two") || strings.Contains(block, "three") {
		t.Fatalf("WithRetrieval(k=2) injected %q, want docs 1 and 2 only", block)
	}
}
