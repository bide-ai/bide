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

// staticRetriever returns fixed documents.
type staticRetriever []agent.Doc

func (r staticRetriever) Retrieve(context.Context, string, int) ([]agent.Doc, error) { return r, nil }

// WithRetrieval's step costs one step per run (its probes and its write), however many model calls the run
// makes: a drive reads the documents once, at its first model call, and its later calls reuse them.
func TestWithRetrieval_OneStepPerDrive(t *testing.T) {
	j, cs, _ := countingJournal(t)
	noop := agent.MustFunc("noop", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
	m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "noop", `{}`), agenttest.ToolTurn("c2", "noop", `{}`), agenttest.TextTurn("done"))
	a, err := agent.New(m, j, agent.WithTools(noop), agent.WithRetrieval(staticRetriever{{Text: "doc"}}, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r", agent.UserText("q")); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range cs.Counts().Names {
		if strings.HasSuffix(n, "@retrieval/0") {
			got = append(got, n)
		}
	}
	if want := []string{"get @retrieval/0", "get attempt:step:@retrieval/0", "insert @retrieval/0"}; !slices.Equal(got, want) {
		t.Errorf("retrieval round trips over three model calls = %v, want %v", got, want)
	}
}

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
	a, err := agent.New(agenttest.NewScriptedModel(agenttest.TextTurn("done")), store, retry, agent.WithRetrieval(r, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "plain", agent.UserText("q")); err == nil || r.calls != 1 {
		t.Fatalf("under Retry middleware only: err %v after %d retriever calls; want the retrieval error after 1", err, r.calls)
	}

	// WithRetrievalRetry(2): two failures, then the documents, recorded once.
	r = &flakyRetriever{fail: 2}
	store = agenttest.MemJournal()
	a, err = agent.New(agenttest.NewScriptedModel(agenttest.TextTurn("done")), store,
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
	a, err = agent.New(agenttest.NewScriptedModel(agenttest.TextTurn("done")), store,
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
	a, err := agent.New(agenttest.NewScriptedModel(agenttest.TextTurn("done")), agenttest.MemJournal(),
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
	a, err = agent.New(agenttest.NewScriptedModel(agenttest.TextTurn("done")), agenttest.MemJournal(),
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
		if _, err := agent.New(agenttest.NewScriptedModel(), agenttest.MemJournal(),
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
	a, err := agent.New(agenttest.NewScriptedModel(agenttest.TextTurn("done")), store,
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
	a, err = agent.New(agenttest.NewScriptedModel(agenttest.TextTurn("done")), store, agent.WithRetrieval(gated, 1))
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
	a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "noop", `{}`), agenttest.TextTurn("answer")), store,
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
	m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "ask", `{}`), agenttest.TextTurn("done"))
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
		{"strict refuses a space", &rulesModel{Model: agenttest.NewScriptedModel(), name: strict}, "get weather", false},
		{"strict refuses a dot", &rulesModel{Model: agenttest.NewScriptedModel(), name: strict}, "fs.read", false},
		{"strict refuses 65 chars", &rulesModel{Model: agenttest.NewScriptedModel(), name: strict}, strings.Repeat("a", 65), false},
		{"strict accepts", &rulesModel{Model: agenttest.NewScriptedModel(), name: strict}, "get_weather-1", true},
		{"gemini accepts a dot", &rulesModel{Model: agenttest.NewScriptedModel(), name: gemini}, "fs.read", true},
		{"gemini refuses a leading digit", &rulesModel{Model: agenttest.NewScriptedModel(), name: gemini}, "1tool", false},
		{"through Unwrap", unwrapModel{&rulesModel{Model: agenttest.NewScriptedModel(), name: strict}}, "fs.read", false},
		{"no rule declared", agenttest.NewScriptedModel(), "fs.read", true},
		{"nil rule", &rulesModel{Model: agenttest.NewScriptedModel()}, "get weather", true},
	} {
		_, err := agent.New(tc.model, j, agent.WithTools(named(tc.tool)))
		if tc.ok != (err == nil) || err != nil && !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: Build err = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
	a, err := agent.New(&rulesModel{Model: agenttest.NewScriptedModel(), name: strict}, j)
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
	m := &rulesModel{Model: agenttest.NewScriptedModel(), required: true}
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
	typed := &rulesModel{Model: agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "final_answer", `{"N":7}`)), required: true}
	b, err := agent.New(typed, store, required)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := b.RunTyped[struct{ N int }](context.Background(), "typed", agent.UserText("count"))
	if err != nil || got.N != 7 {
		t.Fatalf("RunTyped under required with no tools of its own: %+v, %v", got, err)
	}
	// A model that does not declare the rule is not second-guessed: the run reaches it.
	free := &rulesModel{Model: agenttest.NewScriptedModel(agenttest.TextTurn("ok"))}
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
// the run. child.saga Run is, and a sub-agent called from a saga runs as one.
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
		c, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("k1", tool, `{}`), agenttest.TextTurn("done")),
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
	p, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "starter", `{}`), agenttest.ToolTurn("c2", "sub", `{"task":"go"}`), agenttest.TextTurn("done")),
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

// adv127b builds: child agent (one compensable write "book") on childStore; parent saga on
// parentStore whose tool "starter" (call id starterID) runs the child as SubRunFor("child") with
// saga Run, then "boom" fails the saga.
func adv127b(t *testing.T, parentStore, childStore *agent.Journal, starterID string, declare bool, undone *int) *agent.Agent {
	t.Helper()
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { *undone++; return nil })
	child, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("k1", "book", `{}`), agenttest.TextTurn("child done")),
		childStore, agent.WithTools(book))
	if err != nil {
		t.Fatal(err)
	}
	var opts []agent.ToolOption
	if declare {
		opts = append(opts, agent.WithSubRuns(func(string) *agent.Agent { return child }))
	}
	starter := agent.MustFunc("starter", "", func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := agent.RunInfoFrom(ctx)
		res, err := child.Run(ctx, info.SubRunFor("child"), agent.UserText("work"), agent.WithSaga())
		var msg agent.Message
		if res != nil {
			msg = res.Message
		}
		return msg.Text(), err
	}, append([]agent.ToolOption{agent.WithSafety(agent.Safety{ReadOnly: true})}, opts...)...)
	boom := agent.MustFunc("boom", "", func(context.Context, struct{}) (string, error) {
		return "", errors.New("boom")
	})
	p, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn(starterID, "starter", `{}`), agenttest.ToolTurn("c2", "boom", `{}`), agenttest.TextTurn("x")),
		parentStore, agent.WithTools(starter, boom))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func adv127bAbort(t *testing.T, err error) *agent.SagaAborted {
	t.Helper()
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	return ab
}

// B1: a tool-use ID whose encoding is a digest (over 96 escaped bytes). The link is written under
// "@subrun/~<sha>/child", but subRunLinks decodes the tool-use segment and skips digests, so the
// rollback never walks the child: its write is neither compensated nor listed.
func TestAdv127b_LongToolUseIDLinkIgnored(t *testing.T) {
	for _, id := range []string{"c1", strings.Repeat("a", 97), strings.Repeat("|", 33)} {
		var undone int
		s := agenttest.MemJournal()
		_, err := adv127b(t, s, s, id, true, &undone).Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
		ab := adv127bAbort(t, err)
		if undone != 1 || !slices.Contains(ab.Compensated, "book") {
			t.Errorf("tool-use id len %d: undone %d, compensated %v, uncompensated %v, unknown %v; want book compensated",
				len(id), undone, ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
		}
	}
}

// B2 (decided): a saga's programmatic sub-run must journal to the parent's store, where the
// rollback reads it. A child agent on its own store is refused (ErrConfig) before it records or
// runs anything, so no write of it can be missed.
func TestAdv127b_ChildOnOwnStoreRefused(t *testing.T) {
	for _, declare := range []bool{false, true} {
		var undone int
		ps, cs := agenttest.MemJournal(), agent.NewMemStore()
		_, err := adv127b(t, ps, agenttest.MustJournal(cs), "c1", declare, &undone).Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
		ab := adv127bAbort(t, err)
		if !strings.Contains(ab.Cause.Error(), "another store") {
			t.Errorf("declare %v: abort cause %v, want the child refused for its store", declare, ab.Cause)
		}
		for id, lerr := range cs.Runs(context.Background(), agent.RunFilter{}) {
			t.Errorf("declare %v: the child's store holds run %q (%v); want none", declare, id, lerr)
		}
		if undone != 0 {
			t.Errorf("declare %v: undone %d, want 0 (the child never ran)", declare, undone)
		}
	}
}

// B2 (decided): at rollback, an agent from WithSubRuns that journals to another store than the
// run's cannot be used: the call is listed in Uncompensated with the reason, and the sub-run's
// writes are listed from its journal. A panicking WithSubRuns function is listed the same way.
func TestAdv127b_UnusableDeclarationListed(t *testing.T) {
	for _, mode := range []string{"other store", "panic"} {
		s := agenttest.MemJournal()
		book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
			func(context.Context, struct{}, string) error { t.Error("compensated by an unusable agent"); return nil })
		child, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("k1", "book", `{}`), agenttest.TextTurn("done")), s, agent.WithTools(book))
		if err != nil {
			t.Fatal(err)
		}
		elsewhere, err := agent.New(agenttest.NewScriptedModel(), agenttest.MemJournal(), agent.WithTools(book))
		if err != nil {
			t.Fatal(err)
		}
		declared := func(string) *agent.Agent {
			if mode == "panic" {
				panic("tenant lookup failed")
			}
			return elsewhere
		}
		starter := agent.MustFunc("starter", "", func(ctx context.Context, _ struct{}) (string, error) {
			info, _ := agent.RunInfoFrom(ctx)
			res, err := child.Run(ctx, info.SubRunFor("child"), agent.UserText("work"), agent.WithSaga())
			var msg agent.Message
			if res != nil {
				msg = res.Message
			}
			return msg.Text(), err
		}, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithSubRuns(declared))
		boom := agent.MustFunc("boom", "", func(context.Context, struct{}) (string, error) { return "", errors.New("boom") })
		p, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "starter", `{}`), agenttest.ToolTurn("c2", "boom", `{}`), agenttest.TextTurn("x")),
			s, agent.WithTools(starter, boom))
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
		ab := adv127bAbort(t, err)
		want := map[string]string{"other store": "another store", "panic": "tenant lookup failed"}[mode]
		listed := slices.ContainsFunc(ab.Uncompensated, func(u string) bool {
			return strings.HasPrefix(u, "starter (") && strings.Contains(u, want)
		})
		if !listed || !slices.Contains(ab.Uncompensated, "book") {
			t.Errorf("%s: uncompensated %v; want the starter call with its reason and the child's book", mode, ab.Uncompensated)
		}
	}
}

// B3: nesting through a non-saga intermediate. parent saga -> starter runs mid with Run (not
// saga Run) -> mid's tool runs leaf with saga Run as SubRunFor("leaf") -> leaf books. The parent's
// rollback walks mid's journal (and would undo mid's own writes), but mid's call context has
// saga=false, so no link to leaf was written: leaf's book is neither compensated nor listed.
// midSaga=true is the control.
func TestAdv127b_NestedThroughNonSagaChild(t *testing.T) {
	for _, midSaga := range []bool{true, false} {
		s := agenttest.MemJournal()
		var undone int
		book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
			func(context.Context, struct{}, string) error { undone++; return nil })
		leaf, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("k1", "book", `{}`), agenttest.TextTurn("leaf done")),
			s, agent.WithTools(book))
		if err != nil {
			t.Fatal(err)
		}
		spawn := agent.MustFunc("spawn", "", func(ctx context.Context, _ struct{}) (string, error) {
			info, _ := agent.RunInfoFrom(ctx)
			res, err := leaf.Run(ctx, info.SubRunFor("leaf"), agent.UserText("work"), agent.WithSaga())
			var msg agent.Message
			if res != nil {
				msg = res.Message
			}
			return msg.Text(), err
		}, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithSubRuns(func(string) *agent.Agent { return leaf }))
		mid, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("m1", "spawn", `{}`), agenttest.TextTurn("mid done")),
			s, agent.WithTools(spawn))
		if err != nil {
			t.Fatal(err)
		}
		starter := agent.MustFunc("starter", "", func(ctx context.Context, _ struct{}) (string, error) {
			info, _ := agent.RunInfoFrom(ctx)
			var opts []agent.RunOption
			if midSaga {
				opts = append(opts, agent.WithSaga())
			}
			msg, err := agenttest.Answer(mid.Run(ctx, info.SubRunFor("mid"), agent.UserText("work"), opts...))
			return msg.Text(), err
		}, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithSubRuns(func(string) *agent.Agent { return mid }))
		boom := agent.MustFunc("boom", "", func(context.Context, struct{}) (string, error) {
			return "", errors.New("boom")
		})
		p, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "starter", `{}`), agenttest.ToolTurn("c2", "boom", `{}`), agenttest.TextTurn("x")),
			s, agent.WithTools(starter, boom))
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
		ab := adv127bAbort(t, err)
		if undone != 1 && !slices.Contains(ab.Uncompensated, "book") {
			t.Errorf("mid as saga %v: undone %d, compensated %v, uncompensated %v, unknown %v; leaf's book is listed nowhere",
				midSaga, undone, ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
		}
	}
}

// B3, deeper: a saga's tree is inherited through any number of plain runs. root (saga) -> a
// (plain) -> b (plain) -> leaf (saga) books; the root's rollback reaches leaf through two plain
// runs' links and undoes the booking.
func TestAdv127b_NestedThroughTwoPlainRuns(t *testing.T) {
	s := agenttest.MemJournal()
	undone := 0
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { undone++; return nil })
	build := func(m agent.Model, tools ...agent.Tool) *agent.Agent {
		a, err := agent.New(m, s, agent.WithTools(tools...))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	starts := func(tool, name string, sub *agent.Agent, saga bool) agent.Tool {
		return agent.MustFunc(tool, "", func(ctx context.Context, _ struct{}) (string, error) {
			info, _ := agent.RunInfoFrom(ctx)
			var opts []agent.RunOption
			if saga {
				opts = append(opts, agent.WithSaga())
			}
			msg, err := agenttest.Answer(sub.Run(ctx, info.SubRunFor(name), agent.UserText("work"), opts...))
			return msg.Text(), err
		}, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithSubRuns(func(string) *agent.Agent { return sub }))
	}
	leaf := build(agenttest.NewScriptedModel(agenttest.ToolTurn("k1", "book", `{}`), agenttest.TextTurn("leaf")), book)
	b := build(agenttest.NewScriptedModel(agenttest.ToolTurn("b1", "to_leaf", `{}`), agenttest.TextTurn("b")), starts("to_leaf", "leaf", leaf, true))
	a := build(agenttest.NewScriptedModel(agenttest.ToolTurn("a1", "to_b", `{}`), agenttest.TextTurn("a")), starts("to_b", "b", b, false))
	boom := agent.MustFunc("boom", "", func(context.Context, struct{}) (string, error) { return "", errors.New("boom") })
	root := build(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "to_a", `{}`), agenttest.ToolTurn("c2", "boom", `{}`), agenttest.TextTurn("x")),
		starts("to_a", "a", a, false), boom)
	_, err := root.Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
	ab := adv127bAbort(t, err)
	if undone != 1 || !slices.Contains(ab.Compensated, "book") {
		t.Errorf("undone %d, compensated %v, uncompensated %v; want leaf's book undone", undone, ab.Compensated, ab.Uncompensated)
	}
}
