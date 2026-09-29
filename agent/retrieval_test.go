package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
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

// Outside a run there is no journal to hold the documents, so a tool-result turn retrieves
// again for the latest user message: the model is not left without the context mid-loop.
func TestWithRetrieval_ToolResultTurnOutsideRunKeepsContext(t *testing.T) {
	r := &fakeRetriever{docs: []Doc{{Text: "x marks the spot"}}}
	var seen Request
	base := ModelHandler(func(_ context.Context, req Request) (Message, Usage, error) {
		seen = req
		return Message{}, Usage{}, nil
	})
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
	if r.lastQ != "q" {
		t.Fatalf("retriever query = %q, want the latest user message", r.lastQ)
	}
	if !strings.Contains(seen.Messages[0].Text(), "x marks the spot") {
		t.Fatalf("first message = %+v, want the retrieved context", seen.Messages[0])
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

// A similarity score can come out NaN or infinite (cosine similarity against a zero vector is
// 0/0), and JSON has no encoding for either. Such a score is dropped, so the model still gets
// the documents rather than the encoder's "unsupported value" error, and the Retriever's own
// slice is left as it was.
func TestRetrievalTool_NonFiniteScoreDropped(t *testing.T) {
	docs := []Doc{
		{ID: "a", Text: "alpha", Score: math.NaN()},
		{ID: "b", Text: "beta", Score: math.Inf(1)},
		{ID: "c", Text: "gamma", Score: math.Inf(-1)},
		{ID: "d", Text: "delta", Score: 0.5},
	}
	r := &fakeRetriever{docs: docs}

	res, err := RetrievalTool(r, 4).Call(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("RetrievalTool with a non-finite score: %v", err)
	}
	var got []Doc
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatal(err)
	}
	want := []Doc{{ID: "a", Text: "alpha"}, {ID: "b", Text: "beta"}, {ID: "c", Text: "gamma"}, {ID: "d", Text: "delta", Score: 0.5}}
	if len(got) != len(want) {
		t.Fatalf("got %s, want %d docs", res, len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Text != want[i].Text || got[i].Score != want[i].Score {
			t.Errorf("doc %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !math.IsNaN(docs[0].Score) || !math.IsInf(docs[1].Score, 1) || !math.IsInf(docs[2].Score, -1) {
		t.Errorf("the Retriever's docs were modified: %+v", docs)
	}
}

// The retrieved context goes after the agent's own system prompt, not ahead of it: the
// operator's instructions lead, ahead of retrieved text they do not control, and the prompt
// stays a constant prefix that a provider's prompt cache can reuse from turn to turn.
func TestWithRetrieval_ContextFollowsSystemPrompt(t *testing.T) {
	r := &fakeRetriever{docs: []Doc{{Text: "Paris is the capital of France"}}}
	var seen Request
	base := ModelHandler(func(_ context.Context, req Request) (Message, Usage, error) {
		seen = req
		return Message{}, Usage{}, nil
	})
	req := Request{Messages: []Message{SystemText("OPERATOR"), UserText("what's the capital?")}}
	if _, _, err := WithRetrieval(r, 2)(base)(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(seen.Messages) != 3 {
		t.Fatalf("model got %d messages, want 3: %+v", len(seen.Messages), seen.Messages)
	}
	if m := seen.Messages[0]; m.Role != RoleSystem || m.Text() != "OPERATOR" {
		t.Errorf("message 0 = %+v, want the operator's system prompt", m)
	}
	if m := seen.Messages[1]; m.Role != RoleSystem || !strings.Contains(m.Text(), "Paris is the capital") {
		t.Errorf("message 1 = %+v, want the retrieved context", m)
	}
	if m := seen.Messages[2]; m.Role != RoleUser {
		t.Errorf("message 2 = %+v, want the user turn", m)
	}
	if req.Messages[0].Text() != "OPERATOR" || len(req.Messages) != 2 {
		t.Errorf("the caller's request was modified: %+v", req.Messages)
	}
}

// seqRetriever returns a different document on every call, as a store does when its contents
// change, so a test can tell which retrieval a model call saw.
type seqRetriever struct {
	mu    sync.Mutex
	calls int
}

func (r *seqRetriever) Retrieve(context.Context, string, int) ([]Doc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return []Doc{{ID: "d", Text: fmt.Sprintf("version-%d", r.calls), Score: math.NaN()}}, nil
}

func (r *seqRetriever) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// captureRequests is model middleware (install it inside WithRetrieval) that records the
// context block each live model call was sent, or "" for a call sent none.
func captureRequests(got *[]string) Middleware {
	return func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, req Request) (Message, Usage, error) {
			block := ""
			for _, m := range req.Messages {
				if m.Role == RoleSystem && strings.HasPrefix(m.Text(), "Relevant context:") {
					block = m.Text()
				}
			}
			*got = append(*got, block)
			return next(ctx, req)
		}
	}
}

// noopTool is a read-only tool for driving the loop through a tool-result turn.
var noopTool = Func("noop", "does nothing", Safety{ReadOnly: true},
	func(context.Context, struct{}) (string, error) { return "ok", nil })

// Within a run, the model call that follows a tool result still sees the retrieved context:
// the same documents the first call saw, recorded once in the journal, not retrieved again.
func TestWithRetrieval_ContextKeptAcrossTheRun(t *testing.T) {
	r := &seqRetriever{}
	var got []string
	m := NewScriptedModel(ToolTurn("c1", "noop", `{}`), TextTurn("done"))
	a := New(m, NewMemStore(), noopTool).WithSystemPrompt("OPERATOR").Use(WithRetrieval(r, 2), captureRequests(&got))
	if _, err := a.Run(context.Background(), "run-1", "q"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("model called %d times, want 2", len(got))
	}
	for i, block := range got {
		if !strings.Contains(block, "version-1") {
			t.Errorf("model call %d got context %q, want the first retrieval (version-1)", i, block)
		}
	}
	if n := r.count(); n != 1 {
		t.Errorf("retriever called %d times in one run, want 1", n)
	}
}

// A resumed run shows the model exactly the documents the original run retrieved, even when
// the store has changed since, and the journal holds what was retrieved, so the context a
// recorded answer was given can be audited.
func TestWithRetrieval_ResumeInjectsRecordedDocs(t *testing.T) {
	r := &seqRetriever{}
	store := NewMemStore()
	crash := NewScriptedModel(ToolTurn("c1", "noop", `{}`), ErrorTurn(errors.New("process died")))
	if _, err := New(crash, store, noopTool).Use(WithRetrieval(r, 2)).Run(context.Background(), "run-1", "q"); err == nil {
		t.Fatal("first run: want the scripted crash")
	}

	var got []string
	m := NewScriptedModel(ToolTurn("c1", "noop", `{}`), TextTurn("done"))
	a := New(m, store, noopTool).Use(WithRetrieval(r, 2), captureRequests(&got))
	if _, err := a.Run(context.Background(), "run-1", "q"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "version-1") {
		t.Fatalf("resumed model call got context %q, want the original retrieval (version-1)", got)
	}
	if n := r.count(); n != 1 {
		t.Errorf("retriever called %d times across a crash and resume, want 1", n)
	}

	recs, err := store.History(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	var journaled bool
	for _, rec := range recs {
		if rec.Kind == StepValue && strings.Contains(string(rec.Result), "version-1") {
			journaled = true
		}
	}
	if !journaled {
		t.Errorf("no journal record holds the retrieved documents: %+v", recs)
	}
}

// A sub-agent's retrieval is its own: it is journaled in the sub-run, not in the parent run
// whose tool called it, so each run's model sees the documents retrieved for its own input.
func TestWithRetrieval_SubAgentRetrievesForItself(t *testing.T) {
	parentR, subR := &seqRetriever{}, &fakeRetriever{docs: []Doc{{Text: "sub-doc"}}}
	var parentGot, subGot []string
	store := NewMemStore()
	sub := New(NewScriptedModel(TextTurn("sub answer")), store).Use(WithRetrieval(subR, 1), captureRequests(&subGot))
	parent := New(NewScriptedModel(ToolTurn("c1", "helper", `{"task":"sub question"}`), TextTurn("done")), store,
		SubAgent("helper", "a helper", sub)).Use(WithRetrieval(parentR, 1), captureRequests(&parentGot))
	if _, err := parent.Run(context.Background(), "run-1", "q"); err != nil {
		t.Fatal(err)
	}
	if len(subGot) != 1 || !strings.Contains(subGot[0], "sub-doc") {
		t.Errorf("sub-agent got context %q, want its own retrieval (sub-doc)", subGot)
	}
	if subR.lastQ != "sub question" {
		t.Errorf("sub-agent retrieved for %q, want its own input", subR.lastQ)
	}
	for i, block := range parentGot {
		if !strings.Contains(block, "version-1") {
			t.Errorf("parent model call %d got context %q, want its own retrieval (version-1)", i, block)
		}
	}
}
