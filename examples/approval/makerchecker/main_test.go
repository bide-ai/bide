package main

import (
	"context"
	"slices"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

// The maker's approval never counts: the refund runs once, on finance's and risk's approvals, and
// the offline check names ops as the requester and its decision as ignored.
func TestMakerChecker(t *testing.T) {
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	refunds, v, err := story(context.Background(), j, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if refunds != 1 || !v.OK || !slices.Equal(v.Counted, []string{"finance", "risk"}) || !slices.Equal(v.Requesters, []string{"ops"}) {
		t.Fatalf("refunds %d, verdict %+v; want one refund on finance and risk, ops the requester", refunds, v)
	}
	if len(v.Ignored) != 1 || v.Ignored[0].Approver != "ops" || v.Ignored[0].Reason != agent.ReasonRequester {
		t.Fatalf("ignored = %+v; want ops ignored as the requester", v.Ignored)
	}
}

// The config's assertion holds on the declared graph.
func TestMakerCheckerAssertion(t *testing.T) {
	r := plan.InspectConfig(config, nil)
	if len(r.Assertions) != 1 || !r.Assertions[0].Holds {
		t.Fatalf("assertions = %+v", r.Assertions)
	}
}
