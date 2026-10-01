package agent_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// flakyRetriever fails its first `fail` calls, then returns one document.
type flakyRetriever struct{ calls, fail int }

func (r *flakyRetriever) Retrieve(context.Context, string, int) ([]agent.Doc, error) {
	r.calls++
	if r.calls <= r.fail {
		return nil, errors.New("vector store: transient timeout")
	}
	return []agent.Doc{{ID: "1", Text: "doc"}}, nil
}

// R1: a Retry model middleware no longer retries a transient retrieval error. On v0.9.0 the
// retrieval was a middleware, and one placed inside Retry was retried with the model call.
func TestRev127_RetryMiddlewareCoversRetrieval(t *testing.T) {
	r := &flakyRetriever{fail: 1}
	store := agent.NewMemStore()
	a, err := agent.Build(agent.NewScriptedModel(agent.TextTurn("done")), store.Journal(),
		agent.WithMiddleware(middleware.Retry(3, middleware.WithBackoff(0, 0))), agent.WithRetrieval(r, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r1", "q"); err != nil {
		t.Fatalf("run failed on one transient retrieval error under Retry(3): %v (retriever calls %d)", err, r.calls)
	}
}

// R2: a model middleware that refuses the call (a policy gate, a spend cap, a rate limiter) no
// longer stops the retrieval: the user's query is sent to the retriever, and the documents are
// journaled, for a call that is never made.
func TestRev127_RefusingMiddlewareStopsRetrieval(t *testing.T) {
	r := &flakyRetriever{}
	deny := func(agent.ModelHandler) agent.ModelHandler {
		return func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
			return agent.ModelResponse{}, errors.New("policy: model call denied")
		}
	}
	store := agent.NewMemStore()
	a, err := agent.Build(agent.NewScriptedModel(agent.TextTurn("done")), store.Journal(),
		agent.WithMiddleware(deny), agent.WithRetrieval(r, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r2", "secret question"); err == nil {
		t.Fatal("want the denial")
	}
	recs, _ := store.History(context.Background(), "r2")
	var journaled []string
	for _, rec := range recs {
		if strings.HasPrefix(rec.Name, "@retrieval/") {
			journaled = append(journaled, rec.Name)
		}
	}
	if r.calls != 0 || len(journaled) != 0 {
		t.Fatalf("denied call: retriever called %d times, retrieval records %v; want none", r.calls, journaled)
	}
}

// R3 (review of #127): the system prompt function is called only by a drive that sends the model a
// request, once, before the first: a finished run is read back while the prompt's source is down.
func TestSystemPromptFunc_OnlyWhenTheModelIsCalled(t *testing.T) {
	store := agent.NewMemStore()
	down, calls := false, 0
	fn := func(context.Context, agent.RunInfo) (string, error) {
		calls++
		if down {
			return "", errors.New("tenant db down")
		}
		return "you are helpful", nil
	}
	noop := agent.Func("noop", "", agent.Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { return "ok", nil })
	a, err := agent.Build(agent.NewScriptedModel(agent.ToolTurn("c1", "noop", `{}`), agent.TextTurn("answer")), store.Journal(),
		agent.WithSystemPromptFunc(fn), agent.WithTools(noop))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r3", "q"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("a drive of two model turns called the prompt function %d times, want 1", calls)
	}
	down = true
	msg, err := a.Run(context.Background(), "r3", "q")
	if err != nil || msg.Text() != "answer" {
		t.Fatalf("re-reading a finished run: %q, %v; want the recorded answer", msg.Text(), err)
	}
	if calls != 1 {
		t.Fatalf("reading back a finished run called the prompt function (%d calls in all)", calls)
	}
}

// Sanity: retrieval runs once across a pause and a resume (replayed from the journal).
func TestRev127_RetrievalOnceAcrossPause(t *testing.T) {
	r := &flakyRetriever{}
	ask := agent.Func("ask", "", agent.Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		return agent.Interrupt[string](ctx, "q", "ok?")
	})
	store := agent.NewMemStore()
	var sent []int
	count := func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, c agent.ModelCall) (agent.ModelResponse, error) {
			n := 0
			for _, m := range c.Request.Messages {
				if strings.Contains(m.Text(), "\"doc\"") {
					n++
				}
			}
			sent = append(sent, n)
			return next(ctx, c)
		}
	}
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "ask", `{}`), agent.TextTurn("done"))
	a, err := agent.Build(m, store.Journal(), agent.WithTools(ask), agent.WithMiddleware(count), agent.WithRetrieval(r, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r4", "q"); err == nil {
		t.Fatal("want a pause")
	}
	if err := agent.AnswerInterrupt(context.Background(), store, "r4", "q", "yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r4", "q"); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 || len(sent) != 2 || sent[0] != 1 || sent[1] != 1 {
		t.Fatalf("retriever calls %d, docs per request %v; want 1 and [1 1]", r.calls, sent)
	}
}

// S1 (design gap): a programmatic sub-run started under SubRunFor from a saga's tool call, as a
// saga, is "part of the run's tree" (SubRunFor's doc), but the parent's rollback does not reach
// it: its completed write is never compensated, and the abort does not list it as uncompensated.
func TestRev127_SagaRollbackReachesProgrammaticSubRun(t *testing.T) {
	store := agent.NewMemStore()
	undone := 0
	book := agent.CompensatedFunc("book", "", agent.Safety{},
		func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { undone++; return nil })
	child, err := agent.Build(agent.NewScriptedModel(agent.ToolTurn("k1", "book", `{}`), agent.TextTurn("child done")),
		store.Journal(), agent.WithTools(book))
	if err != nil {
		t.Fatal(err)
	}
	starter := agent.Func("starter", "", agent.Safety{}, func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := agent.RunInfoFrom(ctx)
		msg, err := child.RunSaga(ctx, info.SubRunFor("child"), "work")
		return msg.Text(), err
	})
	boom := agent.Func("boom", "", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("boom")
	})
	parent, err := agent.Build(agent.NewScriptedModel(agent.ToolTurn("c1", "starter", `{}`), agent.ToolTurn("c2", "boom", `{}`), agent.TextTurn("x")),
		store.Journal(), agent.WithTools(starter, boom))
	if err != nil {
		t.Fatal(err)
	}
	_, err = parent.RunSaga(context.Background(), "root", "go")
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	t.Logf("undone %d; compensated %v; uncompensated %v", undone, ab.Compensated, ab.Uncompensated)
	if undone != 1 && !slices.Contains(ab.Uncompensated, "book") {
		t.Fatalf("child's booked write: undone %d times; abort compensated %v, uncompensated %v: the write is neither undone nor reported",
			undone, ab.Compensated, ab.Uncompensated)
	}
}
