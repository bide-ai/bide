package chaos

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/durabletest"
)

// The crash-injecting wrapper, when it does not crash, hands back its inner store's record
// unchanged, so live and replay agree through it as they do on the store itself.
func TestCrashStore_Fidelity(t *testing.T) {
	durabletest.Run(t, func(*testing.T) agent.Durable { return &crashStore{inner: agent.NewMemStore()} })
}

// A crash is the process dying: after the injected crash, the wrapper runs nothing and persists
// nothing, so no later step of that attempt (a straggling goroutine, a loop that does not stop
// on the error) can have an effect. Bide's loop and the naive reference both return on the
// crash, so this is not observable through Verify today; it keeps the crash model the same as
// the ADK adapter's for any System built on crashStore.
func TestCrashStore_StaysDeadAfterTheCrash(t *testing.T) {
	ctx := context.Background()
	inner := agent.NewMemStore()
	cs := &crashStore{inner: inner, crashAt: 1}
	step := func(name string, ran *bool) error {
		_, err := cs.Do(ctx, "r", name, func(context.Context) (agent.Record, error) {
			*ran = true
			return agent.Record{Kind: agent.StepValue}, nil
		})
		return err
	}
	var first, second bool
	if err := step("a", &first); !errors.Is(err, errCrash) {
		t.Fatalf("write 1: err = %v, want the injected crash", err)
	}
	if err := step("b", &second); !errors.Is(err, errCrash) {
		t.Fatalf("a write after the crash returned %v, want the crash", err)
	}
	if second {
		t.Fatal("a step after the crash ran its side effect")
	}
	if recs, _ := inner.History(ctx, "r"); len(recs) != 0 {
		t.Fatalf("%d records persisted after the crash, want 0", len(recs))
	}
}

// A dead store does not replay either: a step recorded before the crash is not served after it.
func TestCrashStore_NoReplayAfterTheCrash(t *testing.T) {
	ctx := context.Background()
	cs := &crashStore{inner: agent.NewMemStore(), crashAt: 2}
	ok := func(context.Context) (agent.Record, error) { return agent.Record{Kind: agent.StepValue}, nil }
	if _, err := cs.Do(ctx, "r", "a", ok); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Do(ctx, "r", "b", ok); !errors.Is(err, errCrash) {
		t.Fatalf("write 2: err = %v, want the injected crash", err)
	}
	if _, err := cs.Do(ctx, "r", "a", ok); !errors.Is(err, errCrash) {
		t.Fatalf("replaying a step after the crash returned %v, want the crash", err)
	}
}

// gateStore holds a step named "late" until released, so a test can land the crash while that
// step is already inside the store.
type gateStore struct {
	agent.Durable
	entered, release chan struct{}
}

func (g gateStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	if name == "late" {
		close(g.entered)
		<-g.release
	}
	return g.Durable.Do(ctx, runID, name, fn)
}

// A step already inside the store when the crash lands does not run its side effect either.
func TestCrashStore_InFlightStepDoesNotRunAfterTheCrash(t *testing.T) {
	ctx := context.Background()
	g := gateStore{Durable: agent.NewMemStore(), entered: make(chan struct{}), release: make(chan struct{})}
	cs := &crashStore{inner: g, crashAt: 1}
	ran := false
	done := make(chan error)
	go func() {
		_, err := cs.Do(ctx, "r", "late", func(context.Context) (agent.Record, error) {
			ran = true
			return agent.Record{Kind: agent.StepValue}, nil
		})
		done <- err
	}()
	<-g.entered
	if _, err := cs.Do(ctx, "r", "first", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue}, nil
	}); !errors.Is(err, errCrash) {
		t.Fatalf("write 1: err = %v, want the injected crash", err)
	}
	close(g.release)
	if err := <-done; !errors.Is(err, errCrash) {
		t.Fatalf("the in-flight step returned %v, want the crash", err)
	}
	if ran {
		t.Fatal("the in-flight step ran its side effect after the crash")
	}
}
