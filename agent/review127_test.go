package agent_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
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
func retrievalRecords(t *testing.T, store *agent.Journal, runID string) []string {
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
	store := agenttest.MemJournal()
	a, err := agent.New(agent.NewScriptedModel(agent.TextTurn("done")), store, retry, agent.WithRetrieval(r, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "plain", agent.UserText("q")); err == nil || r.calls != 1 {
		t.Fatalf("under Retry middleware only: err %v after %d retriever calls; want the retrieval error after 1", err, r.calls)
	}

	// WithRetrievalRetry(2): two failures, then the documents, recorded once.
	r = &flakyRetriever{fail: 2}
	store = agenttest.MemJournal()
	a, err = agent.New(agent.NewScriptedModel(agent.TextTurn("done")), store,
		agent.WithRetrieval(r, 1, agent.WithRetrievalRetry(2, time.Millisecond, 2*time.Millisecond)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "retried", agent.UserText("q")); err != nil || r.calls != 3 {
		t.Fatalf("WithRetrievalRetry(2) over two failures: err %v after %d calls; want success after 3", err, r.calls)
	}
	if got := retrievalRecords(t, store, "retried"); !slices.Equal(got, []string{"@retrieval/0"}) {
		t.Fatalf("retrieval records %v, want one", got)
	}

	// Every attempt fails: nothing is recorded, and the next drive retrieves again.
	r = &flakyRetriever{fail: 3}
	store = agenttest.MemJournal()
	a, err = agent.New(agent.NewScriptedModel(agent.TextTurn("done")), store,
		agent.WithRetrieval(r, 1, agent.WithRetrievalRetry(1, 0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "spent", agent.UserText("q")); err == nil || r.calls != 2 || len(retrievalRecords(t, store, "spent")) != 0 {
		t.Fatalf("retries spent: err %v, %d calls, records %v; want an error after 2 calls and no record",
			err, r.calls, retrievalRecords(t, store, "spent"))
	}
	if _, err := a.Run(ctx, "spent", agent.UserText("q")); err != nil || r.calls != 4 {
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
	a, err := agent.New(agent.NewScriptedModel(agent.TextTurn("done")), agenttest.MemJournal(),
		agent.WithRetrieval(refuse, 1, agent.WithRetrievalRetry(5, 0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "cfg", agent.UserText("q")); !errors.Is(err, agent.ErrConfig) || calls != 1 {
		t.Fatalf("ErrConfig from the Retriever: err %v after %d calls; want it unretried", err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelling := agent.RetrieverFunc(func(context.Context, string, int) ([]agent.Doc, error) {
		cancel() // the caller gives up while the step backs off
		return nil, errors.New("transient")
	})
	a, err = agent.New(agent.NewScriptedModel(agent.TextTurn("done")), agenttest.MemJournal(),
		agent.WithRetrieval(cancelling, 1, agent.WithRetrievalRetry(5, time.Hour, time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "cancel", agent.UserText("q")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled during the backoff: err %v, want context.Canceled", err)
	}

	for _, o := range []agent.RetrievalOption{
		agent.WithRetrievalRetry(-1, 0, 0), agent.WithRetrievalRetry(1, -1, 0), agent.WithRetrievalRetry(1, 2, 1), nil,
	} {
		if _, err := agent.New(agent.NewScriptedModel(), agenttest.MemJournal(),
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
	store := agenttest.MemJournal()
	a, err := agent.New(agent.NewScriptedModel(agent.TextTurn("done")), store,
		agent.WithMiddleware(deny), agent.WithRetrieval(r, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "denied", agent.UserText("secret question")); err == nil {
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
	store = agenttest.MemJournal()
	a, err = agent.New(agent.NewScriptedModel(agent.TextTurn("done")), store, agent.WithRetrieval(gated, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "gated", agent.UserText("secret question")); err == nil {
		t.Fatal("want the denial")
	}
	if got := retrievalRecords(t, store, "gated"); r.calls != 0 || len(got) != 0 {
		t.Fatalf("gated retriever: store called %d times, records %v; want none", r.calls, got)
	}
}

// R3 (review of #127): the system prompt function is called only by a drive that sends the model a
// request, once, before the first: a finished run is read back while the prompt's source is down.
func TestSystemPromptFunc_OnlyWhenTheModelIsCalled(t *testing.T) {
	store := agenttest.MemJournal()
	down, calls := false, 0
	fn := func(context.Context, agent.RunInfo) (string, error) {
		calls++
		if down {
			return "", errors.New("tenant db down")
		}
		return "you are helpful", nil
	}
	noop := agent.MustFunc("noop", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
	a, err := agent.New(agent.NewScriptedModel(agent.ToolTurn("c1", "noop", `{}`), agent.TextTurn("answer")), store,
		agent.WithSystemPromptFunc(fn), agent.WithTools(noop))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r3", agent.UserText("q")); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("a drive of two model turns called the prompt function %d times, want 1", calls)
	}
	down = true
	res, err := a.Run(context.Background(), "r3", agent.UserText("q"))
	var msg agent.Message
	if res != nil {
		msg = res.Message
	}
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
	ask := agent.MustFunc("ask", "", func(ctx context.Context, _ struct{}) (string, error) {
		return agent.Interrupt[string](ctx, "q", "ok?")
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	store := agenttest.MemJournal()
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
	a, err := agent.New(m, store, agent.WithTools(ask), agent.WithMiddleware(count), agent.WithRetrieval(r, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r4", agent.UserText("q")); err == nil {
		t.Fatal("want a pause")
	}
	if err := store.AnswerInterrupt(context.Background(), "r4", "q", "yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r4", agent.UserText("q")); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 || len(sent) != 2 || sent[0] != 1 || sent[1] != 1 {
		t.Fatalf("retriever calls %d, docs per request %v; want 1 and [1 1]", r.calls, sent)
	}
}

// rulesModel is a scripted model that declares tool rules (ToolRules), or wraps one that does.
type rulesModel struct {
	agent.Model
	name     *regexp.Regexp
	required bool
	calls    int
}

func (m *rulesModel) ToolNameRule() *regexp.Regexp   { return m.name }
func (m *rulesModel) RequiresToolsForRequired() bool { return m.required }
func (m *rulesModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	m.calls++
	return m.Model.Stream(ctx, req)
}

// unwrapModel wraps a Model without declaring rules itself; Build follows Unwrap to its rules.
type unwrapModel struct{ agent.Model }

func (m unwrapModel) Unwrap() agent.Model { return m.Model }

// V1 (review of #127): Build checks tool names against the rule the agent's model declares
// (ToolRules), through an Unwrap chain, and checks nothing for a model that declares none.
func TestBuild_ToolNamesFollowTheModelsRule(t *testing.T) {
	named := func(n string) agent.Tool {
		return agent.MustFunc(n, "", func(context.Context, struct{}) (string, error) { return "", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
	}
	strict := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	gemini := regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.:-]{0,63}$`)
	j := agenttest.MemJournal()
	for _, tc := range []struct {
		name  string
		model agent.Model
		tool  string
		ok    bool
	}{
		{"strict refuses a space", &rulesModel{Model: agent.NewScriptedModel(), name: strict}, "get weather", false},
		{"strict refuses a dot", &rulesModel{Model: agent.NewScriptedModel(), name: strict}, "fs.read", false},
		{"strict refuses 65 chars", &rulesModel{Model: agent.NewScriptedModel(), name: strict}, strings.Repeat("a", 65), false},
		{"strict accepts", &rulesModel{Model: agent.NewScriptedModel(), name: strict}, "get_weather-1", true},
		{"gemini accepts a dot", &rulesModel{Model: agent.NewScriptedModel(), name: gemini}, "fs.read", true},
		{"gemini refuses a leading digit", &rulesModel{Model: agent.NewScriptedModel(), name: gemini}, "1tool", false},
		{"through Unwrap", unwrapModel{&rulesModel{Model: agent.NewScriptedModel(), name: strict}}, "fs.read", false},
		{"no rule declared", agent.NewScriptedModel(), "fs.read", true},
		{"nil rule", &rulesModel{Model: agent.NewScriptedModel()}, "get weather", true},
	} {
		_, err := agent.New(tc.model, j, agent.WithTools(named(tc.tool)))
		if tc.ok != (err == nil) || err != nil && !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: Build err = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
	a, err := agent.New(&rulesModel{Model: agent.NewScriptedModel(), name: strict}, j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.With(agent.WithTools(named("b c"))); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("With a bad tool name: %v, want ErrConfig", err)
	}
}

// V1: tool choice "required" on an agent with no tools of its own is left to the run, since
// RunTyped supplies an answer tool. A run with nothing to call fails with ErrConfig before it
// opens the journal or calls the model, when the model declares it needs a tool; RunTyped runs.
func TestRequiredChoice_CheckedAtTheRun(t *testing.T) {
	required := agent.WithToolChoice(agent.ToolChoice{Mode: "required"})
	store := agenttest.MemJournal()
	m := &rulesModel{Model: agent.NewScriptedModel(), required: true}
	a, err := agent.New(m, store, required)
	if err != nil {
		t.Fatalf("Build refused required with no tools: %v", err)
	}
	if _, err := a.Run(context.Background(), "plain", agent.UserText("hi")); !errors.Is(err, agent.ErrConfig) || m.calls != 0 {
		t.Fatalf("Run: err %v after %d model calls, want ErrConfig and none", err, m.calls)
	}
	if recs, _ := store.History(context.Background(), "plain"); len(recs) != 0 {
		t.Fatalf("the refused run journaled %d records", len(recs))
	}
	typed := &rulesModel{Model: agent.NewScriptedModel(agent.ToolTurn("c1", "final_answer", `{"N":7}`)), required: true}
	b, err := agent.New(typed, store, required)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := b.RunTyped[struct{ N int }](context.Background(), "typed", agent.UserText("count"))
	if err != nil || got.N != 7 {
		t.Fatalf("RunTyped under required with no tools of its own: %+v, %v", got, err)
	}
	// A model that does not declare the rule is not second-guessed: the run reaches it.
	free := &rulesModel{Model: agent.NewScriptedModel(agent.TextTurn("ok"))}
	c, err := agent.New(free, store, required)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), "free", agent.UserText("hi")); err != nil || free.calls != 1 {
		t.Fatalf("a model with no rule: err %v after %d calls", err, free.calls)
	}
}

// Suspicion (a) of the review of #127: whether a tool call is in a saga is its own run's flag. A
// plain run started from a saga's tool call (child.Run with a SubRunFor ID) is not a saga, and its
// calls are not in one; the context's saga mode used to be inherited from the call that started
// the run. child.RunSaga is, and a sub-agent called from a saga runs as one.
func TestRunInfoSaga_IsTheRunsOwnFlag(t *testing.T) {
	store := agenttest.MemJournal()
	seen := map[string]bool{}
	probe := func(name string) agent.Tool {
		return agent.MustFunc(name, "", func(ctx context.Context, _ struct{}) (string, error) {
			info, _ := agent.RunInfoFrom(ctx)
			seen[name] = info.Saga
			return "ok", nil
		}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	}
	childAgent := func(tool string) *agent.Agent {
		c, err := agent.New(agent.NewScriptedModel(agent.ToolTurn("k1", tool, `{}`), agent.TextTurn("done")),
			store, agent.WithTools(probe(tool)))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	plainChild, sagaChild, sub := childAgent("in_plain_child"), childAgent("in_saga_child"), childAgent("in_sub_agent")
	starter := agent.MustFunc("starter", "", func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := agent.RunInfoFrom(ctx)
		seen["starter"] = info.Saga
		if _, err := plainChild.Run(ctx, info.SubRunFor("plain"), agent.UserText("go")); err != nil {
			return "", err
		}
		_, err := sagaChild.Run(ctx, info.SubRunFor("saga"), agent.UserText("go"), agent.WithSaga())
		return "ok", err
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	p, err := agent.New(agent.NewScriptedModel(agent.ToolTurn("c1", "starter", `{}`), agent.ToolTurn("c2", "sub", `{"task":"go"}`), agent.TextTurn("done")),
		store, agent.WithTools(starter, agent.MustSubAgent("sub", "", sub)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga()); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"starter": true, "in_plain_child": false, "in_saga_child": true, "in_sub_agent": true}
	for k, v := range want {
		if got, ok := seen[k]; !ok || got != v {
			t.Errorf("%s: RunInfo.Saga = %v (seen %v), want %v", k, got, ok, v)
		}
	}
}
