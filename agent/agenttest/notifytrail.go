package agenttest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// NotifyTrailContract runs the agent.NotifyTrail contract. newStore returns a new, empty store as
// a function that returns a view of it: every call of the function is a NotifyTrail over the same
// store, as two processes' would be, so the concurrent case races views.
func NotifyTrailContract(t *testing.T, newStore func() func() agent.NotifyTrail) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	rec := func(key string, k agent.NotifyRecordKind) agent.NotifyRecord {
		return agent.NotifyRecord{Kind: k, Key: key, RunID: "r1", At: at}
	}

	t.Run("append and load in order", func(t *testing.T) {
		tr := newStore()()
		for i, k := range []agent.NotifyRecordKind{agent.NotifyObserved, agent.NotifyClaimed, agent.NotifyAcked} {
			if err := tr.Append(ctx, "notify:r1", i, rec("k", k)); err != nil {
				t.Fatalf("append %d: %v", i, err)
			}
		}
		got, err := tr.Load(ctx, "notify:r1")
		if err != nil || len(got) != 3 || got[0].Kind != agent.NotifyObserved || got[2].Kind != agent.NotifyAcked {
			t.Fatalf("load = %+v, %v", got, err)
		}
		if got, _ := tr.Load(ctx, "notify:none"); len(got) != 0 {
			t.Fatalf("unknown trail loaded %d records", len(got))
		}
	})

	t.Run("the same record again is a no-op, even in another zone", func(t *testing.T) {
		tr := newStore()()
		r := rec("k", agent.NotifyObserved)
		r.At = at.In(time.FixedZone("x", 3600))
		if err := tr.Append(ctx, "t", 0, r); err != nil {
			t.Fatal(err)
		}
		if err := tr.Append(ctx, "t", 0, rec("k", agent.NotifyObserved)); err != nil {
			t.Fatalf("re-append of the same record: %v", err)
		}
		got, _ := tr.Load(ctx, "t")
		if len(got) != 1 {
			t.Fatalf("trail has %d records, want 1", len(got))
		}
		if got[0].At.Location() != time.UTC || !got[0].At.Equal(at) {
			t.Fatalf("a stored time is %v, want %v in UTC", got[0].At, at)
		}
	})

	t.Run("a different record at a taken position is a conflict", func(t *testing.T) {
		tr := newStore()()
		if err := tr.Append(ctx, "t", 0, rec("k", agent.NotifyObserved)); err != nil {
			t.Fatal(err)
		}
		err := tr.Append(ctx, "t", 0, rec("other", agent.NotifyObserved))
		if !errors.Is(err, agent.ErrTrailConflict) {
			t.Fatalf("err = %v, want ErrTrailConflict", err)
		}
		if got, _ := tr.Load(ctx, "t"); len(got) != 1 || got[0].Key != "k" {
			t.Fatalf("the loser changed the trail: %+v", got)
		}
	})

	t.Run("a gap is an error but not a conflict", func(t *testing.T) {
		tr := newStore()()
		err := tr.Append(ctx, "t", 3, rec("k", agent.NotifyObserved))
		if err == nil || errors.Is(err, agent.ErrTrailConflict) {
			t.Fatalf("err = %v, want a non-conflict error", err)
		}
	})

	t.Run("trails are independent", func(t *testing.T) {
		tr := newStore()()
		if err := tr.Append(ctx, "a", 0, rec("k", agent.NotifyObserved)); err != nil {
			t.Fatal(err)
		}
		if err := tr.Append(ctx, "b", 0, rec("other", agent.NotifyObserved)); err != nil {
			t.Fatalf("a second trail's first position: %v", err)
		}
	})

	t.Run("a racing append has exactly one winner", func(t *testing.T) {
		view := newStore()
		const n = 8
		views := make([]agent.NotifyTrail, n)
		for i := range views {
			views[i] = view()
		}
		var wg sync.WaitGroup
		errs := make([]error, n)
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r := rec("k", agent.NotifyClaimed)
				r.Holder = string(rune('a' + i))
				errs[i] = views[i].Append(ctx, "race", 0, r)
			}()
		}
		close(start)
		wg.Wait()
		wins := 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, agent.ErrTrailConflict):
				t.Fatalf("a loser got %v, want ErrTrailConflict", err)
			}
		}
		if wins != 1 {
			t.Fatalf("%d winners at one position, want 1", wins)
		}
		if got, _ := views[0].Load(ctx, "race"); len(got) != 1 {
			t.Fatalf("trail has %d records, want 1", len(got))
		}
	})
}
