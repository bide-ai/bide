package agent_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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

// retrievalRecords returns the names of runID's retrieval records.
func retrievalRecords(t *testing.T, store *agent.MemStore, runID string) []string {
	t.Helper()
	recs, err := store.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, rec := range recs {
		if strings.HasPrefix(rec.Name, "@retrieval/") {
			names = append(names, rec.Name)
		}
	}
	return names
}

// R1 (review of #127): WithRetrieval retrieves before the model middleware chain, so a Retry model
// middleware does not retry a failed retrieval; WithRetrievalRetry does, within the step, and only
// the attempt that succeeded is recorded.
func TestWithRetrievalRetry_RetriesTheStep(t *testing.T) {
	ctx := context.Background()
	retry := agent.WithMiddleware(middleware.Retry(3, middleware.WithBackoff(0, 0)))

	// Retry middleware alone: the transient error fails the drive after one call.
	r := &flakyRetriever{fail: 1}
	store := agent.NewMemStore()
	a, err := agent.Build(agent.NewScriptedModel(agent.TextTurn("done")), store.Journal(), retry, agent.WithRetrieval(r, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "plain", "q"); err == nil || r.calls != 1 {
		t.Fatalf("under Retry middleware only: err %v after %d retriever calls; want the retrieval error after 1", err, r.calls)
	}

	// WithRetrievalRetry(2): two failures, then the documents, recorded once.
	r = &flakyRetriever{fail: 2}
	store = agent.NewMemStore()
	a, err = agent.Build(agent.NewScriptedModel(agent.TextTurn("done")), store.Journal(),
		agent.WithRetrieval(r, 1, agent.WithRetrievalRetry(2, time.Millisecond, 2*time.Millisecond)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "retried", "q"); err != nil || r.calls != 3 {
		t.Fatalf("WithRetrievalRetry(2) over two failures: err %v after %d calls; want success after 3", err, r.calls)
	}
	if got := retrievalRecords(t, store, "retried"); !slices.Equal(got, []string{"@retrieval/0"}) {
		t.Fatalf("retrieval records %v, want one", got)
	}

	// Every attempt fails: nothing is recorded, and the next drive retrieves again.
	r = &flakyRetriever{fail: 3}
	store = agent.NewMemStore()
	a, err = agent.Build(agent.NewScriptedModel(agent.TextTurn("done")), store.Journal(),
		agent.WithRetrieval(r, 1, agent.WithRetrievalRetry(1, 0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "spent", "q"); err == nil || r.calls != 2 || len(retrievalRecords(t, store, "spent")) != 0 {
		t.Fatalf("retries spent: err %v, %d calls, records %v; want an error after 2 calls and no record",
			err, r.calls, retrievalRecords(t, store, "spent"))
	}
	if _, err := a.Run(ctx, "spent", "q"); err != nil || r.calls != 4 {
		t.Fatalf("next drive: err %v, %d calls in all; want success on the fourth", err, r.calls)
	}
}

// WithRetrievalRetry does not retry an ErrConfig, stops when the context is cancelled during a
// backoff, and refuses invalid values at Build.
func TestWithRetrievalRetry_StopsAndValidates(t *testing.T) {
	calls := 0
	refuse := agent.RetrieverFunc(func(context.Context, string, int) ([]agent.Doc, error) {
		calls++
		return nil, fmt.Errorf("bad index name: %w", agent.ErrConfig)
	})
	a, err := agent.Build(agent.NewScriptedModel(agent.TextTurn("done")), agent.NewMemStore().Journal(),
		agent.WithRetrieval(refuse, 1, agent.WithRetrievalRetry(5, 0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "cfg", "q"); !errors.Is(err, agent.ErrConfig) || calls != 1 {
		t.Fatalf("ErrConfig from the Retriever: err %v after %d calls; want it unretried", err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelling := agent.RetrieverFunc(func(context.Context, string, int) ([]agent.Doc, error) {
		cancel() // the caller gives up while the step backs off
		return nil, errors.New("transient")
	})
	a, err = agent.Build(agent.NewScriptedModel(agent.TextTurn("done")), agent.NewMemStore().Journal(),
		agent.WithRetrieval(cancelling, 1, agent.WithRetrievalRetry(5, time.Hour, time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "cancel", "q"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled during the backoff: err %v, want context.Canceled", err)
	}

	for _, o := range []agent.RetrievalOption{
		agent.WithRetrievalRetry(-1, 0, 0), agent.WithRetrievalRetry(1, -1, 0), agent.WithRetrievalRetry(1, 2, 1), nil,
	} {
		if _, err := agent.Build(agent.NewScriptedModel(), agent.NewMemStore().Journal(),
			agent.WithRetrieval(&flakyRetriever{}, 1, o)); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("WithRetrieval with option %v: Build err %v, want ErrConfig", o, err)
		}
	}
}

// R2 (review of #127): retrieval runs before the model middleware chain, so a model middleware
// that refuses the call does not prevent it (as documented on WithRetrieval): the query reaches
// the Retriever and the documents are journaled. A policy that must keep the query from the store
// wraps the Retriever, which this test shows does keep it.
func TestWithRetrieval_RunsBeforeModelMiddleware(t *testing.T) {
	deny := func(agent.ModelHandler) agent.ModelHandler {
		return func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
			return agent.ModelResponse{}, errors.New("policy: model call denied")
		}
	}
	r := &flakyRetriever{}
	store := agent.NewMemStore()
	a, err := agent.Build(agent.NewScriptedModel(agent.TextTurn("done")), store.Journal(),
		agent.WithMiddleware(deny), agent.WithRetrieval(r, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "denied", "secret question"); err == nil {
		t.Fatal("want the denial")
	}
	if got := retrievalRecords(t, store, "denied"); r.calls != 1 || !slices.Equal(got, []string{"@retrieval/0"}) {
		t.Fatalf("model call denied by middleware: retriever called %d times, records %v; want 1 and the record", r.calls, got)
	}

	// The policy wraps the Retriever instead: the query never reaches the store.
	r = &flakyRetriever{}
	allowed := func(context.Context) bool { return false }
	gated := agent.RetrieverFunc(func(ctx context.Context, q string, k int) ([]agent.Doc, error) {
		if !allowed(ctx) {
			return nil, errors.New("policy: retrieval denied")
		}
		return r.Retrieve(ctx, q, k)
	})
	store = agent.NewMemStore()
	a, err = agent.Build(agent.NewScriptedModel(agent.TextTurn("done")), store.Journal(), agent.WithRetrieval(gated, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "gated", "secret question"); err == nil {
		t.Fatal("want the denial")
	}
	if got := retrievalRecords(t, store, "gated"); r.calls != 0 || len(got) != 0 {
		t.Fatalf("gated retriever: store called %d times, records %v; want none", r.calls, got)
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
