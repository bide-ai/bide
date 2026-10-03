package agent_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// The store round trips each engine operation costs, counted by agenttest.CountingStore. These are
// exact budgets: a change may lower one (and must lower the number here with it), never raise it.
// Each case lists the Inserts and Gets in order, and the Loads with the entries they read.

// countingJournal returns a Journal over a CountingStore over a MemStore.
func countingJournal(t *testing.T) (*agent.Journal, *agenttest.CountingStore, *agent.MemStore) {
	t.Helper()
	m := agent.NewMemStore()
	cs := agenttest.NewCountingStore(m)
	j, err := agent.NewJournal(cs)
	if err != nil {
		t.Fatal(err)
	}
	return j, cs, m
}

// wantCounts checks the round trips since the last Reset.
func wantCounts(t *testing.T, cs *agenttest.CountingStore, what string, names []string, loads, read int) {
	t.Helper()
	c := cs.Reset()
	if !slices.Equal(c.Names, names) || c.Load != loads || c.EntriesRead != read {
		t.Errorf("%s:\n got %v, %d Load(s) reading %d entries\nwant %v, %d Load(s) reading %d entries", what, c.Names, c.Load, c.EntriesRead, names, loads, read)
	}
}

func sideEffect() agent.Tool {
	return agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
}

func readOnly() agent.Tool {
	return agent.MustFunc("lookup", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
}

// A new run that answers at once: one Load (which finds the run empty), the header, the start
// record, a second Load once the start is written (model 10's DStart returns to DOpen, so a Cancel
// that landed meanwhile is seen: it reads the header and run:start), one model turn (its memo read
// and its record), and the completion with its read-back of run:cancelled (P14 rule 4: the first
// end marker in journal order is the run's end). The drive's first turn needs no run:cancelled
// check: its Load read the run.
func TestBudget_FirstDriveAndCompletion(t *testing.T) {
	j, cs, _ := countingJournal(t)
	if _, err := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("done")), j).Run(context.Background(), "r", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	wantCounts(t, cs, "first drive, one turn, completion",
		[]string{"insert @journal", "insert run:start", "get @llm/0", "insert @llm/0", "insert run:complete", "get run:cancelled"}, 2, 2)
}

// A side-effect tool call costs two Inserts (its claim and its result) and one Get (run:cancelled,
// read once the claim is won: P14 rule 3); a retry-safe one, one Insert. Every model turn after a
// drive's first costs one Get more (run:cancelled at the turn boundary: P14 rule 2).
func TestBudget_ToolCalls(t *testing.T) {
	for _, c := range []struct {
		name string
		tool agent.Tool
		want []string
	}{
		{"side effect", sideEffect(), []string{"insert attempt:tool:c1", "get run:cancelled", "insert tool:c1"}},
		{"retry-safe", readOnly(), []string{"insert tool:c1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			j, cs, _ := countingJournal(t)
			m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", c.tool.Spec().Name, `{}`), agenttest.TextTurn("done"))
			if _, err := agenttest.MustNew(m, j, agent.WithTools(c.tool)).Run(context.Background(), "r", agent.UserText("hi")); err != nil {
				t.Fatal(err)
			}
			want := append([]string{"insert @journal", "insert run:start", "get @llm/0", "insert @llm/0"}, c.want...)
			want = append(want, "get run:cancelled", "get @llm/1", "insert @llm/1", "insert run:complete", "get run:cancelled")
			wantCounts(t, cs, c.name+" tool call", want, 2, 2) // the Load, and the Load after run:start
		})
	}
}

// Resuming a run reads it once and nothing else: no point reads for its markers, no write of a
// start record or a header it already has, whether or not the Journal has seen the run before.
func TestBudget_Resume(t *testing.T) {
	ctx := context.Background()
	j, cs, m := countingJournal(t)
	// A side-effect call claimed and never recorded: the resume halts on it.
	a := agenttest.MustNew(
		agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.TextTurn("done")),
		j,
		agent.WithTools(sideEffect()),
	)
	if _, err := j.Step(ctx, "r", "warm", func(context.Context) (int, error) { return 1, nil }, agent.WithSafety(agent.Safety{ReadOnly: true})); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "r", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	n := len(must(agenttest.MustJournal(m).History(ctx, "r")))
	cs.Reset()

	if _, err := a.Run(ctx, "r", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	wantCounts(t, cs, "resume of a finished run, same Journal", nil, 1, n)

	cold, err := agent.NewJournal(cs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("done")), cold, agent.WithTools(sideEffect())).Run(ctx, "r", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	wantCounts(t, cs, "resume of a finished run, new Journal", nil, 1, n)

	// A run halted on a claimed call: the resume finds the marker in its one Load and halts.
	if _, _, err := agent.ClaimAttempt(ctx, j, "h", "attempt:tool:c1", agent.Record{Kind: agent.StepAttempt, ToolUseID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := journaltest.Do(ctx, j, "h", "run:start", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: []byte(`{"input":"hi"}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := journaltest.Do(ctx, j, "h", "@llm/0", func(context.Context) (agent.Record, error) {
		msg := agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "c1", Name: "charge", Args: []byte(`{}`)}}}
		return agent.Record{Kind: agent.StepModel, Message: &msg}, nil
	}); err != nil {
		t.Fatal(err)
	}
	cs.Reset()
	_, err = a.Run(ctx, "h", agent.UserText("hi"))
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("resume = %v, want the halt", err)
	}
	wantCounts(t, cs, "resume of a halted run", nil, 1, 4)
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// A retry-safe Step reads its value and the marker an earlier attempt may have left (the
// attempt's recorded safety), then records its value. A side-effect Step reads its value, claims
// its attempt and records its value. A recorded Step costs one read.
func TestBudget_Step(t *testing.T) {
	ctx := context.Background()
	j, cs, _ := countingJournal(t)
	if _, err := j.Step(ctx, "r", "first", func(context.Context) (int, error) { return 1, nil }, agent.WithSafety(agent.Safety{ReadOnly: true})); err != nil {
		t.Fatal(err)
	}
	cs.Reset()
	if _, err := j.Step(ctx, "r", "read", func(context.Context) (int, error) { return 1, nil }, agent.WithSafety(agent.Safety{ReadOnly: true})); err != nil {
		t.Fatal(err)
	}
	wantCounts(t, cs, "retry-safe Step", []string{"get read", "get attempt:step:read", "insert read"}, 0, 0)
	if _, err := j.Step(ctx, "r", "write", func(context.Context) (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	wantCounts(t, cs, "side-effect Step", []string{"get write", "insert attempt:step:write", "insert write"}, 0, 0)
	if _, err := j.Step(ctx, "r", "write", func(context.Context) (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	wantCounts(t, cs, "recorded Step", []string{"get write"}, 0, 0)
}

// countingLister counts Runs calls.
type countingLister struct {
	*agent.MemStore
	calls atomic.Int64
}

func (c *countingLister) Runs(ctx context.Context, f agent.RunFilter) iter.Seq2[string, error] {
	c.calls.Add(1)
	return c.MemStore.Runs(ctx, f)
}

// A recovery pass lists the runs that are not over in one call, and reads nothing of the finished
// ones: the store filters them out. For each run it drives, it makes three point reads under the
// run's lease, one per end-of-run marker, to see that no other driver finished the run after the
// listing (a run it finds over there is not driven, and costs up to three). It loads no run. These
// reads raised this budget from none, with the maintainer's approval: without them a pass called
// resume for a run another driver finished after the listing.
func TestBudget_RecoverPass(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	const runs = 1000
	mj := agenttest.MustJournal(m)
	for i := range runs {
		id := fmt.Sprintf("run-%04d", i)
		name := "run:start"
		if i%2 == 0 {
			name = "run:complete"
		}
		if _, err := journaltest.Do(ctx, mj, id, name, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue, Result: []byte(`{"input":"x"}`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	l := &countingLister{MemStore: m}
	cs := agenttest.NewCountingStore(l)
	j, err := agent.NewJournal(cs)
	if err != nil {
		t.Fatal(err)
	}
	var driven atomic.Int64
	n, err := agent.Recover(ctx, j, func(context.Context, string, agent.RunStart) error { driven.Add(1); return nil }, agent.WithLeaseHolder("w"))
	if err != nil || n != runs/2 || driven.Load() != runs/2 {
		t.Fatalf("Recover = %d, %v (drove %d); want the %d unfinished runs", n, err, driven.Load(), runs/2)
	}
	if l.calls.Load() != 1 {
		t.Errorf("Recover listed runs %d times, want once", l.calls.Load())
	}
	want := make([]string, 0, 4*runs/2)
	for range runs / 2 {
		want = append(want, "get run:complete", "get run:aborted", "get run:cancelled", "get run:start")
	}
	wantCounts(t, cs, "recovery pass", want, 0, 0)
}
