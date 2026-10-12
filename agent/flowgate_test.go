package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// A flow gate's request is recorded once. A later evaluation for another node name or another
// input (a resume under another flow or input, which plan refuses earlier) is ErrConfig, so no
// decision is ever counted against an input other than the one it was signed for.
func TestFlowGate_RequestIsFixed(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	pol := SingleApproval()
	_, err := flowGate(ctx, j, "r1", "node:pay", "pay", json.RawMessage(`{"amount":1}`), pol, nil)
	var pend *ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("first evaluation = %v; want a pause", err)
	}
	for name, args := range map[string]string{"pay": `{"amount":2}`, "refund": `{"amount":1}`} {
		if _, err := flowGate(ctx, j, "r1", "node:pay", name, json.RawMessage(args), pol, nil); !errors.Is(err, ErrConfig) {
			t.Errorf("evaluation as %s(%s) = %v; want ErrConfig", name, args, err)
		}
	}
	// The same input in another spelling is the same request.
	if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", json.RawMessage(`{ "amount" : 1.0e0 }`), pol, nil); !errors.As(err, &pend) {
		t.Errorf("evaluation with the input respelled = %v; want the pause", err)
	}
}

// The gate runs only on a plan node key, with a policy, and an m-of-n gate only with verifiers.
func TestFlowGate_Refusals(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	mofn := &ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}
	for name, call := range map[string]func() error{
		"not a node key": func() error { _, err := flowGate(ctx, j, "r1", "tool:x", "x", nil, SingleApproval(), nil); return err },
		"no policy":      func() error { _, err := flowGate(ctx, j, "r1", "node:x", "x", nil, nil, nil); return err },
		"no verifiers":   func() error { _, err := flowGate(ctx, j, "r1", "node:x", "x", nil, mofn, nil); return err },
	} {
		if err := call(); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: flowGate = %v; want ErrConfig", name, err)
		}
	}
}

// FindToolCall finds a flow gate's request only on a plan node key, so a request record cannot
// stand in for an agent's tool call.
func TestFindToolCall_FlowRequestOnlyOnNodeKeys(t *testing.T) {
	req := func(id string) Record {
		b, _ := json.Marshal(ApprovalRequest{ToolUseID: id, ToolName: "pay", Args: json.RawMessage(`1`)})
		return Record{Name: ApprovalRequestStep(id), Kind: StepValue, Result: b}
	}
	if _, call, ok := FindToolCall([]Record{req("node:pay")}, "node:pay"); !ok || call.Name != "pay" || string(call.Args) != "1" {
		t.Fatalf("FindToolCall(node:pay) = %+v, %v; want the request", call, ok)
	}
	if _, _, ok := FindToolCall([]Record{req("c1")}, "c1"); ok {
		t.Fatal("FindToolCall found a request record for a tool-use id")
	}
	other := req("node:pay")
	other.Name = ApprovalRequestStep("node:other")
	if _, _, ok := FindToolCall([]Record{other}, "node:pay"); ok {
		t.Fatal("FindToolCall found a request recorded under another node's name")
	}
}
