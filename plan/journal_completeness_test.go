package plan

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journalhook"
)

// chargeFlow is a one-node flow whose node counts its runs and fails the first one after its
// effect, which leaves the node's attempt marker without a result, as a crash there would.
func chargeFlow(t *testing.T, fired *int, opts ...NodeOption) *Flow[int, int] {
	t.Helper()
	b := New[int, int]("charge-flow")
	b.Step("charge", func(_ context.Context, n int) (int, error) {
		*fired++
		if *fired == 1 {
			return 0, errors.New("connection reset after the charge was sent")
		}
		return n, nil
	}, opts...)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return flow
}

// Safety is not part of the digest, so a flow whose node is relabelled retry-safe resumes a
// run begun under the old label. The node was attempted as a side effect, so its Step claimed an
// attempt marker, and the marker is the attempt's recorded safety: a resume under the retry-safe
// label halts on it rather than fire the effect a second time.
func TestResume_NodeRelabelledRetrySafeStillHalts(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	var fired int
	if _, err := chargeFlow(t, &fired).Run(ctx, store, "r", 5); err == nil {
		t.Fatal("first drive: want the node's error")
	}
	_, err := chargeFlow(t, &fired, Idempotent()).Run(ctx, store, "r", 5)
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op != (agent.OpRef{Kind: agent.OpStep, ID: "node:charge"}) {
		t.Fatalf("resume under a retry-safe label: err = %v, want *agent.OutcomeUnknown on node:charge", err)
	}
	if fired != 1 {
		t.Fatalf("the node's effect fired %d times, want 1", fired)
	}
}

// The other direction: a node attempted as retry-safe writes no marker (its author declared it
// safe to repeat when it ran), so a resume under a side-effect label runs it again, as agent.Step
// does, and claims a marker for this attempt. Unchanged labels re-run a retry-safe node.
func TestResume_NodeRelabelledSideEffectRunsUnderAClaim(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	var fired int
	if _, err := chargeFlow(t, &fired, Idempotent()).Run(ctx, store, "r", 5); err == nil {
		t.Fatal("first drive: want the node's error")
	}
	recs, err := store.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if hasRecord(recs, "attempt:step:node:charge") {
		t.Fatal("a retry-safe node wrote an attempt marker")
	}
	out, err := chargeFlow(t, &fired).Run(ctx, store, "r", 5)
	if err != nil || out != 5 || fired != 2 {
		t.Fatalf("resume under a side-effect label: out %d, err %v, fired %d; want 5, nil, 2", out, err, fired)
	}
	if recs, err = store.History(ctx, "r"); err != nil || !hasRecord(recs, "attempt:step:node:charge") {
		t.Fatalf("the side-effect attempt claimed no marker (err %v)", err)
	}
}

// A driver that finds another driver's claim on a node's attempt (the marker it would claim is
// already there, with no result) does not run the body, whatever its own flow labels the node:
// the winner attempted it as a side effect and may have fired it.
func TestClaimLost_DecidesByWinnersMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []NodeOption
	}{{"side effect", nil}, {"relabelled retry-safe", []NodeOption{Idempotent()}}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mem := agent.NewMemStore()
			var fired int
			flow := chargeFlow(t, &fired, tc.opts...)
			marker := journalhook.WithClaim(agent.Record{Kind: agent.StepAttempt, ToolUseID: "node:charge", AttemptedAt: 1}, "0ther0driver").(agent.Record)
			for _, w := range []struct {
				name string
				rec  agent.Record
			}{
				{flowDigestStep, agent.Record{Kind: agent.StepValue, Result: json.RawMessage(strconv.Quote(flow.Digest()))}},
				{"attempt:step:node:charge", marker},
			} {
				if _, err := mem.Do(ctx, "r", w.name, func(context.Context) (agent.Record, error) { return w.rec, nil }); err != nil {
					t.Fatal(err)
				}
			}
			_, err := flow.Run(ctx, mem, "r", 5)
			var halt *agent.OutcomeUnknown
			if !errors.As(err, &halt) || fired != 0 || halt.Op.ID != "node:charge" || halt.Cause != agent.HaltCrashed {
				t.Fatalf("claim lost to another driver's attempt: err = %v, fired %d; want *agent.OutcomeUnknown (crashed) on node:charge and 0", err, fired)
			}
		})
	}
}
