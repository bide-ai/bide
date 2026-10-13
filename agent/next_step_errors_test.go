package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A newcomer's errors say what to do next: each message below names the call or the choice that
// gets the user unstuck, and its sentinel still matches.
func TestErrorsSayWhatToDoNext(t *testing.T) {
	var charged int
	store, _, _ := finishedCharge(t, &charged)
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
	_, err := mustNew(&greedyModel{}, store, WithTools(charge)).Run(context.Background(), "r1", UserText("a different input"))
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "new run ID") {
		t.Errorf("finished run, other input: %v; want ErrConfig naming a new run ID", err)
	}

	halt := toolHalt("r1", "r1", "call-1", "charge", time.Time{}, HaltCrashed)
	if msg := halt.Error(); !strings.Contains(msg, "resume halted") || !strings.Contains(msg, "ResolveHalt") {
		t.Errorf("OutcomeUnknown = %q; want it to name ResolveHalt", msg)
	}
	if msg := (&ApprovalPending{RunRef: RunRef{RunID: "r1"}, ToolName: "refund", ToolUseID: "c1"}).Error(); !strings.Contains(msg, "SubmitDecision") {
		t.Errorf("ApprovalPending = %q; want it to name SubmitDecision", msg)
	}
	if _, err := New(nil, nil); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "NewScriptedModel") {
		t.Errorf("New(nil model) = %v; want ErrConfig pointing at the scripted model", err)
	}
}
