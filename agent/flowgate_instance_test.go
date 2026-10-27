package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/internal/journalhook"
)

// The escalation instance of a plan flow node (docs/design/flow-taint.md) is a gate subject that is not a
// node key: "node:pay:taint", "node:iter:2:pay:taint". The gate machinery takes it as a subject, and nothing
// else treats it as a node. These tests use synthetic journals; the engine creates no instance yet.

func TestPlanGateKey(t *testing.T) {
	for key, want := range map[string]bool{
		"node:pay":              true,
		"node:iter:2:pay":       true,
		"node:pay:taint":        true,
		"node:iter:2:pay:taint": true,
		"node:iter:taint":       true, // the instance of a node named iter
		"node:taint":            true, // the node named taint
		"node:pay:step:taint":   false,
		"node:pay:step:fetch":   false,
		"node:pay:taint:taint":  false,
		"node:pay#taint":        true, // a node whose name holds '#': a node
		"tool:pay":              false,
		"c1":                    false,
		"c1#taint":              false, // a tool call's instance is a call's, not a flow subject
		"node:":                 false,
	} {
		if got := planGateKey(key); got != want {
			t.Errorf("planGateKey(%q) = %v, want %v", key, got, want)
		}
	}
	// A node key stays a node key, and an instance is not one: the Step hook and ResolveHalt keep node-only.
	if planNodeKey("node:pay:taint") || planNodeStep("node:pay:taint") {
		t.Error("an escalation instance is a node key")
	}
	if !planNodeKey("node:pay") || !planNodeKey("node:iter:3:pay") {
		t.Error("a node key is not a node key")
	}
}

func TestFlowGate_RunsOnAnInstanceKey(t *testing.T) {
	ctx := context.Background()
	for _, key := range []string{"node:pay:taint", "node:iter:2:pay:taint"} {
		j := memJournal()
		args := json.RawMessage(`{"amount":1}`)
		// A decision before the gate recorded its request is refused, as for a node.
		if err := Approve(ctx, j, "r1", key, true); !errors.Is(err, ErrConfig) {
			t.Fatalf("%s: Approve before the request = %v, want ErrConfig", key, err)
		}
		var pend *ApprovalPending
		if _, err := flowGate(ctx, j, "r1", key, "pay", args, SingleApproval(), nil); !errors.As(err, &pend) {
			t.Fatalf("%s: first evaluation = %v, want a pause", key, err)
		}
		if _, ok, _ := j.Get(ctx, "r1", ApprovalRequestStep(key)); !ok {
			t.Errorf("%s: the request is not recorded under the instance's own name", key)
		}
		if err := Approve(ctx, j, "r1", key, true); err != nil {
			t.Fatalf("%s: Approve after the request = %v", key, err)
		}
		if ok, err := flowGate(ctx, j, "r1", key, "pay", args, SingleApproval(), nil); err != nil || !ok {
			t.Fatalf("%s: gate after Approve = %v, %v", key, ok, err)
		}
	}
	// Not an instance, not a node: still refused.
	for _, key := range []string{"node:pay:step:taint", "node:pay:taint:taint", "tool:pay", "c1#taint"} {
		if _, err := flowGate(ctx, memJournal(), "r1", key, "pay", nil, SingleApproval(), nil); !errors.Is(err, ErrConfig) {
			t.Errorf("flowGate on %q = %v, want ErrConfig", key, err)
		}
	}
}

// A node's own approval does not pass its instance, and the reverse: each gate counts decisions recorded
// under its own subject, and a signature binds the subject, so one cannot stand in for the other.
func TestFlowGate_NodeAndInstanceDoNotShareDecisions(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	args := json.RawMessage(`{"amount":1}`)
	const node, inst = "node:pay", "node:pay:taint"
	var pend *ApprovalPending
	for _, key := range []string{node, inst} {
		if _, err := flowGate(ctx, j, "r1", key, "pay", args, SingleApproval(), nil); !errors.As(err, &pend) {
			t.Fatalf("%s: first evaluation = %v, want a pause", key, err)
		}
	}
	if err := Approve(ctx, j, "r1", node, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := flowGate(ctx, j, "r1", node, "pay", args, SingleApproval(), nil); err != nil || !ok {
		t.Fatalf("the node's gate after its own approval = %v, %v", ok, err)
	}
	if _, err := flowGate(ctx, j, "r1", inst, "pay", args, SingleApproval(), nil); !errors.As(err, &pend) {
		t.Fatalf("the instance's gate after the node's approval = %v, want it still paused", err)
	}
	// The reverse, in a fresh run.
	j = memJournal()
	for _, key := range []string{node, inst} {
		_, _ = flowGate(ctx, j, "r2", key, "pay", args, SingleApproval(), nil)
	}
	if err := Approve(ctx, j, "r2", inst, true); err != nil {
		t.Fatal(err)
	}
	if _, err := flowGate(ctx, j, "r2", node, "pay", args, SingleApproval(), nil); !errors.As(err, &pend) {
		t.Fatalf("the node's gate after the instance's approval = %v, want it still paused", err)
	}

	// m-of-n: a decision signed for the node is not valid under the instance's subject.
	vf := fakeVerifiers("a")
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	j = memJournal()
	for _, key := range []string{node, inst} {
		if _, err := flowGate(ctx, j, "r3", key, "pay", args, pol, vf); !errors.As(err, &pend) {
			t.Fatalf("%s: m-of-n first evaluation = %v", key, err)
		}
	}
	nodeSubj := ApprovalSubject{RunID: "r3", ToolUseID: node, ToolName: "pay", Args: args}
	if err := decideAs(ctx, j, "r3", node, "a", true, fakeSign("a", ApprovalDecisionBytes(nodeSubj, "a", true, testDecidedAt))); err != nil {
		t.Fatal(err)
	}
	if ok, err := flowGate(ctx, j, "r3", node, "pay", args, pol, vf); err != nil || !ok {
		t.Fatalf("the node's m-of-n gate = %v, %v", ok, err)
	}
	if _, err := flowGate(ctx, j, "r3", inst, "pay", args, pol, vf); !errors.As(err, &pend) {
		t.Fatalf("the instance's m-of-n gate after the node's decision = %v, want it still paused", err)
	}
	// A decision signed for the node, submitted under the instance, does not verify there.
	if err := decideAs(ctx, j, "r3", inst, "a", true, fakeSign("a", ApprovalDecisionBytes(nodeSubj, "a", true, testDecidedAt))); err == nil {
		if _, err := flowGate(ctx, j, "r3", inst, "pay", args, pol, vf); !errors.As(err, &pend) {
			t.Fatalf("a decision signed for the node passed the instance: %v", err)
		}
	}
}

// FindToolCall finds the instance's call from the node's request, as it does for a tool call's instance:
// the name and arguments are the node's, the ID is the subject.
func TestFindToolCall_InstanceOfANode(t *testing.T) {
	b, _ := json.Marshal(ApprovalRequest{ToolUseID: "node:pay", ToolName: "pay", Args: json.RawMessage(`{"a":1}`)})
	recs := []Record{{Name: ApprovalRequestStep("node:pay"), Kind: StepValue, Result: b}}
	_, call, ok := FindToolCall(recs, "node:pay:taint")
	if !ok || call.ID != "node:pay:taint" || call.Name != "pay" || string(call.Args) != `{"a":1}` {
		t.Fatalf("FindToolCall(instance) = %+v, %v", call, ok)
	}
	if _, _, ok := FindToolCall(recs, "node:other:taint"); ok {
		t.Fatal("FindToolCall found an instance of a node with no request")
	}
}

// The instance's gate is decided once the node it guards has run: the node's result is the record that
// says the call went ahead, and an instance passed to get there.
func TestEvaluateGate_InstanceIsDecidedWhenItsNodeRan(t *testing.T) {
	recs := []Record{{Name: "node:pay", Kind: StepValue, Result: json.RawMessage(`1`)}}
	for _, subject := range []string{"node:pay", "node:pay:taint"} {
		st := EvaluateGateAfter(recs, ApprovalSubject{RunID: "r1", ToolUseID: subject, ToolName: "pay"}, SingleApproval(), nil, "")
		if st.Status != GateDecided || !st.Ran {
			t.Errorf("%s: standing = %+v, want decided and ran", subject, st)
		}
	}
	// The result of another node does not decide it.
	other := []Record{{Name: "node:other", Kind: StepValue, Result: json.RawMessage(`1`)}}
	st := EvaluateGateAfter(other, ApprovalSubject{RunID: "r1", ToolUseID: "node:pay:taint", ToolName: "pay"}, SingleApproval(), nil, "")
	if st.Ran {
		t.Errorf("another node's result decided the instance: %+v", st)
	}
}

// An escalation instance is a gate subject and nothing else: the Step hook runs only a node's key, and
// ResolveHalt resolves only the Step a node (or its body) halted as. Neither takes an instance.
func TestInstanceKeyIsNotANodeForTheStepHookOrResolveHalt(t *testing.T) {
	ctx := context.Background()
	const inst = "node:pay:taint"
	j := memJournal()
	if _, err := journalhookStepForTest(ctx, j, "r1", inst); !errors.Is(err, ErrConfig) {
		t.Errorf("the Step hook on an instance key = %v, want ErrConfig", err)
	}
	if _, ok, _ := j.Get(ctx, "r1", inst); ok {
		t.Error("the Step hook wrote a record under an instance key")
	}
	ref := HaltRef{RunID: "r1", Op: OpRef{Kind: OpStep, ID: inst}, Cause: HaltCrashed}
	if err := ResolveHalt(ctx, j, ref, Outcome{Result: "x"}); !errors.Is(err, ErrConfig) {
		t.Errorf("ResolveHalt on an instance key = %v, want ErrConfig", err)
	}
}

// journalhookStepForTest runs the flow Step hook on name with a body that returns a fixed value.
func journalhookStepForTest(ctx context.Context, j *Journal, runID, name string) (json.RawMessage, error) {
	return journalhook.Step(ctx, j, runID, name, Safety{}, func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`1`), nil
	})
}

// The standing of the gates that existed before the instance of a node did is what it was: a node key is
// decided by its own record (a node named with a '#' included), and a tool call's ID by a tool result, never
// by a value record that happens to carry its name.
func TestEvaluateGate_ExistingSubjectsAreUnchanged(t *testing.T) {
	value := func(name string) Record { return Record{Name: name, Kind: StepValue, Result: json.RawMessage(`1`)} }
	for _, c := range []struct {
		subject string
		recs    []Record
		ran     bool
	}{
		{"node:pay", []Record{value("node:pay")}, true},
		{"node:iter:2:pay", []Record{value("node:iter:2:pay")}, true},
		{"node:pay#taint", []Record{value("node:pay#taint")}, true}, // a node whose name holds '#'
		{"node:pay", []Record{value("node:iter:2:pay")}, false},
		{"node:pay", []Record{value("node:pay:step:fetch")}, false},
		{"c1", []Record{value("c1")}, false}, // not a plan key: a value record is not a tool's result
		{"c1#taint", []Record{value("c1")}, false},
		{"c1", []Record{{Kind: StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`1`)}}, true},
		{"c1#taint", []Record{{Kind: StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`1`)}}, false},
	} {
		st := EvaluateGateAfter(c.recs, ApprovalSubject{RunID: "r1", ToolUseID: c.subject, ToolName: "t"}, SingleApproval(), nil, "")
		if st.Ran != c.ran {
			t.Errorf("subject %q with %v: Ran = %v, want %v", c.subject, c.recs, st.Ran, c.ran)
		}
	}
}

// A node with no gate of its own, whose only gate is the escalation instance, has no request of its own: the
// instance's call is found from the instance's request. (FindToolCall once looked for the node's alone.)
func TestFindToolCall_InstanceOfAnUngatedNode(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	args := json.RawMessage(`{"amount":1}`)
	const inst = "node:pay:taint"
	var pend *ApprovalPending
	if _, err := flowGate(ctx, j, "r1", inst, "pay", args, SingleApproval(), nil); !errors.As(err, &pend) {
		t.Fatalf("the instance's first evaluation = %v, want a pause", err)
	}
	recs, err := j.History(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	idx, call, ok := FindToolCall(recs, inst)
	if !ok || call.ID != inst || call.Name != "pay" || string(call.Args) != `{"amount":1}` || recs[idx].Name != ApprovalRequestStep(inst) {
		t.Fatalf("FindToolCall(instance of an ungated node) = %d %+v, %v; want the instance's own request", idx, call, ok)
	}
	// With both requests, the instance's own is the one found; the node's alone is not mistaken for it.
	if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, SingleApproval(), nil); !errors.As(err, &pend) {
		t.Fatal(err)
	}
	recs, _ = j.History(ctx, "r1")
	if idx, _, ok := FindToolCall(recs, inst); !ok || recs[idx].Name != ApprovalRequestStep(inst) {
		t.Errorf("with both requests FindToolCall chose record %d (%v)", idx, ok)
	}
	// And a decision on the instance is takeable: the standing reads it from its own request.
	if err := Approve(ctx, j, "r1", inst, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := flowGate(ctx, j, "r1", inst, "pay", args, SingleApproval(), nil); err != nil || !ok {
		t.Fatalf("the instance's gate after its approval = %v, %v", ok, err)
	}
}
