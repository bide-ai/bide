package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// A saga rollback that recurses into a wrapped sub-agent binds the wrapper's context first. When the
// wrapper cannot (the grant it narrowed to is gone), the delegation is reported uncompensated and
// the compensation inside it never runs: it must not run under authority the delegation did not
// grant.
func TestSaga_RollbackStopsWhenAWrapperCannotBindItsContext(t *testing.T) {
	ctx := context.Background()
	var compensated atomic.Int32
	pay := MustCompensatedFunc("pay", "", func(context.Context, struct{}) (string, error) { return "paid", nil },
		func(context.Context, struct{}, string) error { compensated.Add(1); return nil })
	book := MustFunc("book", "", func(context.Context, struct{}) (string, error) { return "", errors.New("no seats") })

	run := func(t *testing.T, bindErr error) (error, int32) {
		compensated.Store(0)
		subModel := &sagaTurns{turns: [][][3]string{{{"p1", "pay", `{}`}}}}
		sub := mustNew(subModel, memJournal(), WithTools(pay))
		inner := MustSubAgent("research", "d", sub)
		wrapped := covBinder{covWrapper{inner: inner, bind: func(ctx context.Context, id string) (context.Context, error) {
			return ctx, bindErr
		}}}
		parent := &sagaTurns{turns: [][][3]string{{{"c1", "research", `{"task":"look"}`}}, {{"c2", "book", `{}`}}}}
		a := mustNew(parent, memJournal(), WithTools(wrapped, book))
		_, err := a.Run(ctx, "r", UserText("go"), WithSaga())
		return err, compensated.Load()
	}

	t.Run("binds", func(t *testing.T) {
		err, comps := run(t, nil)
		var sa *SagaAborted
		if !errors.As(err, &sa) || sa.CompensateErr != nil || comps != 1 {
			t.Fatalf("Run = %v, %d compensations; want a clean abort that compensated the payment once", err, comps)
		}
	})
	t.Run("cannot bind", func(t *testing.T) {
		err, comps := run(t, errors.New("grant gone"))
		var sa *SagaAborted
		if !errors.As(err, &sa) {
			t.Fatalf("Run = %v; want *SagaAborted", err)
		}
		if sa.CompensateErr == nil || !strings.Contains(sa.CompensateErr.Error(), "grant gone") {
			t.Errorf("CompensateErr = %v; want the bind failure", sa.CompensateErr)
		}
		if !contains(sa.Uncompensated, "research") {
			t.Errorf("Uncompensated = %v; want the delegation named", sa.Uncompensated)
		}
		if comps != 0 {
			t.Errorf("%d compensations ran under a context that could not be bound", comps)
		}
	})
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// A saga whose history cannot be read is not driven: the error is the store's, and no model call is
// made on a run whose state is unknown.
func TestSaga_UnreadableHistoryIsNotDriven(t *testing.T) {
	m := &countModel{inner: &scriptModel{turns: [][]Emit{textTurn("x")}}}
	a := mustNew(m, mustJournal(loadFails{NewMemStore()}))
	if _, err := a.Run(context.Background(), "r", UserText("go"), WithSaga()); !errors.Is(err, ErrStorage) {
		t.Fatalf("Run = %v; want ErrStorage", err)
	}
	if m.calls.Load() != 0 {
		t.Fatal("the model was called for a saga whose history could not be read")
	}
}
