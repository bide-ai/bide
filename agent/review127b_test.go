package agent_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// adv127b builds: child agent (one compensable write "book") on childStore; parent saga on
// parentStore whose tool "starter" (call id starterID) runs the child as SubRunFor("child") with
// RunSaga, then "boom" fails the saga.
func adv127b(t *testing.T, parentStore, childStore *agent.MemStore, starterID string, declare bool, undone *int) *agent.Agent {
	t.Helper()
	book := agent.CompensatedFunc("book", "", agent.Safety{},
		func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { *undone++; return nil })
	child, err := agent.Build(agent.NewScriptedModel(agent.ToolTurn("k1", "book", `{}`), agent.TextTurn("child done")),
		childStore.Journal(), agent.WithTools(book))
	if err != nil {
		t.Fatal(err)
	}
	var opts []agent.ToolOption
	if declare {
		opts = append(opts, agent.WithSubRuns(func(string) *agent.Agent { return child }))
	}
	starter := agent.Func("starter", "", agent.Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := agent.RunInfoFrom(ctx)
		msg, err := child.RunSaga(ctx, info.SubRunFor("child"), "work")
		return msg.Text(), err
	}, opts...)
	boom := agent.Func("boom", "", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("boom")
	})
	p, err := agent.Build(agent.NewScriptedModel(agent.ToolTurn(starterID, "starter", `{}`), agent.ToolTurn("c2", "boom", `{}`), agent.TextTurn("x")),
		parentStore.Journal(), agent.WithTools(starter, boom))
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
		s := agent.NewMemStore()
		_, err := adv127b(t, s, s, id, true, &undone).RunSaga(context.Background(), "root", "go")
		ab := adv127bAbort(t, err)
		if undone != 1 || !slices.Contains(ab.Compensated, "book") {
			t.Errorf("tool-use id len %d: undone %d, compensated %v, uncompensated %v, unknown %v; want book compensated",
				len(id), undone, ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
		}
	}
}

// B2: the documented fallback (no WithSubRuns: the child's writes are listed in Uncompensated)
// reads the child's journal from the PARENT's store. A child agent on its own store is silently
// skipped.
func TestAdv127b_UndeclaredChildOnOwnStoreNotListed(t *testing.T) {
	var undone int
	ps, cs := agent.NewMemStore(), agent.NewMemStore()
	_, err := adv127b(t, ps, cs, "c1", false, &undone).RunSaga(context.Background(), "root", "go")
	ab := adv127bAbort(t, err)
	if !slices.Contains(ab.Uncompensated, "book") && !slices.Contains(ab.Compensated, "book") {
		t.Errorf("child on its own store: compensated %v, uncompensated %v, unknown %v; the child's book is listed nowhere",
			ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
	}
}

// B3: nesting through a non-saga intermediate. parent saga -> starter runs mid with Run (not
// RunSaga) -> mid's tool runs leaf with RunSaga as SubRunFor("leaf") -> leaf books. The parent's
// rollback walks mid's journal (and would undo mid's own writes), but mid's call context has
// saga=false, so no link to leaf was written: leaf's book is neither compensated nor listed.
// midSaga=true is the control.
func TestAdv127b_NestedThroughNonSagaChild(t *testing.T) {
	for _, midSaga := range []bool{true, false} {
		s := agent.NewMemStore()
		var undone int
		book := agent.CompensatedFunc("book", "", agent.Safety{},
			func(context.Context, struct{}) (string, error) { return "booked", nil },
			func(context.Context, struct{}, string) error { undone++; return nil })
		leaf, err := agent.Build(agent.NewScriptedModel(agent.ToolTurn("k1", "book", `{}`), agent.TextTurn("leaf done")),
			s.Journal(), agent.WithTools(book))
		if err != nil {
			t.Fatal(err)
		}
		spawn := agent.Func("spawn", "", agent.Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
			info, _ := agent.RunInfoFrom(ctx)
			msg, err := leaf.RunSaga(ctx, info.SubRunFor("leaf"), "work")
			return msg.Text(), err
		}, agent.WithSubRuns(func(string) *agent.Agent { return leaf }))
		mid, err := agent.Build(agent.NewScriptedModel(agent.ToolTurn("m1", "spawn", `{}`), agent.TextTurn("mid done")),
			s.Journal(), agent.WithTools(spawn))
		if err != nil {
			t.Fatal(err)
		}
		starter := agent.Func("starter", "", agent.Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
			info, _ := agent.RunInfoFrom(ctx)
			run := mid.Run
			if midSaga {
				run = mid.RunSaga
			}
			msg, err := run(ctx, info.SubRunFor("mid"), "work")
			return msg.Text(), err
		}, agent.WithSubRuns(func(string) *agent.Agent { return mid }))
		boom := agent.Func("boom", "", agent.Safety{}, func(context.Context, struct{}) (string, error) {
			return "", errors.New("boom")
		})
		p, err := agent.Build(agent.NewScriptedModel(agent.ToolTurn("c1", "starter", `{}`), agent.ToolTurn("c2", "boom", `{}`), agent.TextTurn("x")),
			s.Journal(), agent.WithTools(starter, boom))
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.RunSaga(context.Background(), "root", "go")
		ab := adv127bAbort(t, err)
		if undone != 1 && !slices.Contains(ab.Uncompensated, "book") {
			t.Errorf("mid as saga %v: undone %d, compensated %v, uncompensated %v, unknown %v; leaf's book is listed nowhere",
				midSaga, undone, ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
		}
	}
}
