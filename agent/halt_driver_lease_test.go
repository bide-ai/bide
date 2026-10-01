package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The live-driver check of a halt resolution leases the run whose driver holds the drive lease:
// a session turn's run for the turn and every sub-run inside it, the root run for an ordinary
// sub-agent's run. A lease on that run is a live driver (*HaltInFlight); a lease on another run
// that only shares a prefix of the ID (a root run named like the session) is not.
func TestHaltLiveCheck_LeasesTheRunItsDriverLeases(t *testing.T) {
	for _, c := range []struct {
		halted, driven, unrelated string
	}{
		{"c1>@turn/0", "c1>@turn/0", "c1"},
		{"c1>@turn/0>t1", "c1>@turn/0", "c1"},
		{"c1>@turn/0>t1>t2", "c1>@turn/0", "c1"},
		{"c1>@event/k>t1", "c1>@event/k", "c1"},
		{"root>t1", "root", "root>t1"},
		{"root>t1>t2", "root", "root>t1"},
		{"root", "root", "root>t1"},
	} {
		ctx := context.Background()
		ref := HaltRef{RunID: c.halted, Op: OpRef{Kind: OpTool, ID: "x"}, Cause: HaltCrashed}
		check := func(store *MemStore) error {
			release, _, err := checkNoLiveDriver(ctx, store, "ResolveHaltRef", ref, resolveConfig{now: time.Now})
			if err == nil {
				release()
			}
			return err
		}
		live := NewMemStore()
		if ok, err := live.AcquireLease(ctx, c.driven, "driver", time.Hour); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := check(live); !isHaltInFlight(err) {
			t.Errorf("halt in %s with %s leased by its driver: check = %v; want *HaltInFlight", c.halted, c.driven, err)
		}
		other := NewMemStore()
		if ok, err := other.AcquireLease(ctx, c.unrelated, "someone", time.Hour); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := check(other); err != nil {
			t.Errorf("halt in %s with only %s leased: check = %v; want nil (that lease is not its driver's)", c.halted, c.unrelated, err)
		}
	}
}

func isHaltInFlight(err error) bool {
	_, ok := errors.AsType[*HaltInFlight](err)
	return ok
}

// The check and the halt's RootRunID (rootRunID, from the run context) name one run for a tool
// call in a sub-agent called from a session turn and from a plain run, so the two cannot drift:
// with that run leased, the check on the call's run reports a live driver.
func TestHaltLiveCheck_AgreesWithTheCallsRootRunID(t *testing.T) {
	for _, viaSession := range []bool{false, true} {
		ctx := context.Background()
		store := NewMemStore()
		var info RunInfo
		probe := Func("probe", "", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
			info, _ = RunInfoFrom(ctx)
			return "ok", nil
		})
		sub := New(&scriptModel{turns: [][]Emit{toolTurn("t2", "probe", `{}`), textTurn("sub done")}}, store, probe)
		parent := New(&scriptModel{turns: [][]Emit{toolTurn("t1", "helper", `{"task":"x"}`), textTurn("done")}}, store, SubAgent("helper", "", sub))
		if viaSession {
			if _, err := openSession(t, parent, "c1").Send(ctx, "go"); err != nil {
				t.Fatal(err)
			}
		} else if _, err := parent.Run(ctx, "r1", "go"); err != nil {
			t.Fatal(err)
		}
		if ok, err := store.AcquireLease(ctx, info.RootRunID, "driver", time.Hour); err != nil || !ok {
			t.Fatal(ok, err)
		}
		ref := HaltRef{RunID: info.RunID, Op: OpRef{Kind: OpTool, ID: "t2"}, Cause: HaltCrashed}
		release, _, err := checkNoLiveDriver(ctx, store, "ResolveHaltRef", ref, resolveConfig{now: time.Now})
		if err == nil {
			release()
		}
		if !isHaltInFlight(err) {
			t.Errorf("session=%v: call in %s, its RootRunID %s leased: check = %v; want *HaltInFlight", viaSession, info.RunID, info.RootRunID, err)
		}
	}
}

// R137-2 on agent.run's own completed path: a finished run is final, but it answers the input
// its run:start recorded, so a drive with another input is ErrConfig (as it is for an unfinished
// run, #70) rather than the other input's answer. The same input still returns the answer
// without calling the model.
func TestRun_FinishedRunRefusesAnotherInput(t *testing.T) {
	ctx := context.Background()
	model := &replyModel{}
	a := New(model, NewMemStore())
	if msg, err := a.Run(ctx, "r1", "A"); err != nil || msg.Text() != "re: A" {
		t.Fatalf(`Run("A") = %q, %v`, msg.Text(), err)
	}
	if msg, err := a.Run(ctx, "r1", "B"); !errors.Is(err, ErrConfig) {
		t.Fatalf(`Run("B") of the run that answered "A" = %q, %v; want ErrConfig`, msg.Text(), err)
	}
	if msg, err := a.Run(ctx, "r1", "A"); err != nil || msg.Text() != "re: A" {
		t.Fatalf(`Run("A") again = %q, %v; want the recorded answer`, msg.Text(), err)
	}
	if n := model.calls.Load(); n != 1 {
		t.Fatalf("model calls = %d, want 1", n)
	}
}
