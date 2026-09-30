package postgres_test

// Review of PR #103 on Postgres: two store handles (two processes) drive the same flow runs
// concurrently; a side-effect node fires at most once, a start with another input loses with
// ErrConfig, and a completed flow run is still enumerated by Recover.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
	"github.com/bide-ai/bide/store/postgres"
)

func pgFlow(t *testing.T, fired *atomic.Int64) *plan.Flow[int, string] {
	b := plan.New[int, string]("pg")
	a := b.Step("a", func(_ context.Context, n int) (int, error) {
		fired.Add(1)
		time.Sleep(time.Millisecond)
		return n + 1, nil
	})
	yes := b.Step("yes", func(_ context.Context, n int) (string, error) { return fmt.Sprint("yes", n), nil }, plan.ReadOnly())
	no := b.Step("no", func(_ context.Context, n int) (string, error) { return fmt.Sprint("no", n), nil }, plan.ReadOnly())
	b.Switch(a, plan.When(func(v int) bool { return v > 1 }, yes), plan.Else(no))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRev103PG_ConcurrentFlowDrivers(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("PG_DSN not set")
	}
	ctx := context.Background()
	s1, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	stamp := time.Now().UnixNano()
	var halts, configs, oks int
	for i := range 60 {
		run := fmt.Sprintf("r103-%d-%d", stamp, i)
		var fired atomic.Int64
		flow := pgFlow(t, &fired)
		ins := []int{1, 1, 1, 1}
		if i%3 == 2 {
			ins[1], ins[3] = 5, 5
		}
		errs := make([]error, 4)
		outs := make([]string, 4)
		var wg sync.WaitGroup
		for k := range 4 {
			st := agent.Store(s1)
			if k%2 == 1 {
				st = s2
			}
			wg.Go(func() {
				j, err := agent.NewJournal(st)
				if err != nil {
					errs[k] = err
					return
				}
				outs[k], errs[k] = flow.Run(ctx, j, run, ins[k])
			})
		}
		wg.Wait()
		if fired.Load() > 1 {
			t.Fatalf("run %s: node a fired %d times: %v", run, fired.Load(), errs)
		}
		for k, err := range errs {
			switch {
			case err == nil:
				oks++
				if outs[k] != "yes2" && outs[k] != "yes6" {
					t.Fatalf("run %s: driver %d returned %q", run, k, outs[k])
				}
			case errors.Is(err, agent.ErrConfig):
				configs++
			default:
				if _, ok := errors.AsType[*agent.OutcomeUnknown](err); ok {
					halts++
				} else {
					t.Fatalf("run %s: driver %d: %v", run, k, err)
				}
			}
		}
	}
	t.Logf("240 drives: %d ok, %d halted (contended), %d ErrConfig (other input)", oks, halts, configs)

	// A completed flow run is enumerated by a recovery pass (the SQL filter excludes run:complete).
	run := fmt.Sprintf("r103-%d-done", stamp)
	var fired atomic.Int64
	flow := pgFlow(t, &fired)
	if _, err := flow.Run(ctx, s1, run, 1); err != nil {
		t.Fatal(err)
	}
	seen := false
	for id, err := range s1.Runs(ctx, agent.RunFilter{ExcludeHolding: []string{"run:complete", "run:aborted", "run:cancelled"}}) {
		if err != nil {
			t.Fatal(err)
		}
		if id == run {
			seen = true
		}
	}
	if seen {
		t.Errorf("a completed flow run %s is listed for recovery", run)
	}
}
