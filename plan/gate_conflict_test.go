package plan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A config "approval" on a block that declares a different gate is one problem that names the
// node and says how to fix it, not two, whichever entry point reads the config; the hand-built
// Approval option says the same.
func TestGateConflictIsOneProblemWithAFix(t *testing.T) {
	gated := agent.MustFunc("charge", "charge a card", func(context.Context, int) (int, error) { return 0, nil },
		agent.WithApproval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}, ExcludePrincipal: true}))
	reg := NewRegistry()
	if err := reg.RegisterTool[int, int]("charge", gated); err != nil {
		t.Fatal(err)
	}
	cfg := `{"version": 1, "flow": "f", "in": "int", "out": "int", "entry": "charge",
  "nodes": [{"name": "charge", "block": "charge", "approval": {"need": 1, "approvers": ["ops"]}}],
  "wiring": []}`
	_, vf := approvers("ops", "finance", "risk")
	const want = `node "charge" sets an approval gate, but its block already declares one (2 of [ops finance risk] excluding the requester): keep one, by removing approval from the config or the gate from the block`

	_, loadErr := Load[int, int]([]byte(cfg), reg, WithLoadedApproverVerifiers(vf))
	valErr := Validate([]byte(cfg), reg)
	for name, err := range map[string]error{"Load": loadErr, "Validate": valErr} {
		if !errors.Is(err, agent.ErrConfig) || strings.Count(err.Error(), "gate") == 0 {
			t.Fatalf("%s = %v; want an ErrConfig", name, err)
		}
		if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "carries one gate") {
			t.Errorf("%s = %q; want one problem reading %q", name, err, want)
		}
	}

	rep := InspectConfig([]byte(cfg), reg)
	var gateProblems []string
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, "gate") {
			gateProblems = append(gateProblems, p.Message)
		}
	}
	if len(gateProblems) != 1 || gateProblems[0] != want {
		t.Errorf("InspectConfig gate problems = %q; want exactly [%q]", gateProblems, want)
	}

	b := New[int, int]("f")
	b.Tool[int, int]("charge", gated, Approval(agent.SingleApproval()))
	_, err := b.Build()
	if err == nil || !strings.Contains(err.Error(), "keep one, by removing") || strings.Contains(err.Error(), "carries one gate") {
		t.Errorf("Build = %v; want the same wording with the fix", err)
	}
}

// A problem whose text holds "; " (a node may be named "a; b") stays one problem, attached to its
// node: the problems travel as a list from assemble to InspectConfig and are never split from text.
func TestInspectConfigKeepsAProblemWithSemicolonOnePiece(t *testing.T) {
	cfg := `{"version": 1, "flow": "f", "in": "int", "out": "int", "entry": "a; b",
  "nodes": [{"name": "a; b", "block": "missing"}],
  "wiring": []}`
	rep := InspectConfig([]byte(cfg), NewRegistry())
	var got []ConfigProblem
	for _, p := range rep.Problems {
		if strings.Contains(p.Message, "missing") {
			got = append(got, p)
		}
	}
	if len(got) != 1 || got[0].Node != "a; b" || !strings.Contains(got[0].Message, `node "a; b" references unknown block "missing"`) {
		t.Fatalf("problems naming the unknown block = %+v; want one, on node %q", got, "a; b")
	}
}
