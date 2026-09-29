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
