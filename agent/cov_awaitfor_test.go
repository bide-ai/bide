package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// AwaitFor outside a run is ErrConfig. In a run, a recorded deadline or outcome that does not decode
// is ErrProtocol, a signal payload of another type than the tool asks for is ErrProtocol, and a
// store that cannot record the deadline is ErrStorage: none of them is a timeout or an arrival.
func TestAwaitFor_FaultsAreNotOutcomes(t *testing.T) {
	ctx := context.Background()
	if _, _, err := AwaitFor[string](ctx, "sig", time.Hour); !errors.Is(err, ErrConfig) {
		t.Errorf("AwaitFor outside a run = %v; want ErrConfig", err)
	}
	run := func(j *Journal) context.Context { return withRunContext(ctx, j, "r", "", false) }

	t.Run("corrupt deadline", func(t *testing.T) {
		j := memJournal()
		writeRaw(t, j, "r", awaitTimeoutStep("sig"), Record{Kind: StepValue, Result: []byte(`"soon"`)})
		if _, ok, err := AwaitFor[string](run(j), "sig", time.Hour); !errors.Is(err, ErrProtocol) || ok {
			t.Errorf("= %v, %v; want ErrProtocol", ok, err)
		}
	})
	t.Run("corrupt outcome", func(t *testing.T) {
		j := memJournal()
		writeRaw(t, j, "r", awaitResolvedStep("sig"), Record{Kind: StepValue, Result: []byte(`[1]`)})
		if _, ok, err := AwaitFor[string](run(j), "sig", time.Hour); !errors.Is(err, ErrProtocol) || ok {
			t.Errorf("= %v, %v; want ErrProtocol", ok, err)
		}
	})
	t.Run("payload of another type", func(t *testing.T) {
		j := memJournal()
		if err := j.Signal(ctx, "r", "sig", "text"); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := AwaitFor[int](run(j), "sig", time.Hour); !errors.Is(err, ErrProtocol) || ok {
			t.Errorf("= %v, %v; want ErrProtocol", ok, err)
		}
	})
	t.Run("deadline cannot be recorded", func(t *testing.T) {
		j := mustJournal(covInsertFail{NewMemStore(), awaitTimeoutStep("sig")})
		if _, ok, err := AwaitFor[string](run(j), "sig", time.Hour); !errors.Is(err, ErrStorage) || ok {
			t.Errorf("= %v, %v; want ErrStorage", ok, err)
		}
	})
	t.Run("race cannot be resolved", func(t *testing.T) {
		j := mustJournal(covInsertFail{NewMemStore(), awaitResolvedStep("sig")})
		now := time.Now()
		// The deadline has passed, so the timeout would be recorded; the store refuses it.
		rc := contextWithClock(run(j), func() time.Time { return now })
		if _, _, err := AwaitFor[string](rc, "sig", -time.Hour); !errors.Is(err, ErrStorage) {
			t.Errorf("= %v; want ErrStorage", err)
		}
	})
	t.Run("history unreadable", func(t *testing.T) {
		fj := mustJournal(loadFails{NewMemStore()})
		if _, _, err := AwaitFor[string](run(fj), "sig", time.Hour); !errors.Is(err, ErrStorage) {
			t.Errorf("= %v; want ErrStorage", err)
		}
	})
}
