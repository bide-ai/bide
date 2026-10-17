package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// objTool is a tool with a Spec method whose spec the test chooses.
type objTool struct{ spec ToolSpec }

func (t objTool) Spec() ToolSpec { return t.spec }
func (objTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`"ok"`), nil
}

// namedTool returns an objTool named name with an object input schema.
func namedTool(name string) objTool {
	return objTool{spec: ToolSpec{Name: name, Input: json.RawMessage(`{"type":"object"}`), Safety: Safety{ReadOnly: true}}}
}

// Every configuration problem Build and With can find is ErrConfig, returned when the agent is
// built: nothing is returned that a run would only then refuse.
func TestBuild_EveryValidationError(t *testing.T) {
	m := NewScriptedModel()
	j := memJournal()
	mofn := &ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}
	gated := namedTool("pay")
	gated.spec.Approval = mofn
	badPolicy := namedTool("pay")
	badPolicy.spec.Approval = &ApprovalPolicy{Need: 3, Approvers: []string{"alice"}}
	shared := func(id string) (ApproverVerifier, bool) {
		return keyVerifier{signer: id, keys: []string{"one-key"}}, true
	}
	distinct := func(id string) (ApproverVerifier, bool) { return keyVerifier{signer: id, keys: []string{id}}, true }
	noSchema, arraySchema, stringType, notJSON := namedTool("a"), namedTool("b"), namedTool("c"), namedTool("d")
	noSchema.spec.Input = nil
	arraySchema.spec.Input = json.RawMessage(`[]`)
	stringType.spec.Input = json.RawMessage(`{"type":"string"}`)
	notJSON.spec.Input = json.RawMessage(`{`)
	sub := buildT(t, NewScriptedModel())
	subTimeout := wrapTool{Tool: MustSubAgent("helper", "", sub), spec: MustSubAgent("helper", "", sub).Spec()}
	subTimeout.spec.Timeout = time.Second

	cases := map[string][]Option{
		"nil option":                     {nil},
		"nil option in WithOptions":      {WithOptions(WithMaxTurns(1), nil)},
		"nil tool":                       {WithTools(nil)},
		"nil typed tool":                 {WithTools((*subAgentTool)(nil))},
		"duplicate tool name":            {WithTools(namedTool("a"), namedTool("a"))},
		"duplicate across options":       {WithTools(namedTool("a")), WithTools(namedTool("a"))},
		"reserved final_answer":          {WithTools(namedTool(finalAnswerTool))},
		"nil input schema":               {WithTools(noSchema)},
		"array input schema":             {WithTools(arraySchema)},
		"non-object schema type":         {WithTools(stringType)},
		"input schema not JSON":          {WithTools(notJSON)},
		"invalid approval policy":        {WithTools(badPolicy), WithApproverVerifiers(distinct)},
		"m-of-n without verifiers":       {WithTools(gated)},
		"m-of-n approvers share a key":   {WithTools(gated), WithApproverVerifiers(shared)},
		"wrapper the agent cannot honor": {WithTools(subTimeout)},
		"negative max turns":             {WithMaxTurns(-1)},
		"negative token budget":          {WithTokenBudget(-1)},
		"negative max concurrency":       {WithMaxConcurrency(-1)},
		"retrieval k below 1":            {WithRetrieval(&fakeRetriever{}, 0)},
		"nil retriever":                  {WithRetrieval(nil, 1)},
		"nil middleware":                 {WithMiddleware(nil)},
		"nil tool middleware":            {WithToolMiddleware(nil)},
		"nil approver verifiers":         {WithApproverVerifiers(nil)},
		"nil tool error redactor":        {WithToolErrorRedactor(nil)},
		"nil system prompt func":         {WithSystemPromptFunc(nil)},
		"nil sampling control":           {WithSampling(Temperature(0), nil)},
		"unknown tool choice mode":       {WithToolChoice(ToolChoice{Mode: "sometimes"})},
		"tool choice tool without name":  {WithToolChoice(ToolChoice{Mode: "tool"})},
		"tool choice name with auto":     {WithTools(namedTool("a")), WithToolChoice(ToolChoice{Mode: "auto", Name: "a"})},
		"forced tool the agent lacks":    {WithTools(namedTool("a")), WithToolChoice(ToolChoice{Mode: "tool", Name: "b"})},
		"nil waker":                      {WithWaker(nil)},
		"nil typed waker":                {WithWaker((*MemWaker)(nil))},
		"empty identity":                 {WithIdentity(Identity{})},
		"nil clock":                      {WithClock(nil)},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			a, err := New(m, j, opts...)
			if !errors.Is(err, ErrConfig) || a != nil {
				t.Fatalf("Build = %v, %v; want nil, ErrConfig", a, err)
			}
			base := buildT(t, m)
			c, err := base.With(opts...)
			if !errors.Is(err, ErrConfig) || c != nil {
				t.Fatalf("With = %v, %v; want nil, ErrConfig", c, err)
			}
		})
	}

	// Build itself.
	if _, err := New(nil, j); !errors.Is(err, ErrConfig) {
		t.Errorf("Build(nil model) = %v, want ErrConfig", err)
	}
	if _, err := New((*ScriptedModel)(nil), j); !errors.Is(err, ErrConfig) {
		t.Errorf("Build(typed nil model) = %v, want ErrConfig", err)
	}
	if _, err := New(m, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("Build(nil journal) = %v, want ErrConfig", err)
	}
	// A tool With adds may not take a name the agent already has.
	if _, err := buildT(t, m, WithTools(namedTool("a"))).With(WithTools(namedTool("a"))); !errors.Is(err, ErrConfig) {
		t.Errorf("With adding a second tool named a = %v, want ErrConfig", err)
	}
	// The forced tool is checked against the tools once every option is applied, in any order.
	if _, err := New(m, j, WithToolChoice(ToolChoice{Mode: "tool", Name: "a"}), WithTools(namedTool("a"))); err != nil {
		t.Errorf("forcing a tool given after the choice: %v", err)
	}
	// The error for an m-of-n policy with no resolver says what is missing.
	if _, err := New(m, j, WithTools(gated)); err == nil || !strings.Contains(err.Error(), "WithApproverVerifiers") {
		t.Errorf("m-of-n without verifiers = %v, want an error naming WithApproverVerifiers", err)
	}
	// An m-of-n policy with distinct keys and verifiers given after the tool builds.
	if _, err := New(m, j, WithTools(gated), WithApproverVerifiers(distinct)); err != nil {
		t.Errorf("m-of-n policy with verifiers: %v", err)
	}
	// An agent New was given a bad tool set cannot be configured further: With returns its error.
	if _, err := New(m, memJournal(), WithTools(namedTool("a"), namedTool("a"))); !errors.Is(err, ErrConfig) {
		t.Errorf("With on an agent New refused tools for = %v, want ErrConfig", err)
	}
}

// wrapTool is a tool that wraps another (Unwrap) and declares its own spec.
type wrapTool struct {
	Tool
	spec ToolSpec
}

func (w wrapTool) Spec() ToolSpec { return w.spec }
func (w wrapTool) Unwrap() Tool   { return w.Tool }

// The options of the other scopes refuse a nil option and a bad value with ErrConfig too.
func TestOptions_OtherScopesValidate(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	task := []Task[int]{{Name: "t", Fn: func(context.Context) (int, error) { return 1, nil }}}
	if _, err := store.Parallel(ctx, "r", task, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("Parallel(nil option) = %v, want ErrConfig", err)
	}
	if _, err := store.Parallel(ctx, "r", task, WithMaxConcurrency(-1)); !errors.Is(err, ErrConfig) {
		t.Errorf("Parallel(WithMaxConcurrency(-1)) = %v, want ErrConfig", err)
	}
	if _, err := store.Step(ctx, "r", "s", func(context.Context) (int, error) { return 1, nil }, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("Step(nil option) = %v, want ErrConfig", err)
	}
	ref := HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1", ToolName: "t"}}
	for name, opt := range map[string]ResolveOption{"nil": nil, "nil clock": WithClock(nil)} {
		if err := ResolveHalt(ctx, store, ref, Outcome{Result: "x"}, opt); !errors.Is(err, ErrConfig) {
			t.Errorf("ResolveHalt(%s) = %v, want ErrConfig", name, err)
		}
	}
	drive := func(context.Context) error { return nil }
	if _, err := Lease(ctx, store, "r", drive, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("Lease(nil option) = %v, want ErrConfig", err)
	}
	if _, err := Recover(ctx, store, func(context.Context, string, RunStart) error { return nil }, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("Recover(nil option) = %v, want ErrConfig", err)
	}
	if err := RecoverLoop(ctx, store, func(context.Context, string, RunStart) error { return nil }, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("RecoverLoop(nil option) = %v, want ErrConfig", err)
	}
	var rc runConfig
	for name, opt := range map[string]RunOption{
		"negative max turns":       WithMaxTurns(-1),
		"negative token budget":    WithTokenBudget(-1),
		"negative max concurrency": WithMaxConcurrency(-1),
		"nil clock":                WithClock(nil),
		"nil waker":                WithWaker(nil),
		"empty identity":           WithIdentity(Identity{}),
		"bad tool choice":          WithToolChoice(ToolChoice{Mode: "x"}),
		"tool choice without name": WithToolChoice(ToolChoice{Mode: "tool"}),
		"nil sampling control":     WithSampling(nil),
	} {
		if err := opt.applyRun(&rc); !errors.Is(err, ErrConfig) {
			t.Errorf("%s as a run option = %v, want ErrConfig", name, err)
		}
	}
}

// A run option records the value the caller chose, so the Run API can apply it over the agent's.
func TestRunOptions_RecordTheirSetting(t *testing.T) {
	var rc runConfig
	now := func() time.Time { return time.Unix(1, 0) }
	w := NewMemWaker(func(context.Context, string) error { return nil })
	for _, o := range []RunOption{
		WithMaxTurns(2), WithTokenBudget(3), WithMaxConcurrency(4), WithSystemPrompt("p"),
		WithSampling(Temperature(0.5)), WithSampling(Seed(7)), WithToolChoice(ToolChoice{Mode: "none"}),
		WithWaker(w), WithIdentity(Identity{Actor: "a"}), WithClock(now),
	} {
		if err := o.applyRun(&rc); err != nil {
			t.Fatal(err)
		}
	}
	// The run's sampling is its own: a control's slice the caller changes later is not.
	stops := []string{"END"}
	var sc runConfig
	if err := WithSampling(Stop(stops...)).applyRun(&sc); err != nil {
		t.Fatal(err)
	}
	stops[0] = "CHANGED"
	if sc.sampling.Stop[0] != "END" {
		t.Errorf("run stop sequences = %v, want [END]", sc.sampling.Stop)
	}
	if *rc.maxTurns != 2 || *rc.tokenBudget != 3 || *rc.maxConc != 4 || *rc.systemPrompt != "p" ||
		*rc.sampling.Temperature != 0.5 || *rc.sampling.Seed != 7 || rc.toolChoice.Mode != "none" ||
		rc.waker != w || rc.identity.Actor != "a" || !rc.clock().Equal(now()) {
		t.Errorf("run config = %+v, want every setting recorded (sampling merged per control)", rc)
	}
}

// loopingModel answers with a tool call on every turn, so a run goes on until its turn cap.
func loopingModel() Model {
	return modelFunc(func(_ context.Context, req Request) (*Stream, error) {
		n := 0
		for _, m := range req.Messages {
			if m.Role == RoleAssistant {
				n++
			}
		}
		return NewScriptedModel(ToolTurn(fmt.Sprintf("c%d", n), "noop", `{}`)).Stream(context.Background(), req)
	})
}

// modelFunc adapts a function to Model.
type modelFunc func(context.Context, Request) (*Stream, error)

func (f modelFunc) Stream(ctx context.Context, req Request) (*Stream, error) { return f(ctx, req) }

// turnsTaken runs a to its turn cap and returns how many turns it took.
func turnsTaken(t *testing.T, a *Agent, runID string) int {
	t.Helper()
	_, err := a.Run(context.Background(), runID, UserText("go"))
	m := turnsRE.FindStringSubmatch(fmt.Sprint(err))
	if !errors.Is(err, ErrMaxTurns) || m == nil {
		t.Fatalf("run %s: %v, want ErrMaxTurns", runID, err)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// turnsRE reads the turn count from an ErrMaxTurns error.
var turnsRE = regexp.MustCompile(`\((\d+) turns\)$`)

// Precedence: for any setting, the last agent-level value given wins, and With's value replaces
// the agent's on the copy only.
func TestPrecedence_LastAgentValueWins(t *testing.T) {
	a := buildT(t, loopingModel(), WithTools(noopTool), WithMaxTurns(1), WithMaxTurns(3))
	if n := turnsTaken(t, a, "r1"); n != 3 {
		t.Errorf("WithMaxTurns(1), WithMaxTurns(3): %d turns, want 3", n)
	}
	b, err := a.With(WithMaxTurns(2))
	if err != nil {
		t.Fatal(err)
	}
	if n := turnsTaken(t, b, "r2"); n != 2 {
		t.Errorf("With(WithMaxTurns(2)): %d turns, want 2", n)
	}
	if n := turnsTaken(t, a, "r3"); n != 3 {
		t.Errorf("the agent With copied: %d turns, want its own 3", n)
	}
	// Grouped options apply where the group appears.
	c := buildT(t, loopingModel(), WithTools(noopTool), WithOptions(WithMaxTurns(4)), WithMaxTurns(1), WithOptions(WithMaxTurns(2)))
	if n := turnsTaken(t, c, "r4"); n != 2 {
		t.Errorf("grouped options: %d turns, want the last (2)", n)
	}
}

// systemOf returns the system prompt the first model request of a run of a was sent.
func systemOf(t *testing.T, a *Agent) string {
	t.Helper()
	var got []string
	c, err := a.With(WithMiddleware(func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			for _, m := range call.Request.Messages {
				if m.Role == RoleSystem {
					got = append(got, m.Text())
				}
			}
			return next(ctx, call)
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), fmt.Sprintf("r%d", runSeq.Add(1)), UserText("hi")); err != nil {
		t.Fatal(err)
	}
	return strings.Join(got, "|")
}

// WithSystemPrompt and WithSystemPromptFunc fill one slot: the later of the two wins, in New and
// in With.
func TestPrecedence_SystemPromptSlot(t *testing.T) {
	fn := func(context.Context, RunInfo) (string, error) { return "FUNC", nil }
	model := func() Model {
		return modelFunc(func(ctx context.Context, req Request) (*Stream, error) {
			return NewScriptedModel(TextTurn("ok")).Stream(ctx, req)
		})
	}
	if got := systemOf(t, buildT(t, model(), WithSystemPrompt("TEXT"), WithSystemPromptFunc(fn))); got != "FUNC" {
		t.Errorf("text then func: system %q, want FUNC", got)
	}
	if got := systemOf(t, buildT(t, model(), WithSystemPromptFunc(fn), WithSystemPrompt("TEXT"))); got != "TEXT" {
		t.Errorf("func then text: system %q, want TEXT", got)
	}
	a := buildT(t, model(), WithSystemPromptFunc(fn))
	b, err := a.With(WithSystemPrompt("TEXT"))
	if err != nil {
		t.Fatal(err)
	}
	if got := systemOf(t, b); got != "TEXT" {
		t.Errorf("With(WithSystemPrompt) over a func: system %q, want TEXT", got)
	}
	if got := systemOf(t, a); got != "FUNC" {
		t.Errorf("the agent With copied: system %q, want its own FUNC", got)
	}
	c, err := buildT(t, model(), WithSystemPrompt("TEXT")).With(WithSystemPromptFunc(fn))
	if err != nil {
		t.Fatal(err)
	}
	if got := systemOf(t, c); got != "FUNC" {
		t.Errorf("With(WithSystemPromptFunc) over a text: system %q, want FUNC", got)
	}
}

// The system prompt function is given the run's RunInfo, and its error fails the drive before the
// model is called.
func TestSystemPromptFunc_RunInfoAndError(t *testing.T) {
	var got RunInfo
	fn := func(_ context.Context, info RunInfo) (string, error) { got = info; return "", nil }
	a := buildT(t, NewScriptedModel(TextTurn("ok")), WithSystemPromptFunc(fn))
	if _, err := a.Run(context.Background(), "r1", UserText("hi"), WithSaga()); err != nil {
		t.Fatal(err)
	}
	if got != (RunInfo{RunID: "r1", RootRunID: "r1", Saga: true}) {
		t.Errorf("RunInfo = %+v, want run r1, its own root, a saga, no tool call", got)
	}
	boom := errors.New("tenant lookup failed")
	m := &countModel{inner: NewScriptedModel(TextTurn("ok"))}
	b := buildT(t, m, WithSystemPromptFunc(func(context.Context, RunInfo) (string, error) { return "", boom }))
	if _, err := b.Run(context.Background(), "r2", UserText("hi")); !errors.Is(err, boom) || !strings.Contains(err.Error(), "r2") {
		t.Errorf("Run = %v, want the function's error, naming the run", err)
	}
	if m.calls.Load() != 0 {
		t.Errorf("the model was called %d times after the prompt function failed", m.calls.Load())
	}
}

// Sampling controls are settings of their own: one option sets some, and a later one leaves the
// rest as they were.
func TestPrecedence_SamplingPerControl(t *testing.T) {
	a := buildT(t, NewScriptedModel(), WithSampling(Temperature(0.2), MaxTokens(100)), WithSampling(Temperature(0.9)))
	if *a.sampling.Temperature != 0.9 || *a.sampling.MaxTokens != 100 {
		t.Errorf("sampling = %+v, want the later temperature and the earlier max tokens", a.sampling)
	}
}

// Identity, Waker and clock: a value bound to the run's context (by the caller, or inherited from
// the run that started it) takes precedence over the agent's, which applies to a run whose
// context carries none, and with neither, there is none (identity, Waker) or time.Now (clock).
func TestPrecedence_RunBeatsAgentBeatsDefault(t *testing.T) {
	type seen struct {
		id    Identity
		idSet bool
		waker Waker
		now   time.Time
	}
	var got seen
	probe := MustFunc("probe", "", func(ctx context.Context, _ struct{}) (string, error) {
		got.id, got.idSet = IdentityFrom(ctx)
		got.waker = wakerFrom(ctx)
		got.now = clockFrom(ctx)()
		return "ok", nil
	}, WithSafety(Safety{ReadOnly: true}))
	run := func(a *Agent, ctx context.Context) seen {
		t.Helper()
		got = seen{}
		c, err := a.With(WithTools(probe))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Run(ctx, fmt.Sprintf("r%d", runSeq.Add(1)), UserText("go")); err != nil {
			t.Fatal(err)
		}
		return got
	}
	model := func() Model { return NewScriptedModel(ToolTurn("c1", "probe", `{}`), TextTurn("done")) }
	agentID, runID := Identity{Actor: "agent"}, Identity{Actor: "run"}
	agentW := NewMemWaker(func(context.Context, string) error { return nil })
	runW := NewMemWaker(func(context.Context, string) error { return nil })
	agentT, runT := time.Unix(100, 0), time.Unix(200, 0)
	configured := func() *Agent {
		return buildT(t, model(), WithIdentity(agentID), WithWaker(agentW), WithClock(func() time.Time { return agentT }))
	}

	// The run's own values win.
	ctx := contextWithClock(contextWithWaker(contextWithIdentity(context.Background(), runID), runW), func() time.Time { return runT })
	if s := run(configured(), ctx); s.id != runID || s.waker != runW || !s.now.Equal(runT) {
		t.Errorf("run values over agent values: %+v, want the run's", s)
	}
	// The agent's apply where the run has none.
	if s := run(configured(), context.Background()); s.id != agentID || s.waker != agentW || !s.now.Equal(agentT) {
		t.Errorf("agent values alone: %+v, want the agent's", s)
	}
	// Neither: the defaults.
	before := time.Now()
	if s := run(buildT(t, model()), context.Background()); s.idSet || s.waker != nil || s.now.Before(before) {
		t.Errorf("no values: %+v, want no identity, no Waker and time.Now", s)
	}
}

// With returns a copy that shares nothing mutable with the agent: under -race, many goroutines
// derive, configure (with options) and run copies of
// one agent while it runs, and each sees exactly its own configuration. The agent's middleware,
// tool middleware and retrieval lists have spare capacity, so a copy that shared their arrays
// would write its additions into a sibling's.
func TestWith_IsolationUnderRace(t *testing.T) {
	stops := []string{"STOP"}
	noMW := func(next ModelHandler) ModelHandler { return next }
	noToolMW := func(next ToolHandler) ToolHandler { return next }
	var none docsRetriever
	parent := buildT(t, NewScriptedModel(ToolTurn("c1", "base", `{}`), TextTurn("ok")), WithTools(namedTool("base")),
		WithSampling(Stop(stops...)), WithMaxTurns(5),
		WithMiddleware(noMW), WithMiddleware(noMW), WithMiddleware(noMW), // one at a time: capacity 4
		WithToolMiddleware(noToolMW), WithToolMiddleware(noToolMW), WithToolMiddleware(noToolMW),
		WithRetrieval(none, 1), WithRetrieval(none, 1), WithRetrieval(none, 1))
	stops[0] = "CHANGED" // the caller's slice is not the agent's
	if cap(parent.mw) == len(parent.mw) || cap(parent.toolMW) == len(parent.toolMW) || cap(parent.retrievals) == len(parent.retrievals) {
		t.Fatalf("setup: the agent's lists have no spare capacity (mw %d/%d, toolMW %d/%d, retrievals %d/%d)",
			len(parent.mw), cap(parent.mw), len(parent.toolMW), cap(parent.toolMW), len(parent.retrievals), cap(parent.retrievals))
	}
	parentTools := slices.Clone(parent.specList)

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 16 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			var seen, seenTool []string
			var sent string
			mark := fmt.Sprintf("mark-%d", i)
			mine := docsRetriever{{Text: mark}}
			child, err := parent.With(
				WithTools(namedTool(fmt.Sprintf("t%d", i))),
				WithMaxTurns(i+10),
				WithSampling(Stop(mark)),
				WithRetrieval(mine, 1),
				WithMiddleware(func(next ModelHandler) ModelHandler {
					return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
						seen = append(seen, mark)
						for _, m := range call.Request.Messages {
							if isContext(m) {
								sent += m.Text()
							}
						}
						return next(ctx, call)
					}
				}),
				WithToolMiddleware(func(next ToolHandler) ToolHandler {
					return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
						seenTool = append(seenTool, mark)
						return next(ctx, call)
					}
				}),
			)
			if err != nil {
				errs <- err
				return
			}
			// With on the copy configures another copy, never the agent or a sibling.
			if child, err = child.With(WithMiddleware(noMW), WithToolMiddleware(noToolMW), WithSystemPrompt(mark)); err != nil {
				errs <- err
				return
			}
			if _, err := child.Run(context.Background(), fmt.Sprintf("c%d", i), UserText("go")); err != nil {
				errs <- err
				return
			}
			switch {
			case len(seen) != 2 || seen[0] != mark || len(seenTool) != 1 || seenTool[0] != mark:
				errs <- fmt.Errorf("child %d's middleware saw %v and %v", i, seen, seenTool)
			case !strings.Contains(sent, mark) || strings.Count(sent, "mark-") != 2: // two calls, one document each
				errs <- fmt.Errorf("child %d's model was sent context %q, want its own document only", i, sent)
			case child.maxTurns != i+10 || len(child.tools) != 2 || len(child.mw) != 5 || len(child.toolMW) != 5 ||
				len(child.retrievals) != 4 || child.systemPrompt != mark:
				errs <- fmt.Errorf("child %d config: turns %d, tools %d, mw %d, toolMW %d, retrievals %d, prompt %q", i,
					child.maxTurns, len(child.tools), len(child.mw), len(child.toolMW), len(child.retrievals), child.systemPrompt)
			case len(child.sampling.Stop) != 1 || child.sampling.Stop[0] != mark:
				errs <- fmt.Errorf("child %d stop = %v", i, child.sampling.Stop)
			}
		}()
		go func() {
			defer wg.Done()
			c, err := parent.With()
			if err != nil {
				errs <- err
				return
			}
			if _, err := c.Run(context.Background(), fmt.Sprintf("p%d", i), UserText("go")); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if parent.maxTurns != 5 || len(parent.tools) != 1 || len(parent.specs) != 1 || len(parent.mw) != 3 || len(parent.toolMW) != 3 ||
		len(parent.retrievals) != 3 || parent.systemPrompt != "" ||
		!slices.EqualFunc(parent.specList, parentTools, func(a, b ToolSpec) bool { return a.Name == b.Name }) {
		t.Errorf("the agent changed: turns %d, tools %d, specs %d, mw %d, toolMW %d, retrievals %d, prompt %q, specList %v",
			parent.maxTurns, len(parent.tools), len(parent.specs), len(parent.mw), len(parent.toolMW), len(parent.retrievals),
			parent.systemPrompt, parent.specList)
	}
	if len(parent.sampling.Stop) != 1 || parent.sampling.Stop[0] != "STOP" {
		t.Errorf("the agent's stop sequences = %v, want [STOP]", parent.sampling.Stop)
	}
}

// A failed With leaves the agent as it was: options are applied to the copy.
func TestWith_FailureLeavesAgentUnchanged(t *testing.T) {
	a := buildT(t, NewScriptedModel(), WithTools(namedTool("a")), WithMaxTurns(2))
	if _, err := a.With(WithMaxTurns(7), WithTools(namedTool("b")), WithTools(namedTool("a"))); !errors.Is(err, ErrConfig) {
		t.Fatalf("With = %v, want ErrConfig", err)
	}
	if a.maxTurns != 2 || len(a.tools) != 1 || len(a.specList) != 1 {
		t.Errorf("a failed With changed the agent: turns %d, tools %d, specList %d", a.maxTurns, len(a.tools), len(a.specList))
	}
}

// Journal returns the journal New was given.
func TestAgent_Journal(t *testing.T) {
	store := NewMemStore()
	j, err := NewJournal(store)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(NewScriptedModel(), j)
	if err != nil {
		t.Fatal(err)
	}
	if a.Journal() != j {
		t.Error("Journal() is not the journal New was given")
	}
	if mustNew(NewScriptedModel(), j).Journal() != j {
		t.Error("Journal() of New over a MemStore is not the store's journal")
	}
}

// Every model middleware sees the request with the retrieved documents in it: retrieval is an
// engine step, not a middleware with a place in the chain.
func TestWithRetrieval_EveryMiddlewareSeesTheDocuments(t *testing.T) {
	var got []string
	r := &fakeRetriever{docs: []Doc{{Text: "doc"}}}
	a := buildT(t, NewScriptedModel(TextTurn("ok")), WithMiddleware(captureRequests(&got)), WithRetrieval(r, 1))
	if _, err := a.Run(context.Background(), "r", UserText("q")); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "doc") {
		t.Errorf("middleware given before WithRetrieval saw context %q, want the documents", got)
	}
}

// docsRetriever is a Retriever that returns fixed documents; it is safe for concurrent use.
type docsRetriever []Doc

func (d docsRetriever) Retrieve(context.Context, string, int) ([]Doc, error) { return d, nil }

// runSeq numbers the runs of tests that need a fresh run ID on a shared journal (a clock's
// resolution is too coarse on some platforms).
var runSeq atomic.Int64

// The shared vocabulary of the claim code and model 1 (docs/design/formal-models.md, section
// 6.2): every journal key the claim protocol writes or reads, and every record kind it stores
// under them, has a kind in the spec, declared in the vocabulary block of
// spec/tla/claims/Claims.tla. The chain: every engine write uses a key constructor
// (TestEngineKeys_WritesUseConstructors); every claim-code constructor is in claimKeyKinds (checked
// against keyConstructors below); every kind in the tables is in the spec block, and back.

// stepNameKey stands for the key of a Step's result, which is the step's name itself, in
// claimKeyKinds.
const stepNameKey = "(the step name)"

// claimKeyKinds maps each key constructor the claim code writes through to the spec's kind of
// record stored under the keys it builds.
var claimKeyKinds = map[string]string{
	"toolAttemptStep":  "marker",
	"stepAttemptStep":  "marker",
	"retryAttemptStep": "retry_marker",
	"nextAttemptStep":  "retry_marker",
	"notStartedStep":   "not_started",
	"ToolResultStep":   "result_tool",
	stepNameKey:        "result_step",
}

// claimRecordKinds maps each record kind the claim code stores to the spec kinds of the keys it
// is stored under.
var claimRecordKinds = map[StepKind][]string{
	StepAttempt:    {"marker", "retry_marker"},
	StepNotStarted: {"not_started"},
	StepToolResult: {"result_tool"},
	StepValue:      {"result_step"},
}

// claimsSpec is model 1's spec, relative to this package.
const claimsSpec = "../spec/tla/claims/Claims.tla"

var recordKindsRE = regexp.MustCompile(`^RecordKinds\s*==\s*\{(.*)\}\s*$`)

// specRecordKinds reads RecordKinds from the vocabulary block of the spec.
func specRecordKinds(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(claimsSpec)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	in, found, blocks := false, 0, 0
	for _, line := range strings.Split(string(data), "\n") {
		switch strings.TrimSpace(line) {
		case `\* vocabulary: begin`:
			in = true
			blocks++
			continue
		case `\* vocabulary: end`:
			in = false
			continue
		}
		if !in {
			continue
		}
		if m := recordKindsRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			found++
			for _, f := range strings.Split(m[1], ",") {
				k, err := strconv.Unquote(strings.TrimSpace(f))
				if err != nil {
					t.Fatalf("%s: RecordKinds element %q is not a string: %v", claimsSpec, f, err)
				}
				kinds = append(kinds, k)
			}
		}
	}
	if blocks != 1 || in || found != 1 {
		t.Fatalf("%s: want one closed vocabulary block holding one RecordKinds line; got %d blocks, %d RecordKinds lines, open=%v", claimsSpec, blocks, found, in)
	}
	slices.Sort(kinds)
	return kinds
}

func sortedSet(xs []string) []string {
	out := slices.Clone(xs)
	slices.Sort(out)
	return slices.Compact(out)
}

// claimKey is a claim-protocol key parsed back into the parts the model names: its kind, the
// first attempt's marker key of the effect (base), the attempt number, and the claim id of a
// not-started record. It is the key parser the trace emitter of milestone M3 is to use.
type claimKey struct {
	kind  string
	base  string // for markers and not-started records: the first attempt's marker key
	gen   int
	claim string // for not-started records
	call  string // for results: the encoded tool-use id or the step name
}

// parseClaimKey parses a key the claim protocol writes, or reports false for any other key.
func parseClaimKey(key string) (claimKey, bool) {
	if rest, ok := strings.CutPrefix(key, notStartedPrefix); ok {
		claim, marker, ok := strings.Cut(rest, ":")
		if !ok || claim == "" {
			return claimKey{}, false
		}
		m, ok := parseClaimKey(marker)
		if !ok || (m.kind != "marker" && m.kind != "retry_marker") {
			return claimKey{}, false
		}
		return claimKey{kind: "not_started", base: m.base, gen: m.gen, claim: claim}, true
	}
	if rest, ok := strings.CutPrefix(key, retryAttemptPrefix); ok {
		n, tail, ok := strings.Cut(rest, ":")
		gen, err := strconv.Atoi(n)
		if !ok || err != nil || gen < 1 || strconv.Itoa(gen) != n {
			return claimKey{}, false
		}
		base, ok := parseClaimKey("attempt:" + tail)
		if !ok || base.kind != "marker" {
			return claimKey{}, false
		}
		return claimKey{kind: "retry_marker", base: base.base, gen: gen}, true
	}
	if strings.HasPrefix(key, "attempt:tool:") || strings.HasPrefix(key, "attempt:step:") {
		return claimKey{kind: "marker", base: key}, true
	}
	if strings.HasPrefix(key, "attempt:") {
		return claimKey{}, false
	}
	if id, ok := strings.CutPrefix(key, "tool:"); ok {
		return claimKey{kind: "result_tool", call: id}, true
	}
	if checkStepName("Step", key) == nil || planNodeStep(key) {
		return claimKey{kind: "result_step", call: key}, true
	}
	return claimKey{}, false
}

func TestProtocolVocabulary(t *testing.T) {
	spec := specRecordKinds(t)

	// The tables and the spec block name the same kinds, in both directions.
	var fromKeys []string
	for _, k := range claimKeyKinds {
		fromKeys = append(fromKeys, k)
	}
	if got := sortedSet(fromKeys); !slices.Equal(got, spec) {
		t.Errorf("key constructors map to kinds %q; %s declares RecordKinds %q", got, claimsSpec, spec)
	}
	var fromRecs []string
	for _, ks := range claimRecordKinds {
		fromRecs = append(fromRecs, ks...)
	}
	if got := sortedSet(fromRecs); !slices.Equal(got, spec) {
		t.Errorf("record kinds map to kinds %q; %s declares RecordKinds %q", got, claimsSpec, spec)
	}

	// Every constructor of an attempt: key is in the table, and every constructor in the table
	// (except the step name) is a real constructor.
	for name, build := range keyConstructors {
		if strings.HasPrefix(build("x"), "attempt:") {
			if _, ok := claimKeyKinds[name]; !ok {
				t.Errorf("key constructor %s builds attempt: keys but has no kind in claimKeyKinds", name)
			}
		}
	}
	for name := range claimKeyKinds {
		if _, ok := keyConstructors[name]; !ok && name != stepNameKey {
			t.Errorf("claimKeyKinds lists %s, which keyConstructors does not", name)
		}
	}

	// Each constructor's keys parse back to the table's kind.
	for name, kind := range claimKeyKinds {
		if name == stepNameKey {
			continue
		}
		for _, s := range adversarialToolUseIDs() {
			key := keyConstructors[name](s)
			got, ok := parseClaimKey(key)
			if !ok || got.kind != kind {
				t.Errorf("%s(%.30q) = %.60q parses as %+v (ok=%v), want kind %s", name, s, key, got, ok, kind)
			}
		}
	}
	for _, name := range []string{"reserve", "a:b", "node:n", "node:iter:2:H:step:N"} {
		if got, ok := parseClaimKey(name); !ok || got.kind != claimKeyKinds[stepNameKey] || got.call != name {
			t.Errorf("step name %q parses as %+v (ok=%v), want result_step", name, got, ok)
		}
	}
}

// The key parser round-trips each constructor's output to the parts it was built from.
func TestProtocolVocabulary_KeysRoundTrip(t *testing.T) {
	claim := newClaimID()
	for _, s := range adversarialToolUseIDs() {
		for _, base := range []string{toolAttemptStep(s), stepAttemptStep(s)} {
			for gen := range 4 {
				key := retryAttemptStep(base, gen)
				want := claimKey{kind: "marker", base: base}
				if gen > 0 {
					want = claimKey{kind: "retry_marker", base: base, gen: gen}
				}
				if got, ok := parseClaimKey(key); !ok || got != want {
					t.Fatalf("parse %.60q = %+v (ok=%v), want %+v", key, got, ok, want)
				}
				next, _ := parseClaimKey(nextAttemptStep(key))
				if next.kind != "retry_marker" || next.base != base || next.gen != gen+1 {
					t.Fatalf("parse nextAttemptStep(%.60q) = %+v, want attempt %d of %.60q", key, next, gen+1, base)
				}
				ns := notStartedStep(key, claim)
				wantNS := claimKey{kind: "not_started", base: base, gen: gen, claim: claim}
				if got, ok := parseClaimKey(ns); !ok || got != wantNS {
					t.Fatalf("parse %.60q = %+v (ok=%v), want %+v", ns, got, ok, wantNS)
				}
			}
		}
		key := ToolResultStep(s)
		if got, ok := parseClaimKey(key); !ok || got != (claimKey{kind: "result_tool", call: encodeID(s)}) {
			t.Fatalf("parse %.60q = %+v (ok=%v)", key, got, ok)
		}
	}
	for _, bad := range []string{"attempt:", "attempt:retry:0:tool:x", "attempt:retry:01:tool:x", "attempt:retry:x:tool:x",
		"attempt:retry:1:retry:1:tool:x", "attempt:not-started::attempt:tool:x", "attempt:not-started:c:tool:x", "attempt:other:x", "run:complete"} {
		if got, ok := parseClaimKey(bad); ok {
			t.Errorf("parse %q = %+v, want not a claim key", bad, got)
		}
	}
}

// doneCause is the cancellation cause when there is one, and DeadlineExceeded for a context whose
// deadline has passed but whose timer has not run yet (no cause recorded).
func TestDoneCause(t *testing.T) {
	why := errors.New("operator stop")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(why)
	if got := doneCause(ctx); !errors.Is(got, why) {
		t.Errorf("doneCause(cancelled with a cause) = %v; want the cause", got)
	}
	if got := doneCause(context.Background()); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("doneCause(no cause) = %v; want DeadlineExceeded", got)
	}
}

type covRules struct{ Model }

func (covRules) ToolNameRule() *regexp.Regexp   { return regexp.MustCompile(`^[a-z]+$`) }
func (covRules) RequiresToolsForRequired() bool { return true }

type covModelWrapper struct{ inner Model }

func (w covModelWrapper) Stream(ctx context.Context, r Request) (*Stream, error) {
	return w.inner.Stream(ctx, r)
}
func (w covModelWrapper) Unwrap() Model { return w.inner }

// The tool rules a model declares are found through its wrappers, and a nil model, a model that
// declares none, and a wrapper that unwraps to itself declare none rather than looping.
func TestToolRulesOf(t *testing.T) {
	base := covRules{&scriptModel{}}
	if r, ok := toolRulesOf(covModelWrapper{covModelWrapper{base}}); !ok || !r.RequiresToolsForRequired() {
		t.Errorf("rules behind two wrappers = %v, %v", r, ok)
	}
	if _, ok := toolRulesOf(nil); ok {
		t.Error("a nil model declared rules")
	}
	if _, ok := toolRulesOf(&scriptModel{}); ok {
		t.Error("a model with none declared rules")
	}
	var self covSelfWrapper
	self.inner = &self
	if _, ok := toolRulesOf(self); ok {
		t.Error("a self-wrapping model declared rules")
	}
	// New enforces the declared name rule: a tool whose name the provider would refuse is ErrConfig.
	bad := &countingTool{name: "Bad_Name", calls: new(int)}
	if _, err := New(base, memJournal(), WithTools(bad)); !errors.Is(err, ErrConfig) {
		t.Errorf("New with a tool name the model refuses = %v; want ErrConfig", err)
	}
}

type covSelfWrapper struct{ inner Model }

func (w covSelfWrapper) Stream(ctx context.Context, r Request) (*Stream, error) {
	return w.inner.Stream(ctx, r)
}
func (w covSelfWrapper) Unwrap() Model { return w.inner }

// An agent with no tools offers the model none.
func TestRequestTools_NoToolsOffersNone(t *testing.T) {
	a := mustNew(&scriptModel{}, memJournal())
	if got := a.requestTools(); got != nil {
		t.Errorf("requestTools = %v; want nil", got)
	}
}

// A decoded record carries a ModelTurn / ApproverSignature although its journal encoding holds none,
// when the bytes name a moved member with an empty value. The PR says a decoded record carries one
// exactly when its encoding does.
func TestR140_DecodedEmptySubStruct(t *testing.T) {
	for _, in := range []string{
		`{"name":"a","kind":"model","finish":""}`,
		`{"name":"a","kind":"model","model":null}`,
		`{"name":"a","kind":"approval","approver":""}`,
		`{"name":"a","kind":"approval","signature":null}`,
	} {
		r, err := DecodeRecord([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		enc, _ := EncodeRecord(r)
		back, _ := DecodeRecord(enc)
		back.raw, r.raw = nil, nil
		if !reflect.DeepEqual(r, back) {
			t.Errorf("%s: decoded ModelTurn=%v ApproverSignature=%v, but its encoding %s decodes to ModelTurn=%v ApproverSignature=%v",
				in, r.ModelTurn, r.ApproverSignature, enc, back.ModelTurn, back.ApproverSignature)
		}
	}
}

// Do and History hand back records that share no sub-struct with the record fn returned or with
// each other, so a caller replacing nothing but writing through the pointer cannot alter the journal.
func TestR140_StoreCopiesSubStructs(t *testing.T) {
	ctx := context.Background()
	m := memJournal()
	built := Record{Kind: StepModel, ModelTurn: &ModelTurn{Finish: "stop"}}
	live, err := m.do(ctx, "r", "s", func(context.Context) (Record, error) { return built, nil })
	if err != nil {
		t.Fatal(err)
	}
	if live.ModelTurn == built.ModelTurn {
		t.Fatal("Do returned the built ModelTurn pointer")
	}
	live.ModelTurn.Finish = "X"
	h, _ := m.History(ctx, "r")
	if h[len(h)-1].Finish() != "stop" {
		t.Fatalf("History reads %q after a caller wrote through Do's ModelTurn", h[len(h)-1].Finish())
	}
	h[len(h)-1].ModelTurn.Finish = "Y"
	h2, _ := m.History(ctx, "r")
	memo, _ := m.do(ctx, "r", "s", func(context.Context) (Record, error) { t.Fatal("ran"); return Record{}, nil })
	if h2[len(h2)-1].Finish() != "stop" || memo.Finish() != "stop" || built.Finish() != "stop" {
		t.Fatalf("aliasing: history %q memo %q built %q", h2[len(h2)-1].Finish(), memo.Finish(), built.Finish())
	}
}
