package agent_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// WeatherIn is the arguments of the weather tool in the examples.
type WeatherIn struct {
	City string `json:"city"`
}

// ChargeIn is the arguments of the charge tool in the examples.
type ChargeIn struct {
	Cents int `json:"cents"`
}

// Build an agent with one tool and run it. The scripted model calls the tool, then answers;
// a real adapter (model/anthropic, model/openai, model/gemini) takes its place in production.
func Example() {
	ctx := context.Background()
	weather := agenttest.Must(agent.Func("weather", "Current weather for a city.",
		func(_ context.Context, in WeatherIn) (string, error) { return "sunny in " + in.City, nil },
		agent.WithSafety(agent.Safety{ReadOnly: true})))

	model := agenttest.NewScriptedModel(
		agenttest.ToolTurn("call-1", "weather", `{"city":"Lisbon"}`),
		agenttest.TextTurn("It is sunny in Lisbon."),
	)
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		fmt.Println(err)
		return
	}
	a, err := agent.New(model, j, agent.WithTools(weather))
	if err != nil {
		fmt.Println(err)
		return
	}

	res, err := a.Run(ctx, "run-1", agent.UserText("What's the weather in Lisbon?"))
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(res.Message.Text())
	// Output: It is sunny in Lisbon.
}

// A run that fails partway resumes when Run is called again with the same run ID on the same
// journal: completed steps are replayed from the journal, so the tool does not run twice.
func ExampleAgent_Run_resume() {
	ctx := context.Background()
	var lookups int
	lookup := agenttest.Must(agent.Func("lookup", "Look up an order.",
		func(context.Context, struct{}) (string, error) { lookups++; return "order 42: shipped", nil },
		agent.WithSafety(agent.Safety{ReadOnly: true})))

	// The model is down for the second call of the first attempt.
	model := &outage{Model: agenttest.NewScriptedModel(
		agenttest.ToolTurn("call-1", "lookup", `{}`),
		agenttest.TextTurn("Your order has shipped."),
	), failAt: 2}
	j := agenttest.MemJournal()
	a := agenttest.MustNew(model, j, agent.WithTools(lookup))

	input := agent.UserText("Where is my order?")
	_, err := a.Run(ctx, "run-1", input)
	fmt.Println("first attempt failed on the model:", errors.Is(err, agent.ErrModel))

	res, err := a.Run(ctx, "run-1", input)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("resumed:", res.Message.Text())
	fmt.Println("lookups:", lookups)
	// Output:
	// first attempt failed on the model: true
	// resumed: Your order has shipped.
	// lookups: 1
}

// A tool's Safety decides what a resume does with a call that was attempted but has no recorded
// result (the process died, or lost its store, mid-call). A ReadOnly tool is run again; a side
// effect, the default, halts the run with *OutcomeUnknown rather than risk running it twice.
func ExampleSafety() {
	ctx := context.Background()
	store := &flakyStore{Store: agent.NewMemStore()}
	j := agenttest.MustJournal(store)

	var reads, charges int
	read := agenttest.Must(agent.Func("balance", "Read the balance.",
		func(context.Context, struct{}) (int, error) {
			reads++
			store.down.Store(reads == 1) // the first call's result is lost
			return 100, nil
		},
		agent.WithSafety(agent.Safety{ReadOnly: true})))
	charge := agenttest.Must(agent.Func("charge", "Charge the card.",
		func(context.Context, struct{}) (string, error) {
			charges++
			store.down.Store(true) // the charge went through, but its result is lost
			return "charged", nil
		})) // no WithSafety: a side effect

	for _, tool := range []agent.Tool{read, charge} {
		name := tool.Spec().Name
		model := agenttest.NewScriptedModel(
			agenttest.ToolTurn("call-1", name, `{}`),
			agenttest.TextTurn("done"),
		)
		a := agenttest.MustNew(model, j, agent.WithTools(tool))
		runID := "run-" + name
		_, _ = a.Run(ctx, runID, agent.UserText("go")) // fails: the store is down
		store.down.Store(false)

		_, err := a.Run(ctx, runID, agent.UserText("go"))
		var halt *agent.OutcomeUnknown
		switch {
		case errors.As(err, &halt):
			fmt.Printf("%s: halted on %s\n", name, halt.Op.ID)
		case err != nil:
			fmt.Printf("%s: %v\n", name, err)
		default:
			fmt.Printf("%s: finished\n", name)
		}
	}
	fmt.Println("reads:", reads, "charges:", charges)
	// Output:
	// balance: finished
	// charge: halted on call-1
	// reads: 2 charges: 1
}

// A tool gated by WithApproval pauses the run with *ApprovalPending before it runs. Approve
// records the decision, and calling Run again resumes the run past the pause.
func ExampleApprove() {
	ctx := context.Background()
	refund := agenttest.Must(agent.Func("refund", "Refund a payment.",
		func(_ context.Context, in ChargeIn) (string, error) { return fmt.Sprintf("refunded %d", in.Cents), nil },
		agent.WithApproval(agent.SingleApproval())))
	model := agenttest.NewScriptedModel(
		agenttest.ToolTurn("call-1", "refund", `{"cents":500}`),
		agenttest.TextTurn("The refund is done."),
	)
	j := agenttest.MemJournal()
	a := agenttest.MustNew(model, j, agent.WithTools(refund))

	input := agent.UserText("Refund order 42.")
	_, err := a.Run(ctx, "run-1", input)
	var pending *agent.ApprovalPending
	if !errors.As(err, &pending) {
		fmt.Println("unexpected:", err)
		return
	}
	fmt.Printf("waiting for approval of %s %s\n", pending.ToolName, pending.Args)

	if err := agent.Approve(ctx, j, pending.RunID, pending.ToolUseID, true); err != nil {
		fmt.Println(err)
		return
	}
	res, err := a.Run(ctx, pending.RootRunID, input)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(res.Message.Text())
	// Output:
	// waiting for approval of refund {"cents":500}
	// The refund is done.
}

// A run halted with *OutcomeUnknown stays halted until someone who has checked the real
// outcome out of band records it with ResolveHalt. Calling Run again then continues past the
// halt, and the tool does not run a second time.
func ExampleResolveHalt() {
	ctx := context.Background()
	store := &flakyStore{Store: agent.NewMemStore()}
	j := agenttest.MustJournal(store)

	var charges int
	charge := agenttest.Must(agent.Func("charge", "Charge the card.",
		func(context.Context, ChargeIn) (string, error) {
			charges++
			store.down.Store(true) // the charge went through, but its result is lost
			return "charged", nil
		}))
	model := agenttest.NewScriptedModel(
		agenttest.ToolTurn("call-1", "charge", `{"cents":500}`),
		agenttest.TextTurn("You have been charged."),
	)
	a := agenttest.MustNew(model, j, agent.WithTools(charge))

	input := agent.UserText("Charge me.")
	_, _ = a.Run(ctx, "run-1", input) // fails: the store is down
	store.down.Store(false)

	_, err := a.Run(ctx, "run-1", input)
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) {
		fmt.Println("unexpected:", err)
		return
	}
	fmt.Println("halted:", halt.Cause)

	// The payment provider confirms the charge went through: record that as the call's result.
	err = agent.ResolveHalt(ctx, j, halt.Ref(), agent.Outcome{Result: "charged (confirmed by the provider)"})
	if err != nil {
		fmt.Println(err)
		return
	}
	res, err := a.Run(ctx, halt.RootRunID, input)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(res.Message.Text())
	fmt.Println("charges:", charges)
	// Output:
	// halted: crashed
	// You have been charged.
	// charges: 1
}

// outage is a Model that fails its failAt'th call, as a provider outage would.
type outage struct {
	agent.Model
	calls, failAt int
}

func (m *outage) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	if m.calls++; m.calls == m.failAt {
		return nil, fmt.Errorf("%w: provider unavailable", agent.ErrModel)
	}
	return m.Model.Stream(ctx, req)
}

// flakyStore is a Store whose writes fail while down is set, as a lost database connection
// would.
type flakyStore struct {
	agent.Store
	down atomic.Bool
}

func (s *flakyStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if s.down.Load() {
		return agent.Entry{}, false, errors.New("store unavailable")
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// Unwrap returns the wrapped store, so the journal finds its capabilities (its leases).
func (s *flakyStore) Unwrap() agent.Store { return s.Store }
