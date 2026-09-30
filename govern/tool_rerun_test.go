package govern_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern"
)

// crashAfterStep stands in for a process that dies after a step's work ran but before its
// record was written: the first Do for step name runs fn, then fails without recording.
type crashAfterStep struct {
	agent.Durable
	name    string
	crashed bool
}

func (s *crashAfterStep) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	if name == s.name && !s.crashed {
		s.crashed = true
		_, _ = fn(ctx)
		return agent.Record{}, errors.New("process died before the tool result was recorded")
	}
	return s.Durable.Do(ctx, runID, name, fn)
}

// runToolTwice drives one retry-safe tool call (c1) through a run that crashes before the
// call's result is recorded, then re-runs the run so the call runs again, and returns the
// recorded result of the second run.
func runToolTwice(t *testing.T, tool agent.Tool, between func()) map[string]any {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	crashing := &crashAfterStep{Durable: store, name: agent.ToolResultStep("c1")}
	first := agent.New(agent.NewScriptedModel(agent.ToolTurn("c1", tool.Name(), `{}`), agent.TextTurn("done")), crashing, tool)
	if _, err := first.Run(ctx, "r", "go"); err == nil {
		t.Fatal("the first run did not crash")
	}
	if between != nil {
		between()
	}
	second := agent.New(agent.NewScriptedModel(agent.ToolTurn("c1", tool.Name(), `{}`), agent.TextTurn("done")), store, tool)
	if _, err := second.Run(ctx, "r", "go"); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	recs, err := store.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Name == agent.ToolResultStep("c1") {
			var m map[string]any
			if err := json.Unmarshal(r.Result, &m); err != nil {
				t.Fatalf("tool result %s: %v", r.Result, err)
			}
			return m
		}
	}
	t.Fatal("no recorded result for c1")
	return nil
}

// An EventTool call that re-runs (the process died after the event was appended but before the
// tool's result was recorded) appends its event once, not once per run of the call.
func TestEventTool_RerunAfterCrashAppendsOnce(t *testing.T) {
	ctx := context.Background()
	m := buildCounter(t)
	log := govern.NewMemEventLog()
	g, err := govern.NewPersistent(ctx, m, log, "e", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	res := runToolTwice(t, govern.EventTool(g, govern.EventToolConfig{Name: "bump", Description: "", Event: "inc_a", Safety: agent.Safety{Idempotent: true}}), nil)
	evs, _ := log.Events(ctx, "e", 0)
	if len(evs) != 1 {
		t.Fatalf("one tool call appended %d events: %q", len(evs), evs)
	}
	if res["position"] != float64(0) {
		t.Fatalf("re-run reported position %v, want 0 (the original append)", res["position"])
	}
}

// When another process appends between the crash and the re-run, the re-run still reports the
// original append: its position, and the state an auditor gets by replaying the log through it.
func TestEventToolAttested_RerunReportsTheOriginalAppend(t *testing.T) {
	ctx := context.Background()
	m := buildCounter(t)
	log := govern.NewMemEventLog()
	g, err := govern.NewPersistent(ctx, m, log, "e", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	other, err := govern.NewPersistent(ctx, m, log, "e", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	tool := govern.EventTool(g, govern.EventToolConfig{Name: "bump", Description: "", Event: "inc_a", PolicyDigest: "policy", Safety: agent.Safety{Idempotent: true}})
	res := runToolTwice(t, tool, func() {
		if _, err := other.Apply(ctx, "inc_a"); err != nil {
			t.Fatal(err)
		}
		if _, err := g.Sync(ctx); err != nil { // the governor has folded past the original append
			t.Fatal(err)
		}
	})
	evs, _ := log.Events(ctx, "e", 0)
	if len(evs) != 2 {
		t.Fatalf("log holds %d events, want 2 (one per tool call): %q", len(evs), evs)
	}
	want := m.Apply(m.NewState(), "inc_a").Digest()
	if res["position"] != float64(0) || res["state_digest"] != want {
		t.Fatalf("re-run reported position %v, state %v; want position 0 and the replay through it (%s)", res["position"], res["state_digest"], want)
	}
}

// The same holds for the in-memory Governor and for a federated governor.
func TestEventTool_RerunAfterCrashAppliesOnce_InMemoryAndFederated(t *testing.T) {
	ctx := context.Background()
	m := buildCounter(t)
	g := govern.New(m, m.NewState())
	runToolTwice(t, govern.EventTool(g, govern.EventToolConfig{Name: "bump", Description: "", Event: "inc_a", Safety: agent.Safety{Idempotent: true}}), nil)
	if a, err := g.Apply(ctx, "inc_a"); err != nil || a.Position != 1 {
		t.Fatalf("next Apply after one re-run tool call = %+v, %v; want position 1", a, err)
	}

	fm, _, _, _, _ := buildMfrSupFederation(t)
	log := govern.NewMemEventLog()
	fg, err := govern.NewFederated(ctx, fm, log, "f", fm.NewState())
	if err != nil {
		t.Fatal(err)
	}
	runToolTwice(t, govern.FederatedEventTool(fg, govern.FederatedEventToolConfig{Name: "publish", Description: "", Registry: "manufacturer", Event: "epub", Safety: agent.Safety{Idempotent: true}}), nil)
	if evs, _ := log.Events(ctx, "f", 0); len(evs) != 1 {
		t.Fatalf("one federated tool call appended %d entries: %q", len(evs), evs)
	}
}
