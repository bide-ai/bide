package agent

import (
	"context"
	"testing"
	"time"
)

// Two sub-agents of one root each wait on a timer of the same name, one due in an hour and one in
// two. Both wakes resume the root, so the Waker must hold both: if the later one replaced the
// earlier, the first sub-agent would sleep an hour past its wake time.
func TestWaker_SameNamedTimersInTwoSubRunsAreDistinct(t *testing.T) {
	for _, wait := range map[string]func(ctx context.Context, d time.Duration) error{
		"Sleep": func(ctx context.Context, d time.Duration) error { return Sleep(ctx, "nap", d) },
		"AwaitFor": func(ctx context.Context, d time.Duration) error {
			_, _, err := AwaitFor[string](ctx, "nap", d)
			return err
		},
	} {
		store := memJournal()
		t0 := time.Unix(1_000_000, 0)
		var resumed []string
		w := NewMemWaker(func(_ context.Context, runID string) error { resumed = append(resumed, runID); return nil })
		root := withRunContext(contextWithWaker(contextWithClock(context.Background(), func() time.Time { return t0 }), w), store, "root", "", false)
		for i, sub := range []string{"a", "b"} {
			ctx := withRunContext(root, store, SubRunID("root", sub), "", false)
			if err := wait(ctx, time.Duration(i+1)*time.Hour); !IsPause(err) {
				t.Fatalf("sub-run %s: wait = %v, want a pause", sub, err)
			}
		}
		if n, err := w.Fire(context.Background(), t0.Add(time.Hour)); n != 1 || err != nil {
			t.Fatalf("Fire at the first wake = %d, %v; want the root resumed (the second sub-run's timer replaced the first's)", n, err)
		}
		if n, err := w.Fire(context.Background(), t0.Add(2*time.Hour)); n != 1 || err != nil {
			t.Fatalf("Fire at the second wake = %d, %v; want the root resumed again", n, err)
		}
		if len(resumed) != 2 || resumed[0] != "root" || resumed[1] != "root" {
			t.Fatalf("resumed %v, want the root twice", resumed)
		}
	}
}
