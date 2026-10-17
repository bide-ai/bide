package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// syncTool is a retry-safe tool that does not watch its context: it runs whatever state the run
// is in by the time it is called.
type syncTool struct {
	name string
	fn   func() (json.RawMessage, error)
}

// Spec describes the tool to the agent (see agent.Tool).
func (t syncTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: t.name, Description: "", Input: json.RawMessage(`{"type":"object"}`), Safety: agent.Safety{Idempotent: true}}
}

func (t syncTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	return t.fn()
}

// In a sequential saga, a step fails; the call after it in the turn has not started, and must
// not start: the saga has already failed. It used to be called anyway, on the cancelled context,
// so a tool that does not watch its context (here a retry-safe one, which no attempt claim
// guards) ran its side effect after the transaction had failed.
func TestSaga_CallAfterTheFailureIsNotCalled(t *testing.T) {
	var fired atomic.Int32
	fail := syncTool{"fail", func() (json.RawMessage, error) { return nil, errors.New("no seats") }}
	notify := syncTool{"notify", func() (json.RawMessage, error) { fired.Add(1); return json.RawMessage(`{}`), nil }}
	turn := []agent.Emit{
		{Event: agent.ToolCallDelta{Index: 0, ID: "f1", Name: "fail", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.ToolCallDelta{Index: 1, ID: "n1", Name: "notify", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.Finish{Reason: "tool_use"}},
	}
	a := agenttest.MustNew(
		modelFunc(func() []agent.Emit { return turn }),
		agenttest.MemJournal(),
		agent.WithTools(fail, notify), agent.WithMaxConcurrency(1))
	_, err := a.Run(context.Background(), "r", agent.UserText("go"), agent.WithSaga())
	var aborted *agent.SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("saga Run = %v, want *SagaAborted", err)
	}
	if fired.Load() != 0 {
		t.Fatalf("notify ran %d times after the saga's step failed, want 0", fired.Load())
	}
}

// subRunFixture is a parent saga whose tool "starter" runs the programmatic sub-run "child" (an
// agent whose one write, "book", is compensable), then a tool "boom" fails the saga.
type subRunFixture struct {
	store   *agent.Journal
	undone  int
	undoErr error // returned by the next compensation, then cleared
}

// child builds the child agent, fresh each time (as a new process would).
func (f *subRunFixture) child(t *testing.T) *agent.Agent {
	t.Helper()
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error {
			if err := f.undoErr; err != nil {
				f.undoErr = nil
				return err
			}
			f.undone++
			return nil
		})
	c, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("k1", "book", `{}`), agenttest.TextTurn("child done")),
		f.store, agent.WithTools(book))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// parent builds the parent agent; failAfter makes "starter" fail after its sub-run finished, and
// declare gives it WithSubRuns.
func (f *subRunFixture) parent(t *testing.T, declare, failAfter bool) *agent.Agent {
	t.Helper()
	child := f.child(t)
	var opts []agent.ToolOption
	if declare {
		opts = append(opts, agent.WithSubRuns(func(name string) *agent.Agent {
			if name == "child" {
				return child
			}
			return nil
		}))
	}
	starter := agent.MustFunc("starter", "", func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := agent.RunInfoFrom(ctx)
		res, err := child.Run(ctx, info.SubRunFor("child"), agent.UserText("work"), agent.WithSaga())
		var msg agent.Message
		if res != nil {
			msg = res.Message
		}
		if err == nil && failAfter {
			err = errors.New("starter failed after its sub-run")
		}
		return msg.Text(), err
	}, append([]agent.ToolOption{agent.WithSafety(agent.Safety{ReadOnly: true})}, opts...)...)
	boom := agent.MustFunc("boom", "", func(context.Context, struct{}) (string, error) {
		return "", errors.New("boom")
	})
	turns := []agenttest.ScriptedTurn{agenttest.ToolTurn("c1", "starter", `{}`), agenttest.ToolTurn("c2", "boom", `{}`), agenttest.TextTurn("x")}
	p, err := agent.New(agenttest.NewScriptedModel(turns...), f.store, agent.WithTools(starter, boom))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func abortOf(t *testing.T, err error) *agent.SagaAborted {
	t.Helper()
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	return ab
}

// S1 (review of #127): a saga's rollback walks the programmatic sub-runs its calls started, as it
// walks a sub-agent's: with the agent the tool declares (WithSubRuns), the child's write is undone.
func TestSagaRollback_ProgrammaticSubRunCompensated(t *testing.T) {
	for _, failAfter := range []bool{false, true} {
		f := &subRunFixture{store: agenttest.MemJournal()}
		_, err := f.parent(t, true, failAfter).Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
		ab := abortOf(t, err)
		if f.undone != 1 || !slices.Equal(ab.Compensated, []string{"book"}) || len(ab.Uncompensated) != 0 || ab.CompensateErr != nil {
			t.Errorf("starter fails after its sub-run %v: undone %d; compensated %v, uncompensated %v, err %v; want book undone once",
				failAfter, f.undone, ab.Compensated, ab.Uncompensated, ab.CompensateErr)
		}
	}
}

// Without WithSubRuns the rollback cannot undo the child's write, and reports it.
func TestSagaRollback_UndeclaredSubRunReported(t *testing.T) {
	f := &subRunFixture{store: agenttest.MemJournal()}
	_, err := f.parent(t, false, false).Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
	ab := abortOf(t, err)
	if f.undone != 0 || !slices.Equal(ab.Uncompensated, []string{"book"}) || len(ab.Compensated) != 0 {
		t.Errorf("undeclared sub-run: undone %d; compensated %v, uncompensated %v; want book reported uncompensated",
			f.undone, ab.Compensated, ab.Uncompensated)
	}
}

// The link is durable, and the declaration is code: a rollback that stopped (a compensation
// failed) and is resumed by fresh agents, as after a restart, still undoes the child's write.
func TestSagaRollback_SubRunResumedByFreshAgents(t *testing.T) {
	f := &subRunFixture{store: agenttest.MemJournal(), undoErr: errors.New("booking service down")}
	_, err := f.parent(t, true, false).Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
	if ab := abortOf(t, err); ab.CompensateErr == nil || f.undone != 0 {
		t.Fatalf("first rollback: undone %d, err %v; want it stopped at the failed compensation", f.undone, ab.CompensateErr)
	}
	_, err = f.parent(t, true, false).Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
	if ab := abortOf(t, err); ab.CompensateErr != nil || f.undone != 1 || !slices.Equal(ab.Compensated, []string{"book"}) {
		t.Fatalf("resumed rollback: undone %d, compensated %v, err %v; want book undone", f.undone, ab.Compensated, ab.CompensateErr)
	}
}

// Only a saga's tree links its programmatic sub-runs: a plain run outside one writes no link. (A
// plain run started from a saga's call is in the tree and links its own; see
// TestAdv127b_NestedThroughNonSagaChild.)
func TestSubRunLink_OnlyInASagaTree(t *testing.T) {
	f := &subRunFixture{store: agenttest.MemJournal()}
	_, _ = f.parent(t, true, false).Run(context.Background(), "plain", agent.UserText("go"))
	recs, err := f.store.History(context.Background(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if strings.HasPrefix(r.Name, "@subrun/") {
			t.Errorf("a plain run recorded the link %s", r.Name)
		}
	}
	f = &subRunFixture{store: agenttest.MemJournal()}
	_, _ = f.parent(t, true, false).Run(context.Background(), "saga", agent.UserText("go"), agent.WithSaga())
	recs, _ = f.store.History(context.Background(), "saga")
	n := 0
	for _, r := range recs {
		if strings.HasPrefix(r.Name, "@subrun/") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("a saga recorded %d links, want 1", n)
	}
}

// Suspicion (c) of the review of #127: a programmatic sub-run started from a goroutine that
// outlived its tool call belongs to no call, so neither a rollback nor Recover would reach it; it
// is refused with ErrConfig, in a saga or not.
func TestSubRunFor_RefusedAfterTheCallReturned(t *testing.T) {
	for _, saga := range []bool{false, true} {
		store := agenttest.MemJournal()
		child, err := agent.New(agenttest.NewScriptedModel(agenttest.TextTurn("late")), store)
		if err != nil {
			t.Fatal(err)
		}
		release, result := make(chan struct{}), make(chan error, 1)
		spawn := agent.MustFunc("spawn", "", func(ctx context.Context, _ struct{}) (string, error) {
			info, _ := agent.RunInfoFrom(ctx)
			id := info.SubRunFor("late")
			go func() {
				<-release
				_, err := child.Run(ctx, id, agent.UserText("work"))
				result <- err
			}()
			return "spawned", nil
		}, agent.WithSafety(agent.Safety{ReadOnly: true}))
		p, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "spawn", `{}`), agenttest.TextTurn("done")),
			store, agent.WithTools(spawn))
		if err != nil {
			t.Fatal(err)
		}
		var opts []agent.RunOption
		if saga {
			opts = append(opts, agent.WithSaga())
		}
		if _, err := p.Run(context.Background(), "r", agent.UserText("go"), opts...); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := <-result; !errors.Is(err, agent.ErrConfig) {
			t.Errorf("saga %v: a sub-run started after its call returned: err %v, want ErrConfig", saga, err)
		}
	}
}

// In a saga, a programmatic sub-run's name must be short enough to be recovered from its link.
func TestSubRunFor_LongNameInASaga(t *testing.T) {
	store := agenttest.MemJournal()
	child, err := agent.New(agenttest.NewScriptedModel(agenttest.TextTurn("ok")), store)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("n", 200)
	starter := agent.MustFunc("starter", "", func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := agent.RunInfoFrom(ctx)
		_, err := child.Run(ctx, info.SubRunFor(long), agent.UserText("work"))
		if !errors.Is(err, agent.ErrConfig) {
			return "", errors.New("long name accepted in a saga: " + errString(err))
		}
		return "refused", nil
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	p, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "starter", `{}`), agenttest.TextTurn("done")),
		store, agent.WithTools(starter))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(context.Background(), "r", agent.UserText("go"), agent.WithSaga()); err != nil {
		t.Fatal(err)
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// WithSubRuns(nil) is ErrConfig, and SubAgent refuses the option.
func TestWithSubRuns_Refusals(t *testing.T) {
	sub, err := agent.New(agenttest.NewScriptedModel(), agenttest.MemJournal())
	if err != nil {
		t.Fatal(err)
	}
	for name, build := range map[string]func(){
		"nil function": func() {
			agent.MustFunc("f", "", func(context.Context, struct{}) (string, error) { return "", nil }, agent.WithSubRuns(nil))
		},
		"on a SubAgent": func() {
			agent.MustSubAgent("s", "", sub, agent.WithSubRuns(func(string) *agent.Agent { return sub }))
		},
	} {
		func() {
			defer func() {
				err, _ := recover().(error)
				if !errors.Is(err, agent.ErrConfig) {
					t.Errorf("%s: panic %v, want ErrConfig", name, err)
				}
			}()
			build()
		}()
	}
}

// A sub-agent's saga that aborts because its tool failed is the sub-agent's verdict, even when
// the tool's error wraps ErrStorage (a tool that writes to a database): the parent's saga aborts
// on it and settles. Only a failure of the sub-run's own journal is not a verdict (see
// TestRefModel_SubAgentStorageFailureIsNotItsAnswer).
func TestSubAgent_SagaAbortWrappingStorageIsAVerdict(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	save := agent.MustFunc("save", "", func(context.Context, struct{}) (struct{}, error) {
		return struct{}{}, fmt.Errorf("insert row: %w", agent.ErrStorage)
	})
	sub := agenttest.MustNew(
		agenttest.NewScriptedModel(agenttest.ToolTurn("s1", "save", `{}`), agenttest.TextTurn("saved")),
		store,
		agent.WithTools(save),
	)
	root := agenttest.MustNew(
		agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "saver", `{"task":"save it"}`), agenttest.TextTurn("done")),
		store,
		agent.WithTools(agent.MustSubAgent("saver", "", sub)),
	)
	for attempt := range 3 {
		_, err := root.Run(ctx, "r", agent.UserText("go"), agent.WithSaga())
		var sa *agent.SagaAborted
		if !errors.As(err, &sa) || sa.RunID != "r" || sa.CompensateErr != nil {
			t.Fatalf("attempt %d: saga Run = %v; want the root saga aborted, rolled back", attempt, err)
		}
	}
}
