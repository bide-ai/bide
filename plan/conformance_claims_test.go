package plan

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journalhook"
)

// A claim's bookkeeping (a not-started record, written when a node's claim failed before its body
// ran, and a claim-held record, written when the claim was then taken back) says nothing about the
// flow's topology: Conform does not report it as a step, or as an attempt for an undeclared one.
func TestConformIgnoresClaimBookkeeping(t *testing.T) {
	b := New[int, int]("conform-claims")
	b.Step("entry", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	mem := agent.NewMemStore()
	ctx := context.Background()
	if _, err := flow.Run(ctx, mem, "r", 1); err != nil {
		t.Fatal(err)
	}
	for name, kind := range map[string]agent.StepKind{
		"attempt:not-started:0123abcd:attempt:entry": agent.StepNotStarted,
		"attempt:not-started:4567ef01:attempt:entry": agent.StepClaimHeld,
	} {
		rec := journalhook.WithClaim(agent.Record{Kind: kind}, name[len("attempt:not-started:"):len("attempt:not-started:")+8]).(agent.Record)
		if _, err := mem.Do(ctx, "r", name, func(context.Context) (agent.Record, error) { return rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	ok, diffs, err := flow.Conform(ctx, mem, "r")
	if err != nil || !ok || len(diffs) != 0 {
		t.Fatalf("Conform = %v, %v, %v; want the run to conform", ok, diffs, err)
	}
}
