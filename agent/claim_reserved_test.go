package agent

import (
	"context"
	"errors"
	"testing"
)

// ClaimAttempt refuses a name under a reserved prefix other than an attempt marker, so a caller with
// journal access cannot plant an engine record (a cap-intent that pins a reservation to an old window, a
// gate-reason, an approval-pending): the refusal is ErrConfig and nothing is written.
func TestClaimAttempt_RefusesReservedNames(t *testing.T) {
	for _, name := range []string{
		"cap-intent:" + encodeID("c1"),
		gateReasonStep("c1"),
		ApprovalPendingStep("c1"),
		"cap-reserve:x", "slot:0", "taint/session", "run:start", "tool:c1", "@journal", "node:n", "audit:x",
		"",
	} {
		store := NewMemStore()
		j := mustJournal(store)
		won, _, err := ClaimAttempt(context.Background(), j, "r1", name, Record{Kind: StepAttempt})
		if won || !errors.Is(err, ErrConfig) {
			t.Errorf("ClaimAttempt(%q) = won %v, err %v; want ErrConfig and no win", name, won, err)
		}
		if _, ok, _ := j.Get(context.Background(), "r1", name); ok {
			t.Errorf("ClaimAttempt(%q) wrote the record", name)
		}
	}
}

// Attempt markers, which ClaimAttempt exists to write, and names the engine does not reserve are still claimed.
func TestClaimAttempt_StillClaimsAttemptMarkersAndPlainNames(t *testing.T) {
	j := mustJournal(NewMemStore())
	for _, name := range []string{toolAttemptStep("c1"), stepAttemptStep("s"), "attempt:x", "my-claim"} {
		if won, _, err := ClaimAttempt(context.Background(), j, "r1", name, Record{Kind: StepAttempt, ToolUseID: "c1"}); err != nil || !won {
			t.Errorf("ClaimAttempt(%q) = won %v, err %v; want a win", name, won, err)
		}
	}
}

// The engine's own writes of a reserved name do not go through the exported call: Journal.claim takes any
// name, and a capped tool call still reserves (cap-intent: and slot: are written) and a halt resolution
// still claims the next attempt.
func TestClaimAttempt_EnginePathsStillWriteReservedNames(t *testing.T) {
	ctx := context.Background()
	j := mustJournal(NewMemStore())
	if won, _, err := j.claim(ctx, "r1", "cap-intent:"+encodeID("c1"), Record{Kind: StepValue}); err != nil || !won {
		t.Fatalf("engine claim of a reserved name: won %v, err %v", won, err)
	}

	capped := memJournal()
	tool := sumTool(Safety{}, capRuleDay)
	if _, err := runSum(t, capped, "r0", sumArgs(60, ""), tool); err != nil || tool.calls() != 1 {
		t.Fatalf("capped call: %v, calls %d", err, tool.calls())
	}
	if live := liveDay(t, capped); len(live) != 1 {
		t.Fatalf("live = %+v, want the one reservation", live)
	}

	h := mustJournal(NewMemStore())
	if won, _, err := ClaimAttempt(ctx, h, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
		t.Fatalf("claim: won %v, err %v", won, err)
	}
	ref := HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
	if err := ResolveHalt(ctx, h, ref, Outcome{Result: "ok"}, WithMinHaltAge(0)); err != nil {
		t.Fatalf("ResolveHalt: %v", err)
	}
	if _, ok, _ := h.Get(ctx, "r1", nextAttemptStep(toolAttemptStep("c1"))); !ok {
		t.Fatal("the resolution did not claim the next attempt")
	}
}
