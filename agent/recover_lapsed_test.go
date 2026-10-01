package agent

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

// Finding L1 of model 10 (spec/tla/README.md, "Recovery cost: the pickup bound"): a RecoverLoop
// pass visits every unfinished run it lists, halted ones included, one slot at a time, so a dead
// holder's run listed after halted runs waited for their visits, and a lease that lapsed just
// after the pass tried the run waited for the whole next pass. A dead holder's run must be taken
// over within one interval of its lease lapsing, however many halted runs are listed before it.
//
// The halted runs h1..hH sort before the dead holder's run z, as older runs do; each visit to a
// halted run takes one interval (its resume replays to the halt and pauses again), and the full
// pass drives one run at a time. The dead holder's lease lapses at every phase of the pass, in
// steps of a quarter interval, and the takeover is measured from the lapse to the moment resume
// is called for z, on the bubble's clock.
func TestRecoverLoop_TakesOverALapsedLeaseWithinAnIntervalBehindHaltedRuns(t *testing.T) {
	const (
		interval = 20 * time.Millisecond
		visit    = interval // one halted run's visit
		step     = interval / 4
	)
	for halted := 0; halted <= 3; halted++ {
		for phase := time.Duration(0); phase <= time.Duration(halted+2)*visit; phase += step {
			t.Run(fmt.Sprintf("halted=%d/lapse=+%v", halted, phase), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					if d := lapsedTakeover(t, halted, visit, interval, 100*time.Millisecond+phase); d > interval {
						t.Fatalf("the dead holder's run was taken over %v after its lease lapsed, behind %d halted runs; want within one interval (%v)", d, halted, interval)
					}
				})
			})
		}
	}
}

// lapsedTakeover runs RecoverLoop at concurrency 1 over halted runs h1..hN and a run z whose dead
// holder's lease lapses deadTTL after the loop starts, and returns how long after the lapse the
// loop called resume for z.
func lapsedTakeover(t *testing.T, halted int, visit, interval, deadTTL time.Duration) time.Duration {
	t.Helper()
	ctx := context.Background()
	s := NewMemStore()
	for i := 1; i <= halted; i++ {
		seedRun(t, s, fmt.Sprintf("h%d", i))
	}
	seedRun(t, s, "z")
	if ok, _ := s.AcquireLease(ctx, "z", "dead-worker#0", deadTTL); !ok {
		t.Fatal("setup: the dead worker should hold z")
	}
	lapse := time.Now().Add(deadTTL)
	drivenAt := make(chan time.Time, 1)
	stop := runLoop(t, s, func(ctx context.Context, id string) error {
		if id != "z" {
			time.Sleep(visit)
			return &OutcomeUnknown{RunRef: RunRef{RunID: id}} // halted: stays unfinished
		}
		select {
		case drivenAt <- time.Now():
		default:
		}
		_, err := s.Do(ctx, id, runCompleteStep, func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil })
		return err
	}, WithRecoverInterval(interval), WithRecoverConcurrency(1), WithLeaseTTL(10*interval))
	defer stop()
	select {
	case at := <-drivenAt:
		if at.Before(lapse) {
			t.Fatalf("the loop drove z %v before its dead holder's lease lapsed", lapse.Sub(at))
		}
		return at.Sub(lapse)
	case <-time.After(time.Minute):
		t.Fatal("the loop never took over the dead holder's run")
		return 0
	}
}
