package chaos

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/blackwell-systems/bide/agent"
)

// NaiveReference is a deliberately-simple at-least-once loop: it journals tool results and
// replays them, but a tool whose result isn't journaled is simply RE-RUN — no attempt
// marker, no halt-on-unknown-outcome. So a crash after the side effect but before its
// result persists causes a DOUBLE-FIRE. This is the common "just make your side effects
// idempotent" design; it is the baseline an SDK must beat on this benchmark, and it makes
// the harness demonstrably non-vacuous (a correct loop passes it; this one fails).
func NaiveReference() System { return naive{} }

type naive struct{}

func (naive) Writes() int { return 3 } // @llm/0, c1, @llm/1

func (naive) NewRun() Run { return &naiveRun{store: agent.NewMemStore(), fired: new(int)} }

type naiveRun struct {
	store agent.Durable
	fired *int
}

func (r *naiveRun) Fired() int { return *r.fired }

func (r *naiveRun) Step(crashAt int) bool {
	cs := &crashStore{inner: r.store, crashAt: crashAt}
	ctx := context.Background()

	recs, _ := cs.History(ctx, "chaos")
	charged := false
	for _, rec := range recs {
		if rec.Kind == agent.StepToolResult && rec.ToolUseID == "c1" {
			charged = true
		}
	}

	do := func(name string, fn func() (agent.Record, error)) bool {
		_, err := cs.Do(ctx, "chaos", name, func(context.Context) (agent.Record, error) { return fn() })
		return errors.Is(err, errCrash)
	}

	if do("@llm/0", func() (agent.Record, error) {
		return agent.Record{Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant}}, nil
	}) {
		return true
	}
	if !charged {
		// NAIVE: run the tool (side effect INSIDE fn) then journal — but NO attempt marker
		// and NO halt-on-unknown. A crash on this write loses the result; resume re-runs it.
		if do("c1", func() (agent.Record, error) {
			*r.fired++ // the side effect
			return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{}`)}, nil
		}) {
			return true
		}
	}
	return do("@llm/1", func() (agent.Record, error) {
		return agent.Record{Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant}}, nil
	})
}
