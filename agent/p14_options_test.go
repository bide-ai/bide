package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// The P14 contract's journaling rules (rules 8 to 13), the principal, typed runs, image input and
// a Result on every error kind.

func ptr[T any](v T) *T { return &v }

// systemText is the text of the system message a request starts with, if any.
func systemText(req agent.Request) string {
	if len(req.Messages) > 0 && req.Messages[0].Role == agent.RoleSystem {
		return req.Messages[0].Text()
	}
	return ""
}

// racingStore inserts other's run:start just before the first Insert of run:start it is asked
// for, as a concurrent first drive would.
type racingStore struct {
	agent.Store
	once  sync.Once
	other []byte
}

func (s *racingStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if name == "run:start" {
		s.once.Do(func() { _, _, _ = s.Store.Insert(ctx, runID, name, s.other) })
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// Rule 8: the first drive inserts run:start with its caller's options and runs under the stored
// entry the insert returns. Here a concurrent first drive's run:start lands first, with a system
// prompt and a one-turn limit; this drive passed neither, and runs under them.
func TestP14Rule08_FirstDriveRunsUnderStoredStart(t *testing.T) {
	ctx := context.Background()
	other := agent.RunStart{Input: agent.UserText("go"), Settings: agent.RunSettings{MaxTurns: ptr(1), SystemPrompt: ptr("theirs")}}
	b, _ := json.Marshal(other)
	data, err := agent.JournalEntry("run:start", agent.Record{Kind: agent.StepValue, Result: b})
	if err != nil {
		t.Fatal(err)
	}
	rs := &racingStore{Store: agent.NewMemStore(), other: data}
	j, err := agent.NewJournal(rs)
	if err != nil {
		t.Fatal(err)
	}
	var c counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "lookup")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(c.tool("lookup", agent.Safety{ReadOnly: true})), agent.WithSystemPrompt("mine"))
	_, err = a.Run(ctx, "r", agent.UserText("go"))
	if !errors.Is(err, agent.ErrMaxTurns) {
		t.Fatalf("run = %v, want ErrMaxTurns under the stored one-turn limit", err)
	}
	if got := systemText(model.lastReq(t)); got != "theirs" {
		t.Fatalf("system prompt = %q, want the stored run:start's", got)
	}
}

// approvalAgent builds an agent whose model calls pay (gated by an approval), then lookup, then
// lookup again, then answers; it returns the agent, its model and the pay counter.
func approvalAgent(t *testing.T, j *agent.Journal, opts ...agent.Option) (*agent.Agent, *p14Model, *counter) {
	t.Helper()
	var c counter
	model := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "pay")}},
		{calls: []agent.ToolUse{call("c2", "lookup")}},
		{calls: []agent.ToolUse{call("c3", "lookup")}},
		{text: "done"},
	}}
	tools := agent.WithTools(c.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval())),
		agent.MustFunc("lookup", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true})))
	return p14Build(t, model, j, append([]agent.Option{tools}, opts...)...), model, &c
}

// pauseThenApprove runs a's first drive of "r" with opts to its approval pause and approves.
func pauseThenApprove(t *testing.T, a *agent.Agent, j *agent.Journal, opts ...agent.RunOption) {
	t.Helper()
	ctx := context.Background()
	if _, err := a.Run(ctx, "r", agent.UserText("go"), opts...); err == nil {
		t.Fatal("the first drive did not pause")
	} else if _, ok := errors.AsType[*agent.ApprovalPending](err); !ok {
		t.Fatalf("first drive = %v, want the approval pause", err)
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
}

// Rule 9: every later drive runs under run:start's options, whatever the agent's defaults are and
// whatever entry point resumes it.
func TestP14Rule09_LaterDrivesRunUnderJournaledOptions(t *testing.T) {
	for _, entry := range []string{"Resume", "Run"} {
		t.Run(entry, func(t *testing.T) {
			ctx := context.Background()
			j, _ := p14Journal(t)
			a, model, _ := approvalAgent(t, j, agent.WithSystemPrompt("agent"), agent.WithMaxTurns(10), agent.WithSampling(agent.Temperature(0.9)))
			pauseThenApprove(t, a, j, agent.WithSystemPrompt("per-run"), agent.WithMaxTurns(2), agent.WithSampling(agent.Temperature(0.3)))
			var err error
			if entry == "Resume" {
				_, err = a.Resume(ctx, "r")
			} else {
				_, err = a.Run(ctx, "r", agent.UserText("go"))
			}
			if !errors.Is(err, agent.ErrMaxTurns) {
				t.Fatalf("resume = %v, want ErrMaxTurns under the journaled limit of 2", err)
			}
			req := model.lastReq(t)
			if got := systemText(req); got != "per-run" {
				t.Fatalf("system prompt = %q, want the journaled one", got)
			}
			if req.Sampling.Temperature == nil || *req.Sampling.Temperature != 0.3 {
				t.Fatalf("temperature = %v, want the journaled 0.3", req.Sampling.Temperature)
			}
		})
	}
}

// Resume of a run with no run:start is ErrNotStarted, and drives nothing.
func TestP14Rule09_ResumeOfUnstartedRun(t *testing.T) {
	j, _ := p14Journal(t)
	model := &p14Model{turns: []p14Turn{{text: "done"}}}
	a := p14Build(t, model, j)
	res, err := a.Resume(context.Background(), "nope")
	if !errors.Is(err, agent.ErrNotStarted) || res == nil || model.calls.Load() != 0 {
		t.Fatalf("Resume = %v, %v (model calls %d); want ErrNotStarted with a Result", res, err, model.calls.Load())
	}
}

// Rule 10: a later drive with a different limit writes run:limits:<n> before it drives and runs
// under it, and the drives after it run under the amendment.
func TestP14Rule10_LimitAmendment(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	model := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "lookup")}},
		{calls: []agent.ToolUse{call("c2", "lookup")}},
		{calls: []agent.ToolUse{call("c3", "lookup")}},
		{text: "done"},
	}}
	var c counter
	a := p14Build(t, model, j, agent.WithTools(c.tool("lookup", agent.Safety{ReadOnly: true})))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithMaxTurns(1)); !errors.Is(err, agent.ErrMaxTurns) {
		t.Fatalf("first drive = %v, want ErrMaxTurns", err)
	}
	if _, err := a.Resume(ctx, "r", agent.WithMaxTurns(2)); !errors.Is(err, agent.ErrMaxTurns) {
		t.Fatalf("raised to 2 = %v, want ErrMaxTurns at 2", err)
	}
	if !has(t, m, "r", "run:limits:0") {
		t.Fatal("the raise was not journaled as run:limits:0")
	}
	// A drive that passes nothing runs under the amendment, not the agent's unbounded default.
	if _, err := a.Resume(ctx, "r"); !errors.Is(err, agent.ErrMaxTurns) {
		t.Fatalf("drive with no option = %v, want ErrMaxTurns under the amendment", err)
	}
	if n := model.calls.Load(); n != 2 {
		t.Fatalf("model calls = %d, want 2", n)
	}
	res, err := a.Resume(ctx, "r", agent.WithMaxTurns(4))
	if err != nil || res.Message.Text() != "done" {
		t.Fatalf("raised to 4 = %v, %v; want the answer", res, err)
	}
	if !has(t, m, "r", "run:limits:1") {
		t.Fatal("the second raise was not journaled as run:limits:1")
	}
	// The same limit is no amendment.
	if _, err := a.Resume(ctx, "r", agent.WithMaxTurns(4)); err != nil || has(t, m, "r", "run:limits:2") {
		t.Fatalf("repeat of the limit = %v, or it wrote an amendment", err)
	}
}

// Rule 10 for the token budget: an amendment raises a budget the run spent.
func TestP14Rule10_TokenBudgetAmendment(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	model := &usageModel{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "lookup")}},
		{text: "done"},
	}}
	var c counter
	a := p14Build(t, model, j, agent.WithTools(c.tool("lookup", agent.Safety{ReadOnly: true})))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithTokenBudget(10)); !errors.Is(err, agent.ErrBudgetExceeded) {
		t.Fatalf("first drive = %v, want ErrBudgetExceeded", err)
	}
	if _, err := a.Resume(ctx, "r"); !errors.Is(err, agent.ErrBudgetExceeded) {
		t.Fatalf("drive with no option = %v, want the journaled budget", err)
	}
	res, err := a.Resume(ctx, "r", agent.WithTokenBudget(1000))
	if err != nil || res.Message.Text() != "done" || !has(t, m, "r", "run:limits:0") {
		t.Fatalf("raised budget = %v, %v; want the answer and run:limits:0", res, err)
	}
}

// usageModel is a p14Model whose every turn reports 20 tokens.
type usageModel struct {
	turns []p14Turn
	m     p14Model
}

func (u *usageModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	u.m.turns = u.turns
	s, err := u.m.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan agent.Emit, 8)
	go func() {
		defer close(ch)
		for e, err := range s.Events() {
			if err != nil {
				ch <- agent.Emit{Err: err}
				return
			}
			if f, ok := e.(agent.Finish); ok {
				f.Usage = agent.Usage{InputTokens: 10, OutputTokens: 10}
				e = f
			}
			ch <- agent.Emit{Event: e}
		}
	}()
	return agent.NewStream(ch), nil
}

// Rule 11: a later drive with any other different setting is ErrConfig, before any model call.
func TestP14Rule11_OtherMismatchesAreErrConfig(t *testing.T) {
	first := []agent.RunOption{
		agent.WithSystemPrompt("P"), agent.WithSampling(agent.Temperature(0.3)),
		agent.WithToolChoice(agent.ToolChoice{Mode: "auto"}), agent.WithToolFilter("pay", "lookup"),
		agent.WithIdentity(agent.Identity{Actor: "a1", OnBehalfOf: "desk", AuthorityRef: "g1"}),
	}
	for _, tc := range []struct {
		name  string
		input string
		opts  []agent.RunOption
	}{
		{"input", "other", nil},
		{"system prompt", "go", []agent.RunOption{agent.WithSystemPrompt("Q")}},
		{"sampling", "go", []agent.RunOption{agent.WithSampling(agent.Temperature(0.4))}},
		{"tool choice", "go", []agent.RunOption{agent.WithToolChoice(agent.ToolChoice{Mode: "none"})}},
		{"tool filter", "go", []agent.RunOption{agent.WithToolFilter("lookup")}},
		{"principal", "go", []agent.RunOption{agent.WithIdentity(agent.Identity{OnBehalfOf: "other desk"})}},
		{"saga", "go", []agent.RunOption{agent.WithSaga()}},
		{"output mode on an untyped run", "go", []agent.RunOption{agent.WithOutputMode(agent.OutputNative)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			j, _ := p14Journal(t)
			a, model, pay := approvalAgent(t, j)
			pauseThenApprove(t, a, j, first...)
			before := model.calls.Load()
			res, err := a.Run(ctx, "r", agent.UserText(tc.input), tc.opts...)
			if !errors.Is(err, agent.ErrConfig) || res == nil {
				t.Fatalf("drive = %v, %v; want ErrConfig with a Result", res, err)
			}
			if model.calls.Load() != before || pay.n.Load() != 0 {
				t.Fatal("a mismatched drive called the model or a tool")
			}
		})
	}
	// The same values are no mismatch.
	ctx := context.Background()
	j, _ := p14Journal(t)
	a, _, _ := approvalAgent(t, j)
	pauseThenApprove(t, a, j, first...)
	if _, err := a.Run(ctx, "r", agent.UserText("go"), first...); err != nil {
		t.Fatalf("drive with the journaled values = %v, want the answer", err)
	}
}

// Rule 12: the turn limit is checked against the drive's journaled limit when it starts, before
// any model call, whatever the agent's own limit is.
func TestP14Rule12_TurnLimitAtStart(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	a, model, pay := approvalAgent(t, j, agent.WithMaxTurns(10))
	pauseThenApprove(t, a, j, agent.WithMaxTurns(1))
	_, err := a.Resume(ctx, "r")
	if !errors.Is(err, agent.ErrMaxTurns) {
		t.Fatalf("resume = %v, want ErrMaxTurns", err)
	}
	if model.calls.Load() != 1 || pay.n.Load() != 1 {
		t.Fatalf("model calls %d, pay %d; want the pending call run and no second turn", model.calls.Load(), pay.n.Load())
	}
}

// Rule 13 (L5, model 10's findings/filter-request-only): the filter narrows the tools the model
// is offered, and a call outside it is refused at dispatch with an error result recorded.
func TestP14Rule13_FilterEnforcedAtDispatch(t *testing.T) {
	ctx := context.Background()
	j, m := p14Journal(t)
	var pay, look counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(pay.tool("pay", agent.Safety{}), look.tool("lookup", agent.Safety{ReadOnly: true})))
	res, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithToolFilter("lookup"))
	if err != nil || res.Message.Text() != "done" {
		t.Fatalf("run = %v, %v", res, err)
	}
	if pay.n.Load() != 0 {
		t.Fatal("a tool outside the filter ran")
	}
	if names := toolNames(model.lastReq(t)); !slices.Equal(names, []string{"lookup"}) {
		t.Fatalf("offered tools = %v, want [lookup]", names)
	}
	rec, ok, err := j.Get(ctx, "r", "tool:c1")
	if err != nil || !ok || !rec.IsError {
		t.Fatalf("refused call's record = %+v, %v, %v; want an error result", rec, ok, err)
	}
	_ = m
}

// Rule 13 for a turn replayed from the journal, dispatched by a later drive that passes no filter:
// the journaled filter still refuses the call.
func TestP14Rule13_FilterHoldsOnResume(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var pay counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "lookup"), call("c2", "pay")}}, {text: "done"}}}
	gated := agent.MustFunc("lookup", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}),
		agent.WithApproval(agent.SingleApproval()))
	a := p14Build(t, model, j, agent.WithTools(pay.tool("pay", agent.Safety{}), gated))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithToolFilter("lookup")); err == nil {
		t.Fatal("the first drive did not pause")
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Resume(ctx, "r"); err != nil {
		t.Fatalf("resume = %v", err)
	}
	if pay.n.Load() != 0 {
		t.Fatal("the journaled turn's call outside the filter ran on resume")
	}
}

func toolNames(req agent.Request) []string {
	var out []string
	for _, s := range req.Tools {
		out = append(out, s.Name)
	}
	return out
}

// The principal is journaled and restored; the Actor stays live.
func TestP14_PrincipalRestoredActorLive(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var seen agent.Identity
	who := agent.MustFunc("who", "", func(ctx context.Context, _ struct{}) (string, error) {
		seen, _ = agent.IdentityFrom(ctx)
		return "ok", nil
	}, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithApproval(agent.SingleApproval()))
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "who")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(who))
	if _, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithIdentity(agent.Identity{Actor: "v1", OnBehalfOf: "desk", AuthorityRef: "grant-1"})); err == nil {
		t.Fatal("no pause")
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Resume(ctx, "r", agent.WithIdentity(agent.Identity{OnBehalfOf: "elsewhere"})); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("another principal = %v, want ErrConfig", err)
	}
	if _, err := a.Resume(ctx, "r", agent.WithIdentity(agent.Identity{Actor: "v2"})); err != nil {
		t.Fatal(err)
	}
	if seen != (agent.Identity{Actor: "v2", OnBehalfOf: "desk", AuthorityRef: "grant-1"}) {
		t.Fatalf("the tool saw %+v; want the live actor and the journaled principal", seen)
	}
}

type answerV struct {
	V int `json:"v"`
}

type answerW struct {
	W string `json:"w"`
}

// Typed runs journal their schema; they are resumed by ResumeTyped with their type, and refused
// by an untyped drive or another type.
func TestP14_TypedRunResumedByResumeTyped(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var pay counter
	model := &p14Model{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "pay")}},
		{calls: []agent.ToolUse{{ID: "f1", Name: "final_answer", Args: json.RawMessage(`{"v":7}`)}}},
	}}
	a := p14Build(t, model, j, agent.WithTools(pay.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	if _, res, err := a.RunTyped[answerV](ctx, "r", agent.UserText("go")); err == nil || res == nil {
		t.Fatalf("typed run = %v, %v; want the approval pause with a Result", res, err)
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	start, ok, err := agent.RecordedStart(ctx, j, "r")
	if err != nil || !ok || start.Typed == nil || start.Typed.Mode != agent.OutputTool {
		t.Fatalf("recorded start = %+v, %v, %v; want the typed start", start, ok, err)
	}
	before := model.calls.Load()
	if _, err := a.Run(ctx, "r", agent.UserText("go")); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("untyped drive of a typed run = %v, want ErrConfig", err)
	}
	if _, _, err := a.RunTyped[answerW](ctx, "r", agent.UserText("go")); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("another type = %v, want ErrConfig", err)
	}
	if err := agent.ResumeAgent(a)(ctx, "r", start); !errors.Is(err, agent.ErrNotResumable) {
		t.Fatalf("ResumeAgent on a typed run = %v, want ErrNotResumable", err)
	}
	if err := agent.ResumeTyped[answerW](a)(ctx, "r", start); !errors.Is(err, agent.ErrNotResumable) {
		t.Fatalf("ResumeTyped of another type = %v, want ErrNotResumable", err)
	}
	if model.calls.Load() != before {
		t.Fatal("a refused drive called the model")
	}
	if err := agent.ResumeTyped[answerV](a)(ctx, "r", start); err != nil {
		t.Fatalf("ResumeTyped = %v", err)
	}
	v, res, err := a.RunTyped[answerV](ctx, "r", agent.UserText("go"))
	if err != nil || v.V != 7 || !bytes.Contains(res.Output, []byte(`7`)) {
		t.Fatalf("finished typed run = %+v, %+v, %v; want v=7 and the journaled output", v, res, err)
	}
}

// Image input is journaled in run:start and given to the model on every drive.
func TestP14_ImageInput(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	a, model, _ := approvalAgent(t, j)
	img := agent.ImageData("image/png", []byte{0x89, 'P', 'N', 'G'})
	in := agent.UserParts(agent.Text{Text: "what is this?"}, img)
	if _, err := a.Run(ctx, "r", in); err == nil {
		t.Fatal("no pause")
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Resume(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	req := model.lastReq(t)
	var got *agent.Image
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if im, ok := p.(agent.Image); ok {
				got = &im
			}
		}
	}
	if got == nil || !bytes.Equal(got.Data, img.Data) || got.Mime != "image/png" {
		t.Fatalf("the resumed drive sent the image %+v, want the journaled one", got)
	}
	start, _, _ := agent.RecordedStart(ctx, j, "r")
	if len(start.Input.Parts) != 2 {
		t.Fatalf("run:start input = %+v, want the text and the image", start.Input)
	}
	// Another image is another input.
	other := agent.UserParts(agent.Text{Text: "what is this?"}, agent.ImageData("image/png", []byte{1}))
	if _, err := a.Run(ctx, "r", other); err != nil && !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("another image = %v", err)
	}
}

// A Result is returned on every error kind once the run ID is valid; an invalid run ID has none.
func TestP14_ResultOnEveryErrorKind(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("provider down")
	for _, tc := range []struct {
		name  string
		turns []p14Turn
		opts  []agent.RunOption
		want  func(error) bool
	}{
		{"answer", []p14Turn{{text: "done"}}, nil, func(err error) bool { return err == nil }},
		{"pause", []p14Turn{{calls: []agent.ToolUse{call("c1", "gated")}}}, nil, func(err error) bool { return agent.IsPause(err) }},
		{"model failure", []p14Turn{{err: boom}}, nil, func(err error) bool { return errors.Is(err, agent.ErrModel) }},
		{"turn limit", []p14Turn{{calls: []agent.ToolUse{call("c1", "lookup")}}, {text: "done"}}, []agent.RunOption{agent.WithMaxTurns(1)}, func(err error) bool { return errors.Is(err, agent.ErrMaxTurns) }},
		{"bad option", []p14Turn{{text: "done"}}, []agent.RunOption{agent.WithMaxTurns(-1)}, func(err error) bool { return errors.Is(err, agent.ErrConfig) }},
		{"saga abort", []p14Turn{{calls: []agent.ToolUse{call("c1", "fail")}}}, []agent.RunOption{agent.WithSaga()}, func(err error) bool {
			_, ok := errors.AsType[*agent.SagaAborted](err)
			return ok
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, _ := p14Journal(t)
			model := &p14Model{turns: tc.turns}
			a := p14Build(t, model, j, agent.WithTools(
				agent.MustFunc("gated", "", func(context.Context, struct{}) (string, error) { return "", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithApproval(agent.SingleApproval())),
				agent.MustFunc("lookup", "", func(context.Context, struct{}) (string, error) { return "", nil }, agent.WithSafety(agent.Safety{ReadOnly: true})),
				agent.MustFunc("fail", "", func(context.Context, struct{}) (string, error) { return "", errors.New("declined") })))
			res, err := a.Run(ctx, "r", agent.UserText("go"), tc.opts...)
			if !tc.want(err) {
				t.Fatalf("err = %v", err)
			}
			if res == nil || res.RunID != "r" {
				t.Fatalf("Result = %+v; want one for run r", res)
			}
			if err != nil && res.Message.Parts != nil {
				t.Fatalf("a failed run's Result carries a message %+v", res.Message)
			}
		})
	}
	j, _ := p14Journal(t)
	a := p14Build(t, &p14Model{turns: []p14Turn{{text: "x"}}}, j)
	for _, id := range []string{"", "a>b"} {
		if res, err := a.Run(ctx, id, agent.UserText("go")); res != nil || !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("Run(%q) = %v, %v; want ErrConfig and no Result", id, res, err)
		}
	}
	if err := agent.ValidateRunID("ok:run/1"); err != nil {
		t.Fatal(err)
	}
	if err := agent.ValidateRunID("x>y"); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), ">") {
		t.Fatalf("ValidateRunID = %v", err)
	}
}
