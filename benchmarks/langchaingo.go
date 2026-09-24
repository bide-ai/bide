package benchmarks

import (
	"context"

	"github.com/dayna/go-agents/chaos"
	"github.com/tmc/langchaingo/agents"
	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/tools"
)

// langchaingo has NO durable resume/checkpoint of any kind. So the crash model is: the
// tool fires, the process dies, and the natural recovery — re-invoking the agent — re-runs
// everything (there is no record of what already happened). We model the crash as
// cancelling the run's context right after the side effect fires; "resume" is a fresh
// invocation, which re-fires. This isn't a bug in langchaingo — durable side-effect safety
// is simply an absent feature.

// mockLLM drives the MRKL agent deterministically: first call → call the charge tool, then
// → final answer. Respects ctx cancellation so an injected crash aborts the run.
type mockLLM struct{ calls int }

func (m *mockLLM) GenerateContent(ctx context.Context, _ []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.calls++
	text := "Action: charge\nAction Input: none"
	if m.calls > 1 {
		text = "Final Answer: done"
	}
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: text}}}, nil
}

func (m *mockLLM) Call(ctx context.Context, prompt string, opts ...llms.CallOption) (string, error) {
	return llms.GenerateFromSinglePrompt(ctx, m, prompt, opts...)
}

// chargeToolLCG is the non-idempotent side effect. On a crash run it cancels the context
// right after firing, so the run aborts as if the process died at that instant.
type chargeToolLCG struct {
	fired  *int
	cancel context.CancelFunc
	crash  bool
}

func (chargeToolLCG) Name() string        { return "charge" }
func (chargeToolLCG) Description() string { return "Charge the customer. Input: none." }
func (t chargeToolLCG) Call(_ context.Context, _ string) (string, error) {
	*t.fired++ // the non-idempotent side effect
	if t.crash {
		t.cancel() // "process dies" right after the charge
	}
	return "charged", nil
}

// LangChainGo returns a chaos.System for langchaingo's MRKL agent executor.
func LangChainGo() chaos.System { return lcgSys{} }

type lcgSys struct{}

func (lcgSys) Writes() int { return 2 }

func (lcgSys) NewRun() chaos.Run { return &lcgRun{fired: new(int)} }

type lcgRun struct{ fired *int } // no store — langchaingo has none

func (r *lcgRun) Fired() int { return *r.fired }

func (r *lcgRun) Step(crashAt int) bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	crash := crashAt > 0
	charge := chargeToolLCG{fired: r.fired, cancel: cancel, crash: crash}
	exec, err := agents.Initialize(&mockLLM{}, []tools.Tool{charge}, agents.ZeroShotReactDescription)
	if err != nil {
		return false
	}
	_, runErr := chains.Run(ctx, exec, "charge the customer")
	// A crash run aborts (ctx cancelled after firing) → runErr != nil → needs a re-invoke.
	return crash && runErr != nil
}
