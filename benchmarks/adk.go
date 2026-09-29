package benchmarks

import (
	"context"
	"fmt"
	"iter"
	"sync"

	"github.com/bide-ai/bide/chaos"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// ADK-Go (Google's Agent Development Kit) persists every event to a session.Service as the
// run proceeds, and re-invoking the runner with the same session replays that history to the
// model. So a history-aware model naturally de-dupes work that was durably recorded — ADK
// has *real* event persistence, unlike langchaingo/eino. What it has NO framework support
// for is the window between a non-idempotent side effect *executing* and the AppendEvent
// that *records* it: there is no attempt-marker / halt-on-unknown-outcome. A crash in that
// window loses the record but not the charge, and the replay re-fires it.
//
// We inject the crash through ADK's real machinery: a session.Service wrapper that fails the
// Nth AppendEvent of a run (a real persist failure). The session survives across Steps (that
// is ADK's durability); "resume" is a fresh runner.Run on the same session, which replays
// history. This mirrors the fair, session-based injection used for trpc — the measured
// number, not an assumed one, decides where ADK lands.

// adkModel is a deterministic, history-aware mock model. On a model turn it scans the
// conversation the runner replays from the session: if a completed `charge` tool response is
// already in history, it answers "done"; otherwise it calls `charge`. This is exactly how a
// real LLM behaves — it sees the conversation — so a charge that was durably recorded is not
// repeated, and only an *unrecorded* charge (crash in the execute→persist window) re-fires.
type adkModel struct{}

func (adkModel) Name() string { return "mock" }

func (adkModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	charged := false
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "charge" {
				charged = true
			}
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		if charged {
			yield(&model.LLMResponse{Content: &genai.Content{
				Role:  "model",
				Parts: []*genai.Part{genai.NewPartFromText("done")},
			}}, nil)
			return
		}
		yield(&model.LLMResponse{Content: &genai.Content{
			Role: "model",
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID:   "fc1",
				Name: "charge",
				Args: map[string]any{},
			}}},
		}}, nil)
	}
}

// crashSession wraps the run's persistent session.Service for one Step and fails the failAt-th
// AppendEvent, modelling a process crash mid-persist. failAt<=0 disables injection. A crash is the
// process dying, so once it has happened the Step is dead: every later AppendEvent fails too, and
// the charge does not run (see adkRun.Step). ADK keeps running the agent in a scheduler goroutine
// after the runner stops on the failed append, and that goroutine can still reach the tool; a
// real crash would not let it.
type crashSession struct {
	inner   session.Service
	failAt  int // 1-based append index to fail on, within this Step
	mu      sync.Mutex
	appends int  // appends seen so far this Step
	dropped bool // the crash happened: this Step is dead
}

// alive reports whether the Step's process is still running (no crash yet) and, if so, runs fn
// while holding the lock, so fn cannot interleave with the crash.
func (c *crashSession) alive(fn func()) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped {
		return false
	}
	fn()
	return true
}

func (c *crashSession) Create(ctx context.Context, r *session.CreateRequest) (*session.CreateResponse, error) {
	return c.inner.Create(ctx, r)
}
func (c *crashSession) Get(ctx context.Context, r *session.GetRequest) (*session.GetResponse, error) {
	return c.inner.Get(ctx, r)
}
func (c *crashSession) List(ctx context.Context, r *session.ListRequest) (*session.ListResponse, error) {
	return c.inner.List(ctx, r)
}
func (c *crashSession) Delete(ctx context.Context, r *session.DeleteRequest) error {
	return c.inner.Delete(ctx, r)
}
func (c *crashSession) AppendEvent(ctx context.Context, s session.Session, e *session.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped {
		return fmt.Errorf("chaos: process crashed at append #%d", c.failAt)
	}
	c.appends++
	if c.failAt > 0 && c.appends == c.failAt {
		c.dropped = true
		return fmt.Errorf("chaos: injected crash at append #%d", c.failAt)
	}
	return c.inner.AppendEvent(ctx, s, e)
}

// ADK returns a chaos.System for an ADK-Go LLM agent (mock model + one non-idempotent tool).
func ADK() chaos.System { return adkSys{} }

type adkSys struct{}

// Writes is the number of AppendEvents in a clean run (measured; see writes_test.go).
func (adkSys) Writes() int { return 4 }

func (adkSys) NewRun() chaos.Run {
	return &adkRun{fired: new(int), store: session.InMemoryService()}
}

type adkRun struct {
	mu    sync.Mutex // guards fired
	fired *int
	store session.Service // one persisted session across Steps — ADK's durability
}

func (r *adkRun) Fired() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r.fired
}

func (r *adkRun) Step(crashAt int) bool {
	ctx := context.Background()
	svc := &crashSession{inner: r.store, failAt: crashAt}

	charge, err := functiontool.New(functiontool.Config{
		Name:        "charge",
		Description: "Charge the customer once.",
	}, func(_ agent.Context, _ struct{}) (struct{}, error) {
		svc.alive(func() {
			r.mu.Lock()
			*r.fired++ // the non-idempotent side effect
			r.mu.Unlock()
		})
		return struct{}{}, nil
	})
	if err != nil {
		return false
	}

	ag, err := llmagent.New(llmagent.Config{
		Name:        "charger",
		Description: "Charges the customer.",
		Model:       adkModel{},
		Instruction: "Charge the customer, then report done.",
		Tools:       []tool.Tool{charge},
	})
	if err != nil {
		return false
	}

	run, err := runner.New(runner.Config{
		AppName:           "chaos",
		Agent:             ag,
		SessionService:    svc,
		AutoCreateSession: true,
	})
	if err != nil {
		return false
	}

	// Drain the run to completion (or to the injected fault).
	msg := genai.NewContentFromText("charge the customer", genai.RoleUser)
	for _, err := range run.Run(ctx, "u", "s", msg, agent.RunConfig{}) {
		if err != nil {
			break // the injected persist failure aborts the run, as a crash would
		}
	}
	return !svc.alive(func() {}) // crashed → the harness resumes with Step(0) on the same session
}
