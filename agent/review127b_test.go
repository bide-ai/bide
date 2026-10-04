package agent_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// adv127b builds: child agent (one compensable write "book") on childStore; parent saga on
// parentStore whose tool "starter" (call id starterID) runs the child as SubRunFor("child") with
// saga Run, then "boom" fails the saga.
func adv127b(t *testing.T, parentStore, childStore *agent.Journal, starterID string, declare bool, undone *int) *agent.Agent {
	t.Helper()
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { *undone++; return nil })
	child, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("k1", "book", `{}`), agenttest.TextTurn("child done")),
		childStore, agent.WithTools(book))
	if err != nil {
		t.Fatal(err)
	}
	var opts []agent.ToolOption
	if declare {
		opts = append(opts, agent.WithSubRuns(func(string) *agent.Agent { return child }))
	}
	starter := agent.MustFunc("starter", "", func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := agent.RunInfoFrom(ctx)
		res, err := child.Run(ctx, info.SubRunFor("child"), agent.UserText("work"), agent.WithSaga())
		var msg agent.Message
		if res != nil {
			msg = res.Message
		}
		return msg.Text(), err
	}, append([]agent.ToolOption{agent.WithSafety(agent.Safety{ReadOnly: true})}, opts...)...)
	boom := agent.MustFunc("boom", "", func(context.Context, struct{}) (string, error) {
		return "", errors.New("boom")
	})
	p, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn(starterID, "starter", `{}`), agenttest.ToolTurn("c2", "boom", `{}`), agenttest.TextTurn("x")),
		parentStore, agent.WithTools(starter, boom))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func adv127bAbort(t *testing.T, err error) *agent.SagaAborted {
	t.Helper()
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	return ab
}

// B1: a tool-use ID whose encoding is a digest (over 96 escaped bytes). The link is written under
// "@subrun/~<sha>/child", but subRunLinks decodes the tool-use segment and skips digests, so the
// rollback never walks the child: its write is neither compensated nor listed.
func TestAdv127b_LongToolUseIDLinkIgnored(t *testing.T) {
	for _, id := range []string{"c1", strings.Repeat("a", 97), strings.Repeat("|", 33)} {
		var undone int
		s := agenttest.MemJournal()
		_, err := adv127b(t, s, s, id, true, &undone).Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
		ab := adv127bAbort(t, err)
		if undone != 1 || !slices.Contains(ab.Compensated, "book") {
			t.Errorf("tool-use id len %d: undone %d, compensated %v, uncompensated %v, unknown %v; want book compensated",
				len(id), undone, ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
		}
	}
}

// B2 (decided): a saga's programmatic sub-run must journal to the parent's store, where the
// rollback reads it. A child agent on its own store is refused (ErrConfig) before it records or
// runs anything, so no write of it can be missed.
func TestAdv127b_ChildOnOwnStoreRefused(t *testing.T) {
	for _, declare := range []bool{false, true} {
		var undone int
		ps, cs := agenttest.MemJournal(), agent.NewMemStore()
		_, err := adv127b(t, ps, agenttest.MustJournal(cs), "c1", declare, &undone).Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
		ab := adv127bAbort(t, err)
		if !strings.Contains(ab.Cause.Error(), "another store") {
			t.Errorf("declare %v: abort cause %v, want the child refused for its store", declare, ab.Cause)
		}
		for id, lerr := range cs.Runs(context.Background(), agent.RunFilter{}) {
			t.Errorf("declare %v: the child's store holds run %q (%v); want none", declare, id, lerr)
		}
		if undone != 0 {
			t.Errorf("declare %v: undone %d, want 0 (the child never ran)", declare, undone)
		}
	}
}

// B2 (decided): at rollback, an agent from WithSubRuns that journals to another store than the
// run's cannot be used: the call is listed in Uncompensated with the reason, and the sub-run's
// writes are listed from its journal. A panicking WithSubRuns function is listed the same way.
func TestAdv127b_UnusableDeclarationListed(t *testing.T) {
	for _, mode := range []string{"other store", "panic"} {
		s := agenttest.MemJournal()
		book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
			func(context.Context, struct{}, string) error { t.Error("compensated by an unusable agent"); return nil })
		child, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("k1", "book", `{}`), agenttest.TextTurn("done")), s, agent.WithTools(book))
		if err != nil {
			t.Fatal(err)
		}
		elsewhere, err := agent.New(agenttest.NewScriptedModel(), agenttest.MemJournal(), agent.WithTools(book))
		if err != nil {
			t.Fatal(err)
		}
		declared := func(string) *agent.Agent {
			if mode == "panic" {
				panic("tenant lookup failed")
			}
			return elsewhere
		}
		starter := agent.MustFunc("starter", "", func(ctx context.Context, _ struct{}) (string, error) {
			info, _ := agent.RunInfoFrom(ctx)
			res, err := child.Run(ctx, info.SubRunFor("child"), agent.UserText("work"), agent.WithSaga())
			var msg agent.Message
			if res != nil {
				msg = res.Message
			}
			return msg.Text(), err
		}, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithSubRuns(declared))
		boom := agent.MustFunc("boom", "", func(context.Context, struct{}) (string, error) { return "", errors.New("boom") })
		p, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "starter", `{}`), agenttest.ToolTurn("c2", "boom", `{}`), agenttest.TextTurn("x")),
			s, agent.WithTools(starter, boom))
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
		ab := adv127bAbort(t, err)
		want := map[string]string{"other store": "another store", "panic": "tenant lookup failed"}[mode]
		listed := slices.ContainsFunc(ab.Uncompensated, func(u string) bool {
			return strings.HasPrefix(u, "starter (") && strings.Contains(u, want)
		})
		if !listed || !slices.Contains(ab.Uncompensated, "book") {
			t.Errorf("%s: uncompensated %v; want the starter call with its reason and the child's book", mode, ab.Uncompensated)
		}
	}
}

// B3: nesting through a non-saga intermediate. parent saga -> starter runs mid with Run (not
// saga Run) -> mid's tool runs leaf with saga Run as SubRunFor("leaf") -> leaf books. The parent's
// rollback walks mid's journal (and would undo mid's own writes), but mid's call context has
// saga=false, so no link to leaf was written: leaf's book is neither compensated nor listed.
// midSaga=true is the control.
func TestAdv127b_NestedThroughNonSagaChild(t *testing.T) {
	for _, midSaga := range []bool{true, false} {
		s := agenttest.MemJournal()
		var undone int
		book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
			func(context.Context, struct{}, string) error { undone++; return nil })
		leaf, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("k1", "book", `{}`), agenttest.TextTurn("leaf done")),
			s, agent.WithTools(book))
		if err != nil {
			t.Fatal(err)
		}
		spawn := agent.MustFunc("spawn", "", func(ctx context.Context, _ struct{}) (string, error) {
			info, _ := agent.RunInfoFrom(ctx)
			res, err := leaf.Run(ctx, info.SubRunFor("leaf"), agent.UserText("work"), agent.WithSaga())
			var msg agent.Message
			if res != nil {
				msg = res.Message
			}
			return msg.Text(), err
		}, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithSubRuns(func(string) *agent.Agent { return leaf }))
		mid, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("m1", "spawn", `{}`), agenttest.TextTurn("mid done")),
			s, agent.WithTools(spawn))
		if err != nil {
			t.Fatal(err)
		}
		starter := agent.MustFunc("starter", "", func(ctx context.Context, _ struct{}) (string, error) {
			info, _ := agent.RunInfoFrom(ctx)
			var opts []agent.RunOption
			if midSaga {
				opts = append(opts, agent.WithSaga())
			}
			msg, err := agenttest.Answer(mid.Run(ctx, info.SubRunFor("mid"), agent.UserText("work"), opts...))
			return msg.Text(), err
		}, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithSubRuns(func(string) *agent.Agent { return mid }))
		boom := agent.MustFunc("boom", "", func(context.Context, struct{}) (string, error) {
			return "", errors.New("boom")
		})
		p, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "starter", `{}`), agenttest.ToolTurn("c2", "boom", `{}`), agenttest.TextTurn("x")),
			s, agent.WithTools(starter, boom))
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
		ab := adv127bAbort(t, err)
		if undone != 1 && !slices.Contains(ab.Uncompensated, "book") {
			t.Errorf("mid as saga %v: undone %d, compensated %v, uncompensated %v, unknown %v; leaf's book is listed nowhere",
				midSaga, undone, ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
		}
	}
}

// B3, deeper: a saga's tree is inherited through any number of plain runs. root (saga) -> a
// (plain) -> b (plain) -> leaf (saga) books; the root's rollback reaches leaf through two plain
// runs' links and undoes the booking.
func TestAdv127b_NestedThroughTwoPlainRuns(t *testing.T) {
	s := agenttest.MemJournal()
	undone := 0
	book := agent.MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { undone++; return nil })
	build := func(m agent.Model, tools ...agent.Tool) *agent.Agent {
		a, err := agent.New(m, s, agent.WithTools(tools...))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	starts := func(tool, name string, sub *agent.Agent, saga bool) agent.Tool {
		return agent.MustFunc(tool, "", func(ctx context.Context, _ struct{}) (string, error) {
			info, _ := agent.RunInfoFrom(ctx)
			var opts []agent.RunOption
			if saga {
				opts = append(opts, agent.WithSaga())
			}
			msg, err := agenttest.Answer(sub.Run(ctx, info.SubRunFor(name), agent.UserText("work"), opts...))
			return msg.Text(), err
		}, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithSubRuns(func(string) *agent.Agent { return sub }))
	}
	leaf := build(agenttest.NewScriptedModel(agenttest.ToolTurn("k1", "book", `{}`), agenttest.TextTurn("leaf")), book)
	b := build(agenttest.NewScriptedModel(agenttest.ToolTurn("b1", "to_leaf", `{}`), agenttest.TextTurn("b")), starts("to_leaf", "leaf", leaf, true))
	a := build(agenttest.NewScriptedModel(agenttest.ToolTurn("a1", "to_b", `{}`), agenttest.TextTurn("a")), starts("to_b", "b", b, false))
	boom := agent.MustFunc("boom", "", func(context.Context, struct{}) (string, error) { return "", errors.New("boom") })
	root := build(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "to_a", `{}`), agenttest.ToolTurn("c2", "boom", `{}`), agenttest.TextTurn("x")),
		starts("to_a", "a", a, false), boom)
	_, err := root.Run(context.Background(), "root", agent.UserText("go"), agent.WithSaga())
	ab := adv127bAbort(t, err)
	if undone != 1 || !slices.Contains(ab.Compensated, "book") {
		t.Errorf("undone %d, compensated %v, uncompensated %v; want leaf's book undone", undone, ab.Compensated, ab.Uncompensated)
	}
}
