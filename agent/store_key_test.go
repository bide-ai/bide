package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// overlappingDo runs Do for (run1, name1) with a step that waits, then, while it waits, Do for
// (run2, name2), and returns the record each call got back and whether the second step ran. The
// first step is held until the second call has had time to reach the store, so a store that
// keyed the two steps alike would hand the second caller the first step's record.
func overlappingDo(t *testing.T, d Durable, run1, name1, run2, name2 string) (first, second Record, secondRan bool) {
	t.Helper()
	ctx := context.Background()
	release := make(chan struct{})
	started := make(chan struct{})
	done := make(chan Record, 1)
	go func() {
		rec, err := d.Do(ctx, run1, name1, func(context.Context) (Record, error) {
			close(started)
			<-release
			return Record{Kind: StepValue, Result: json.RawMessage(`"first"`)}, nil
		})
		if err != nil {
			t.Errorf("Do(%q, %q): %v", run1, name1, err)
		}
		done <- rec
	}()
	<-started
	secondDone := make(chan Record, 1)
	go func() {
		rec, err := d.Do(ctx, run2, name2, func(context.Context) (Record, error) {
			secondRan = true
			return Record{Kind: StepValue, Result: json.RawMessage(`"second"`)}, nil
		})
		if err != nil {
			t.Errorf("Do(%q, %q): %v", run2, name2, err)
		}
		secondDone <- rec
	}()
	select {
	case second = <-secondDone: // the second call did not wait on the first: nothing shared
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	first = <-done
	if second.Name == "" {
		second = <-secondDone
	}
	return first, second, secondRan
}

// Two different steps are never one step, whatever bytes their run IDs and names hold. The
// store's in-process deduplication key must tell ("a\x00b", "c") from ("a", "b\x00c"): keyed
// alike, a concurrent Do of the second would share the first's fn and get its record back.
func TestMemStore_DistinctStepsWithNULDoNotShareADo(t *testing.T) {
	for _, c := range [][4]string{{"a\x00b", "c", "a", "b\x00c"}, {"ab", "c", "a", "bc"}} {
		first, second, ran := overlappingDo(t, NewMemStore(), c[0], c[1], c[2], c[3])
		if string(first.Result) != `"first"` {
			t.Errorf("%q: first step's record = %s, want \"first\"", c, first.Result)
		}
		if !ran || string(second.Result) != `"second"` || second.Name != c[3] {
			t.Errorf("%q: second step ran=%v, record %q %s; want its own fn run and record \"second\"", c, ran, second.Name, second.Result)
		}
	}
}

// Two timers of two runs are two wakes, whatever bytes the run IDs and timer names hold:
// ("a\x00b", "c") and ("a", "b\x00c") keyed alike would let one registration overwrite the
// other, and the overwritten run would never wake.
func TestMemWaker_DistinctTimersWithNULAreKeptApart(t *testing.T) {
	woke := map[string]bool{}
	w := NewMemWaker(func(_ context.Context, runID string) error {
		woke[runID] = true
		return nil
	})
	at := time.Unix(100, 0)
	w.Schedule("a\x00b", "c", at)
	w.Schedule("a", "b\x00c", at)
	w.Schedule("xy", "z", at)
	w.Schedule("x", "yz", at)
	if n, err := w.Fire(context.Background(), at); err != nil || n != 4 {
		t.Fatalf("Fire = %d, %v; want both runs resumed", n, err)
	}
	if !woke["a\x00b"] || !woke["a"] || !woke["xy"] || !woke["x"] {
		t.Fatalf("woke %v; want all four runs", woke)
	}
}
