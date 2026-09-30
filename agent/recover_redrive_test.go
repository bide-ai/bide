package agent

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
)

// A recovery pass lists the runs that are not over, then leases and drives each in turn. Another
// driver can finish a listed run while the pass waits (for a drive slot, or for the drive before
// it); the pass must then see, under the lease, that the run is over and leave it alone, rather
// than call resume for a finished run.

// completeUnderLease drives runID to completion as another holder would: under its own lease,
// through Agent.Run, which writes run:complete and then releases the lease.
func completeUnderLease(t *testing.T, s *MemStore, runID string) {
	t.Helper()
	driven, err := Lease(context.Background(), s, runID, func(ctx context.Context) error {
		_, err := New(NewScriptedModel(TextTurn("done")), s).Run(ctx, runID, "go")
		return err
	}, WithLeaseHolder("other"))
	if err != nil || !driven {
		t.Fatalf("other holder's drive of %s: driven=%v err=%v", runID, driven, err)
	}
	if done, err := IsComplete(context.Background(), s, runID); err != nil || !done {
		t.Fatalf("run %s not complete after the other holder's drive: %v %v", runID, done, err)
	}
}

// endUnderLease ends runID as another holder would: under its own lease, it records the terminal
// marker name and then releases the lease.
func endUnderLease(t *testing.T, s *MemStore, runID, name string) {
	t.Helper()
	driven, err := Lease(context.Background(), s, runID, func(ctx context.Context) error {
		_, err := putRecord(ctx, s, runID, name, Record{Kind: StepValue})
		return err
	}, WithLeaseHolder("other"))
	if err != nil || !driven {
		t.Fatalf("other holder's %s of %s: driven=%v err=%v", name, runID, driven, err)
	}
}

func TestRecoverLoop_DoesNotResumeARunCompletedWhileItWaited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewMemStore()
		for _, id := range []string{"a", "b"} {
			seedRun(t, s, id)
		}
		first, release := make(chan string, 1), make(chan struct{})
		var mu sync.Mutex
		var resumedComplete []string
		stop := runLoop(t, s, func(ctx context.Context, id string) error {
			if done, _ := IsComplete(ctx, s, id); done {
				mu.Lock()
				resumedComplete = append(resumedComplete, id)
				mu.Unlock()
				return nil
			}
			select {
			case first <- id: // the first run driven holds the only slot until released
				<-release
			default:
			}
			return nil
		}, WithLeaseHolder("w"), WithRecoverConcurrency(1))
		held := <-first
		synctest.Wait() // the pass has listed the other run as unfinished and waits for the slot
		other := "a"
		if held == "a" {
			other = "b"
		}
		completeUnderLease(t, s, other)
		close(release)
		synctest.Wait() // the pass has finished; the next waits on the (fake) ticker
		if err := stop(); err != nil && err != context.Canceled {
			t.Fatal(err)
		}
		if len(resumedComplete) > 0 {
			t.Errorf("RecoverLoop called resume for %v after it completed", resumedComplete)
		}
	})
}

func TestRecover_DoesNotResumeARunCompletedWhileItWaited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewMemStore()
		for _, id := range []string{"a", "b"} {
			seedRun(t, s, id)
		}
		var resumedComplete []string
		n, err := Recover(context.Background(), s, func(ctx context.Context, id string) error {
			if done, _ := IsComplete(ctx, s, id); done {
				resumedComplete = append(resumedComplete, id)
				// The documented resume: Run replays a finished run and returns its answer.
				before, _ := s.History(ctx, id)
				_, err := New(NewScriptedModel(), s).Run(ctx, id, "go")
				after, _ := s.History(ctx, id)
				if len(after) != len(before) {
					t.Errorf("replaying completed run %s wrote %d records", id, len(after)-len(before))
				}
				return err
			}
			if id == "a" {
				completeUnderLease(t, s, "b") // b completes while this pass drives a
			}
			return nil
		}, WithLeaseHolder("w"))
		if err != nil {
			t.Fatal(err)
		}
		if len(resumedComplete) > 0 {
			t.Errorf("Recover called resume for %v after it completed", resumedComplete)
		}
		if n != 1 {
			t.Errorf("Recover = %d, want 1: b was over before the pass drove it", n)
		}
	})
}

// Every marker that ends a run, not only run:complete, is checked under the lease.
func TestRecover_DoesNotResumeARunEndedWhileItWaited(t *testing.T) {
	for _, name := range []string{runCompleteStep, runAbortedStep, runCancelledStep} {
		t.Run(name, func(t *testing.T) {
			s := NewMemStore()
			for _, id := range []string{"a", "b"} {
				seedRun(t, s, id)
			}
			var resumed []string
			n, err := Recover(context.Background(), s, func(ctx context.Context, id string) error {
				resumed = append(resumed, id)
				if id == "a" {
					endUnderLease(t, s, "b", name) // b ends while this pass drives a
				}
				return nil
			}, WithLeaseHolder("w"))
			if err != nil {
				t.Fatal(err)
			}
			if len(resumed) != 1 || resumed[0] != "a" || n != 1 {
				t.Errorf("Recover resumed %v (reported %d), want only a: b held %s before the pass leased it", resumed, n, name)
			}
		})
	}
}

// The markers the check reads are the ones the engine writes to end a run, and the ones the
// listing excludes.
func TestRecoverFilter_HoldsEveryEndOfRunMarker(t *testing.T) {
	want := []string{runCompleteStep, runAbortedStep, runCancelledStep}
	if len(recoverFilter.ExcludeHolding) != len(want) {
		t.Fatalf("recoverFilter.ExcludeHolding = %v, want %v", recoverFilter.ExcludeHolding, want)
	}
	for i, n := range want {
		if recoverFilter.ExcludeHolding[i] != n {
			t.Fatalf("recoverFilter.ExcludeHolding = %v, want %v", recoverFilter.ExcludeHolding, want)
		}
	}
}
