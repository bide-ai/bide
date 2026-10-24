package agent

import (
	"context"
	"encoding/json"
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
		notStartedStep(toolAttemptStep("c1"), "abc123"), retryAttemptStep(toolAttemptStep("c1"), 1),
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
	if won, _, err := ClaimAttempt(ctx, h, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1"}); err != nil || !won {
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

// Pre-claiming the attempt marker of a capped call (the one attempt: form a caller may still claim) can only
// stop the call: the engine's own claim loses and the call halts without running (the safe direction), no
// reservation is left counted for it, and the reconcile pass changes nothing, since only a not-started record
// written by the winning claim voids an attempt (and ClaimAttempt refuses to write one, above).
func TestClaimAttempt_PreclaimedMarkerCannotChangeACapOutcome(t *testing.T) {
	j := memJournal()
	if won, _, err := ClaimAttempt(context.Background(), j, "r0", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1"}); err != nil || !won {
		t.Fatalf("pre-claim: won %v, err %v", won, err)
	}
	tool := sumTool(Safety{}, capRuleDay)
	_, err := runSum(t, j, "r0", sumArgs(60, ""), tool)
	if _, ok := errors.AsType[*OutcomeUnknown](err); !ok || tool.calls() != 0 {
		t.Fatalf("run = %v, calls %d; want a halt (OutcomeUnknown) and the tool not run", err, tool.calls())
	}
	if live := liveDay(t, j); len(live) != 0 {
		t.Fatalf("live = %+v, want no reservation left counted for the stopped call", live)
	}
	if released, err := reconcileCaps(capCtx(), j); err != nil || len(released) != 0 {
		t.Fatalf("reconcileCaps = %v, %v; want nothing released", released, err)
	}
}

// A not-started record planted for a live marker voids nothing, so the reconcile pass does not release the
// call's slot: a void must carry the claim id of the marker it names, and a planted record has a claim of
// its own. ClaimAttempt refuses the name; the unexported claim, which has no name check, is tried too.
func TestClaimAttempt_PlantedNotStartedDoesNotVoid(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	tool := sumTool(Safety{}, capRuleDay)
	spoil := func(next ToolHandler) ToolHandler { // the tool runs, then the chain fails: the call may have run
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if _, err := next(ctx, call); err != nil {
				return nil, err
			}
			return nil, errors.New("middleware failed after the tool ran")
		}
	}
	_, _ = runSum(t, j, "r0", sumArgs(60, ""), tool, WithToolMiddleware(spoil))
	if tool.calls() != 1 {
		t.Fatalf("calls = %d", tool.calls())
	}
	key := toolAttemptStep("c1")
	marker, ok, err := j.Get(ctx, "r0", key)
	if err != nil || !ok || marker.claim == "" {
		t.Fatalf("marker = %+v, %v, %v", marker, ok, err)
	}
	forged := notStartedStep(key, marker.claim)
	if _, _, err := ClaimAttempt(ctx, j, "r0", forged, Record{Kind: StepNotStarted}); !errors.Is(err, ErrConfig) {
		t.Fatalf("ClaimAttempt of a not-started name = %v, want ErrConfig", err)
	}
	if won, _, err := j.claim(ctx, "r0", forged, Record{Kind: StepNotStarted}); err != nil || !won {
		t.Fatalf("plant: won %v, err %v", won, err)
	}
	if released, err := reconcileCaps(capCtx(), j); err != nil || len(released) != 0 {
		t.Fatalf("reconcileCaps = %v, %v; want the planted record to void nothing", released, err)
	}
	if live := liveDay(t, j); len(live) != 1 {
		t.Fatalf("live = %+v, want the slot still counted", live)
	}
}
