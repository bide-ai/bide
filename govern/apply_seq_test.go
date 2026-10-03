package govern_test

import (
	"context"
	"slices"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/govern"
)

// composite is a retry-safe tool that calls each of tools once, in order, within its own call.
func composite(name string, tools ...agent.Tool) agent.Tool {
	return agent.Func(name, "", agent.Safety{Idempotent: true}, func(ctx context.Context, _ struct{}) (map[string]any, error) {
		for _, tl := range tools {
			if _, err := tl.Call(ctx, []byte(`{}`)); err != nil {
				return nil, err
			}
		}
		return map[string]any{"ok": true}, nil
	})
}

func runOnce(t *testing.T, tool agent.Tool) {
	t.Helper()
	a := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("c1", tool.Name(), `{}`), agent.TextTurn("done")),
		agenttest.MemJournal(),
		agent.WithTools(tool),
	)
	if _, err := a.Run(context.Background(), "r", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
}

func logEvents(t *testing.T, log govern.EventLog, entity string) []string {
	t.Helper()
	evs, err := log.Events(context.Background(), entity, 0)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// A tool call that applies governed events more than once (a composite tool reusing EventTool)
// applies each of them: the same event twice, or two different events.
func TestEventTool_EveryApplyInOneCallIsRecorded(t *testing.T) {
	ctx := context.Background()
	m := buildTwoCounters(t)
	for name, events := range map[string][]string{"same event twice": {"inc_a", "inc_a"}, "two events": {"inc_a", "inc_b"}} {
		t.Run(name, func(t *testing.T) {
			log := govern.NewMemEventLog()
			g, err := govern.NewPersistent(ctx, m, log, "e", m.NewState())
			if err != nil {
				t.Fatal(err)
			}
			var tools []agent.Tool
			for _, ev := range events {
				tools = append(tools, govern.EventTool(g, govern.EventToolConfig{Name: ev, Description: "", Event: ev, Safety: agent.Safety{Idempotent: true}}))
			}
			runOnce(t, composite("both", tools...))
			if evs := logEvents(t, log, "e"); !slices.Equal(evs, events) {
				t.Fatalf("one tool call applying %q recorded %q", events, evs)
			}
		})
	}
}

// The same holds for the in-memory governor and for a federated governor.
func TestEventTool_EveryApplyInOneCallIsRecorded_InMemoryAndFederated(t *testing.T) {
	ctx := context.Background()
	m := buildTwoCounters(t)
	g := govern.New(m, m.NewState())
	runOnce(t, composite("both", govern.EventTool(g, govern.EventToolConfig{Name: "a1", Description: "", Event: "inc_a", Safety: agent.Safety{Idempotent: true}}), govern.EventTool(g, govern.EventToolConfig{Name: "a2", Description: "", Event: "inc_a", Safety: agent.Safety{Idempotent: true}})))
	if got := g.State().Digest(); got != m.Apply(m.Apply(m.NewState(), "inc_a"), "inc_a").Digest() {
		t.Fatal("two applies in one tool call applied once to the in-memory governor")
	}

	fm, _, _, _, _ := buildMfrSupFederation(t)
	log := govern.NewMemEventLog()
	fg, err := govern.NewFederated(ctx, fm, log, "f", fm.NewState())
	if err != nil {
		t.Fatal(err)
	}
	pub := govern.FederatedEventTool(fg, govern.FederatedEventToolConfig{Name: "publish", Description: "", Registry: "manufacturer", Event: "epub", Safety: agent.Safety{Idempotent: true}})
	runOnce(t, composite("twice", pub, pub))
	if evs := logEvents(t, log, "f"); len(evs) != 2 {
		t.Fatalf("two federated applies in one tool call recorded %d entries: %q", len(evs), evs)
	}
}

// A tool call that applies two events and then re-runs (its process died before its result was
// recorded) records each event once: each apply keeps its id across runs of the call.
func TestEventTool_ReRunOfATwoApplyCallRecordsEachOnce(t *testing.T) {
	ctx := context.Background()
	m := buildTwoCounters(t)
	log := govern.NewMemEventLog()
	g, err := govern.NewPersistent(ctx, m, log, "e", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	tool := composite("both",
		govern.EventTool(g, govern.EventToolConfig{Name: "a", Description: "", Event: "inc_a", Safety: agent.Safety{Idempotent: true}}),
		govern.EventTool(g, govern.EventToolConfig{Name: "b", Description: "", Event: "inc_b", Safety: agent.Safety{Idempotent: true}}),
		govern.EventTool(g, govern.EventToolConfig{Name: "a", Description: "", Event: "inc_a", Safety: agent.Safety{Idempotent: true}}))
	runToolTwice(t, tool, nil)
	if evs := logEvents(t, log, "e"); !slices.Equal(evs, []string{"inc_a", "inc_b", "inc_a"}) {
		t.Fatalf("a re-run three-apply tool call recorded %q", evs)
	}
}

// A tool call that fans out with agent.Parallel, each task applying its own event, records every
// event once, even when the tasks apply in a different order when the call runs again.
func TestEventTool_ParallelFanOutInOneCall(t *testing.T) {
	ctx := context.Background()
	m := buildTwoCounters(t)
	log := govern.NewMemEventLog()
	g, err := govern.NewPersistent(ctx, m, log, "e", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	incA := govern.EventTool(g, govern.EventToolConfig{Name: "a", Description: "", Event: "inc_a", Safety: agent.Safety{Idempotent: true}})
	incB := govern.EventTool(g, govern.EventToolConfig{Name: "b", Description: "", Event: "inc_b", Safety: agent.Safety{Idempotent: true}})
	bFirst := true // which task applies first; the re-run flips it
	fan := agent.Func("fan", "", agent.Safety{Idempotent: true}, func(ctx context.Context, _ struct{}) (map[string]any, error) {
		first, second := incA, incB
		if bFirst {
			first, second = incB, incA
		}
		firstDone := make(chan struct{})
		task := func(tl agent.Tool, wait, done chan struct{}) func(context.Context) (bool, error) {
			return func(ctx context.Context) (bool, error) {
				if wait != nil {
					<-wait
				}
				_, err := tl.Call(ctx, []byte(`{}`))
				if done != nil {
					close(done)
				}
				return err == nil, err
			}
		}
		fnFor := map[agent.Tool]func(context.Context) (bool, error){
			first:  task(first, nil, firstDone),
			second: task(second, firstDone, nil),
		}
		// A fresh journal each time the call runs, so the re-run runs both tasks again.
		_, err := agent.Parallel(ctx, agenttest.MemJournal(), "fan", []agent.Task[bool]{
			{Name: "a", Fn: fnFor[incA], Safety: agent.Safety{Idempotent: true}},
			{Name: "b", Fn: fnFor[incB], Safety: agent.Safety{Idempotent: true}}})
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
	runToolTwice(t, fan, func() { bFirst = false })
	evs := logEvents(t, log, "e")
	if !slices.Equal(evs, []string{"inc_b", "inc_a"}) {
		t.Fatalf("a re-run fan-out tool call recorded %q, want [inc_b inc_a]", evs)
	}
}
