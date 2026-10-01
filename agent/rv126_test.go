package agent

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Review of #126: a holder that writes run:complete and dies before its deferred ReleaseLease
// leaves a lapsed lease row on a finished run. Neither loop ever acquires (and so never releases)
// it: the lapsed listing excludes the finished run, and so does the full pass. The row stays for
// good, and every lapsed pass on every worker reads it again, so the lapsed loop's per-interval
// cost grows with every such crash.
func TestRV126_LapsedRowOnFinishedRunIsNeverReaped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		s := NewMemStore()
		seedRun(t, s, "f")
		if _, err := s.Do(ctx, "f", runCompleteStep, func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil }); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.AcquireLease(ctx, "f", "dead-worker#0", time.Millisecond); !ok {
			t.Fatal("setup: the dead worker should hold f")
		}
		stop := runLoop(t, s, func(context.Context, string) error { return nil }, WithRecoverInterval(20*time.Millisecond))
		time.Sleep(2 * time.Second) // a hundred passes of each loop
		synctest.Wait()
		_ = stop()
		s.mu.Lock()
		_, stale := s.leases["f"]
		s.mu.Unlock()
		if stale {
			t.Fatal("the lapsed lease row of finished run f is still in the store after 100 lapsed passes: nothing reaps it, so each one adds to every later lapsed pass's cost")
		}
	})
}
