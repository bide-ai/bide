package govern_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/govern"
)

// crashAfterStep is a store wrapper standing in for a process that dies after a step's work ran
// but before its record was written: the first Insert of step name (which comes after the step's
// work ran) fails without storing. It does not Unwrap: the dying process shares nothing in-process
// with the one that re-runs the run.
type crashAfterStep struct {
	agent.Store
	name    string
	crashed bool
}

func (s *crashAfterStep) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if name == s.name && !s.crashed {
		s.crashed = true
		return agent.Entry{}, false, errors.New("process died before the tool result was recorded")
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// runToolTwice drives one retry-safe tool call (c1) through a run that crashes before the
// call's result is recorded, then re-runs the run so the call runs again, and returns the
// recorded result of the second run.
func runToolTwice(t *testing.T, tool agent.Tool, between func()) map[string]any {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	j := agenttest.MustJournal(store)
	crashing := agenttest.MustJournal(&crashAfterStep{Store: store, name: agent.ToolResultStep("c1")})
	first := agenttest.MustNew(
		agenttest.NewScriptedModel(agenttest.ToolTurn("c1", tool.Spec().Name, `{}`), agenttest.TextTurn("done")),
		crashing,
		agent.WithTools(tool),
	)
	if _, err := first.Run(ctx, "r", agent.UserText("go")); err == nil {
		t.Fatal("the first run did not crash")
	}
	if between != nil {
		between()
	}
	second := agenttest.MustNew(
		agenttest.NewScriptedModel(agenttest.ToolTurn("c1", tool.Spec().Name, `{}`), agenttest.TextTurn("done")),
		j,
		agent.WithTools(tool),
	)
	if _, err := second.Run(ctx, "r", agent.UserText("go")); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	recs, err := j.History(ctx, "r")
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

// EventTool and FederatedEventTool apply their config's Safety and Options to the tool's spec, so
// a governed action can be gated on approval or bounded by a timeout like any agent tool.
func TestEventToolConfig_SpecCarriesSafetyAndOptions(t *testing.T) {
	ctx := context.Background()
	m := buildCounter(t)
	g, err := govern.NewPersistent(ctx, m, govern.NewMemEventLog(), "e", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	opts := []agent.ToolOption{agent.WithApproval(agent.SingleApproval()), agent.WithTimeout(time.Minute), agent.WithTitle("Bump")}
	for name, tool := range map[string]agent.Tool{
		"plain":    govern.EventTool(g, govern.EventToolConfig{Name: "bump", Event: "inc_a", Safety: agent.Safety{Idempotent: true}, Options: opts}),
		"attested": govern.EventTool(g, govern.EventToolConfig{Name: "bump", Event: "inc_a", PolicyDigest: "p", Safety: agent.Safety{Idempotent: true}, Options: opts}),
	} {
		s := tool.Spec()
		if s.Name != "bump" || !s.Safety.Idempotent || s.Approval == nil || s.Timeout != time.Minute || s.Title != "Bump" {
			t.Errorf("%s: spec %+v; want the config's name, safety and options", name, s)
		}
	}
	fm, _, _, _, _ := buildMfrSupFederation(t)
	fg, err := govern.NewFederated(ctx, fm, govern.NewMemEventLog(), "f", fm.NewState())
	if err != nil {
		t.Fatal(err)
	}
	s := govern.FederatedEventTool(fg, govern.FederatedEventToolConfig{Name: "publish", Registry: "manufacturer", Event: "epub", Safety: agent.Safety{Idempotent: true}, Options: opts}).Spec()
	if s.Name != "publish" || !s.Safety.Idempotent || s.Approval == nil || s.Timeout != time.Minute {
		t.Errorf("federated: spec %+v; want the config's name, safety and options", s)
	}
}
