package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"testing"
)

// Deterministic Simulation Testing of the SAGA path: reverse-order compensation under
// crashes. Two guarantees, and they are DIFFERENT (this is the honest part):
//
//   - The forward non-idempotent effect (chargeA) fires AT MOST ONCE — same strong
//     guarantee as the base DST, halt-on-unknown, now in saga mode.
//   - Compensators are AT LEAST ONCE: they're durable memoized steps, so a compensator
//     that completes runs once, but a crash mid-compensation re-runs it — hence the
//     documented idempotency contract. So we assert the rollback COMPLETES (every
//     completed write is compensated) and the end state is correct, NOT that a compensator
//     runs exactly once.

// boomTool always errors — the step whose failure triggers the saga abort.
type boomTool struct{}

func (boomTool) Name() string                { return "failB" }
func (boomTool) Description() string         { return "" }
func (boomTool) Safety() Safety              { return Safety{ReadOnly: true} }
func (boomTool) ArgsSchema() json.RawMessage { return nil }
func (boomTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("failB always fails")
}

// sagaModel: charge first, then run the failing step (deterministic on the conversation).
type sagaModel struct{}

func (sagaModel) Stream(_ context.Context, req Request) (*Stream, error) {
	results := 0
	for _, m := range req.Messages {
		if m.Role == RoleTool {
			results++
		}
	}
	emits := toolTurn("a1", "chargeA", `{}`)
	if results > 0 { // chargeA already succeeded → now trip the failing step
		emits = toolTurn("b1", "failB", `{}`)
	}
	ch := make(chan Emit, len(emits))
	for _, e := range emits {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

func runSagaOnce(mem Durable, tools []Tool, crashAt int) error {
	a := New(sagaModel{}, &crashStore{inner: mem, crashAt: crashAt}, tools...).SetMaxConcurrency(1)
	_, err := a.RunSaga(context.Background(), "dst", "go")
	return err
}

// incompleteRollback reports a saga abort whose rollback crashed part-way (must resume).
func incompleteRollback(err error) bool {
	var sa *SagaAborted
	return errors.As(err, &sa) && sa.CompensateErr != nil
}

// chargeSaga builds the compensatable charge tool over fresh counters.
func chargeSaga(charge, refund *int) Tool {
	return CompensatedFunc("chargeA", "", Safety{}, // non-idempotent forward
		func(context.Context, struct{}) (struct{}, error) { *charge++; return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { *refund++; return nil })
}

// Crash at every write point (forward AND rollback), resume, and check: chargeA fires at
// most once; the run ends halted (forward crashed unknown) or fully rolled back; and a
// completed charge is always compensated. Both terminal paths must be exercised.
func TestDST_Saga_CrashSweep(t *testing.T) {
	haltSeen, abortSeen := false, false
	for crashAt := 1; crashAt <= 40; crashAt++ {
		var charge, refund int
		mem := NewMemStore()
		tools := []Tool{chargeSaga(&charge, &refund), boomTool{}}

		err := runSagaOnce(mem, tools, crashAt)
		crashed := errors.Is(err, errCrash) || incompleteRollback(err)
		for errors.Is(err, errCrash) || incompleteRollback(err) { // resume until settled
			err = runSagaOnce(mem, tools, 0)
		}

		if charge > 1 {
			t.Fatalf("crashAt=%d: chargeA fired %d times — DOUBLE FORWARD FIRE", crashAt, charge)
		}
		var halt *ResumeHalt
		var sa *SagaAborted
		switch {
		case errors.As(err, &halt):
			haltSeen = true // forward crashed with unknown outcome → halt, no auto-rollback
		case errors.As(err, &sa):
			abortSeen = true
			if charge != 1 {
				t.Fatalf("crashAt=%d: aborted but charged %d times, want 1", crashAt, charge)
			}
			if refund < 1 { // completeness: a completed write must be compensated
				t.Fatalf("crashAt=%d: aborted but never compensated (refund=%d)", crashAt, refund)
			}
		default:
			t.Fatalf("crashAt=%d: unexpected terminal %v (charge=%d refund=%d)", crashAt, err, charge, refund)
		}

		if !crashed { // crashAt exceeded the write count → sweep complete
			break
		}
	}
	if !haltSeen {
		t.Fatal("forward-crash halt path never exercised (test would be vacuous)")
	}
	if !abortSeen {
		t.Fatal("rollback path never exercised")
	}
}

// Adversarial: randomized multi-crash schedules over the saga. chargeA never double-fires;
// every settled abort leaves the charge compensated.
func TestDST_Saga_Randomized(t *testing.T) {
	for seed := uint64(1); seed <= 300; seed++ {
		rng := rand.New(rand.NewPCG(seed, 0xD1B54A32D192ED03))
		var charge, refund int
		mem := NewMemStore()
		tools := []Tool{chargeSaga(&charge, &refund), boomTool{}}

		var err error
		for attempt := 0; attempt < 60; attempt++ {
			err = runSagaOnce(mem, tools, rng.IntN(9)+1)
			if charge > 1 {
				t.Fatalf("seed=%d attempt=%d: DOUBLE FORWARD FIRE (charge=%d)", seed, attempt, charge)
			}
			if !errors.Is(err, errCrash) && !incompleteRollback(err) {
				break // settled
			}
		}
		var halt *ResumeHalt
		var sa *SagaAborted
		switch {
		case errors.As(err, &halt):
			// forward crashed unknown → halt; fine
		case errors.As(err, &sa):
			if charge == 1 && refund < 1 {
				t.Fatalf("seed=%d: aborted with a charge but no compensation", seed)
			}
		case errors.Is(err, errCrash) || incompleteRollback(err):
			// didn't settle within the attempt cap — invariant (charge<=1) still held throughout
		default:
			t.Fatalf("seed=%d: unexpected terminal %v", seed, err)
		}
	}
}
