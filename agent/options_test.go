package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func (t objTool) Spec() ToolSpec              { return t.spec }
func (t objTool) Name() string                { return t.spec.Name }
func (t objTool) Description() string         { return t.spec.Description }
func (t objTool) ArgsSchema() json.RawMessage { return t.spec.Input }
func (t objTool) Safety() Safety              { return t.spec.Safety }
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
	if _, err := mustNew(m, memJournal(), WithTools(namedTool("a"), namedTool("a"))).With(); !errors.Is(err, ErrConfig) {
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
			t.Errorf("ResolveHaltRef(%s) = %v, want ErrConfig", name, err)
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

// WithSystemPrompt and WithSystemPromptFunc fill one slot: the later of the two wins, as options
// and as the transitional builder methods.
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
	old := must(mustNew(
		model(),
		memJournal(),
		WithSystemPromptFunc(func(_ context.Context, _ RunInfo) (string, error) { return "FUNC", nil }),
	).With(WithSystemPrompt("TEXT")))
	if got := systemOf(t, old); got != "TEXT" {
		t.Errorf("builder methods, func then text: system %q, want TEXT", got)
	}
	old = must(mustNew(model(), memJournal(), WithSystemPrompt("TEXT")).With(WithSystemPromptFunc(func(_ context.Context, _ RunInfo) (string, error) { return "FUNC", nil })))
	if got := systemOf(t, old); got != "FUNC" {
		t.Errorf("builder methods, text then func: system %q, want FUNC", got)
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
// derive, configure (with options and with the transitional builder methods) and run copies of
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

// Journal returns the journal Build was given, and for New the journal of its store.
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
		t.Error("Journal() is not the journal Build was given")
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
