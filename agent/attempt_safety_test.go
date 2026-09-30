package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// chargeOnce is a side effect that fires and then loses its answer, leaving an attempt marker
// with no result in the journal of run r1, as a crash between the two would.
func chargeOnce(t *testing.T, store Durable, charged *int) {
	t.Helper()
	charge := Func("charge", "charge the card", Safety{}, func(context.Context, struct{}) (string, error) {
		*charged++
		return "", fmt.Errorf("gateway connection reset (%w)", ErrToolOutcomeUnknown)
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	if _, err := New(m, store, charge).Run(context.Background(), "r1", "pay"); !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("first run err = %v, want ErrToolOutcomeUnknown", err)
	}
}

// A call's safety is the one it fired under. A side effect that fired with no recorded result
// must halt the resume even if the tool is now declared retry-safe (a trusted MCP server that
// relabels it read-only, or a code change), or the resume would run it a second time.
func TestResume_RelabelledRetrySafeStillHalts(t *testing.T) {
	store := NewMemStore()
	var charged int
	chargeOnce(t, store, &charged)

	relabelled := Func("charge", "charge the card", Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) {
		charged++
		return "charged", nil
	})
	m := &greedyModel{script: [][]Emit{textTurn("done")}} // the charge turn replays from the journal
	_, err := New(m, store, relabelled).Run(context.Background(), "r1", "pay")
	var halt *ResumeHalt
	if !errors.As(err, &halt) || halt.ToolUseID != "c1" {
		t.Fatalf("resume err = %v after %d charges, want *ResumeHalt for c1", err, charged)
	}
	if charged != 1 {
		t.Fatalf("charged %d times, want 1", charged)
	}
}

// The halt does not depend on the tool still being registered: the call fired, and its outcome
// is unknown whatever tools the resuming agent has.
func TestResume_AttemptedToolNoLongerRegisteredHalts(t *testing.T) {
	store := NewMemStore()
	var charged int
	chargeOnce(t, store, &charged)

	m := &greedyModel{script: [][]Emit{textTurn("done")}}
	_, err := New(m, store).Run(context.Background(), "r1", "pay")
	var halt *ResumeHalt
	if !errors.As(err, &halt) || halt.ToolUseID != "c1" || halt.ToolName != "charge" {
		t.Fatalf("resume err = %v, want *ResumeHalt for charge (c1)", err)
	}
}

// Step keeps the same rule: a step attempted as a side effect halts on resume even when the
// resuming code declares it retry-safe.
func TestStep_RelabelledRetrySafeStillHalts(t *testing.T) {
	store := NewMemStore()
	var ran int
	reserve := func(context.Context) (string, error) {
		ran++
		return "", errors.New("connection reset after the reservation was sent")
	}
	if _, err := Step(context.Background(), store, "r1", "reserve", reserve); err == nil {
		t.Fatal("first attempt succeeded, want its error")
	}
	_, err := Step(context.Background(), store, "r1", "reserve", reserve, StepSafety(Safety{Idempotent: true}))
	var halt *ResumeHalt
	if !errors.As(err, &halt) || halt.ToolUseID != "reserve" {
		t.Fatalf("resume err = %v after %d runs, want *ResumeHalt for reserve", err, ran)
	}
	if ran != 1 {
		t.Fatalf("the step ran %d times, want 1", ran)
	}
}

// markerLookupFails is a store whose lookups of step attempt markers fail.
type markerLookupFails struct{ *MemStore }

func (s markerLookupFails) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	if strings.HasPrefix(name, "attempt:step:") {
		return Record{}, fmt.Errorf("disk on fire (%w)", ErrStorage)
	}
	return s.MemStore.Do(ctx, runID, name, fn)
}

// If the store cannot say whether a retry-safe step was attempted before as a side effect, the
// step does not run: running it could be the second run of that side effect.
func TestStep_MarkerLookupFailureStopsTheStep(t *testing.T) {
	ran := 0
	_, err := Step(context.Background(), markerLookupFails{NewMemStore()}, "r1", "read", func(context.Context) (int, error) {
		ran++
		return 1, nil
	}, StepSafety(Safety{ReadOnly: true}))
	if !errors.Is(err, ErrStorage) || ran != 0 {
		t.Fatalf("err = %v after %d runs, want the store's error and no run", err, ran)
	}
}
