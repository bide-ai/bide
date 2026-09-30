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
// run begun under the old label. The node was attempted as a side effect; the resume re-ran it
// under the new label and fired the effect a second time. The attempt marker now records the
// safety the node was attempted under, and a resume decides by it.
func TestResume_NodeRelabelledRetrySafeStillHalts(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	var fired int
	if _, err := chargeFlow(t, &fired).Run(ctx, store, "r", 5); err == nil {
		t.Fatal("first drive: want the node's error")
	}
	_, err := chargeFlow(t, &fired, Idempotent()).Run(ctx, store, "r", 5)
	var halt *HaltAmbiguous
	if !errors.As(err, &halt) || halt.Step != "charge" {
		t.Fatalf("resume under a retry-safe label: err = %v, want *HaltAmbiguous on charge", err)
	}
	if fired != 1 {
		t.Fatalf("the node's effect fired %d times, want 1", fired)
	}
}

// The other direction: a node attempted as retry-safe and since labelled a side effect halts
// too. The journal cannot show whether the attempt took effect, and the node's author now says
// running it again is not safe, so a resume re-runs a node only if it was retry-safe when it was
// attempted and is retry-safe now.
func TestResume_NodeRelabelledSideEffectHalts(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	var fired int
	if _, err := chargeFlow(t, &fired, Idempotent()).Run(ctx, store, "r", 5); err == nil {
		t.Fatal("first drive: want the node's error")
	}
	_, err := chargeFlow(t, &fired).Run(ctx, store, "r", 5)
	var halt *HaltAmbiguous
	if !errors.As(err, &halt) || fired != 1 {
		t.Fatalf("resume under a side-effect label: err = %v, fired %d; want *HaltAmbiguous and 1", err, fired)
	}
	// Unchanged labels keep today's behavior: a retry-safe node re-runs.
	out, err := chargeFlow(t, &fired, Idempotent()).Run(ctx, store, "r", 5)
	if err != nil || out != 5 || fired != 2 {
		t.Fatalf("resume under the recorded label: out %d, err %v, fired %d; want 5, nil, 2", out, err, fired)
	}
}

// A marker written before markers recorded safety says nothing about the attempt, so a resume
// halts on it whatever the node's label is now.
func TestResume_LegacyMarkerHalts(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	var fired int
	flow := chargeFlow(t, &fired, Idempotent())
	for _, w := range []struct {
		name string
		rec  agent.Record
	}{
		{flowDigestStep, agent.Record{Kind: agent.StepValue, Result: json.RawMessage(strconv.Quote(flow.Digest()))}},
		{attemptMarker("charge"), agent.Record{Kind: agent.StepValue}},
	} {
		if _, err := store.Do(ctx, "r", w.name, func(context.Context) (agent.Record, error) { return w.rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	_, err := flow.Run(ctx, store, "r", 5)
	var halt *HaltAmbiguous
	if !errors.As(err, &halt) || fired != 0 {
		t.Fatalf("resume over a legacy marker: err = %v, fired %d; want *HaltAmbiguous and 0", err, fired)
	}
}

// hideStore hides one step from History, standing in for a second driver that writes the step
// after this driver read the journal and before it claims the step itself.
type hideStore struct {
	*agent.MemStore
	name string
}

func (s hideStore) History(ctx context.Context, runID string) ([]agent.Record, error) {
	recs, err := s.MemStore.History(ctx, runID)
	out := recs[:0:0]
	for _, r := range recs {
		if r.Name != s.name {
			out = append(out, r)
		}
	}
	return out, err
}

// A driver that loses the claim on a node's attempt to another driver decides by the winner's
// marker: the winner attempted the node as a side effect, so the loser halts even if its own flow
// labels the node retry-safe.
func TestClaimLost_DecidesByWinnersMarker(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	var fired int
	flow := chargeFlow(t, &fired, Idempotent())
	for _, w := range []struct {
		name string
		rec  agent.Record
	}{
		{flowDigestStep, agent.Record{Kind: agent.StepValue, Result: json.RawMessage(strconv.Quote(flow.Digest()))}},
		{attemptMarker("charge"), journalhook.WithClaim(agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`{"retry_safe":false}`)}, "other-driver").(agent.Record)},
	} {
		if _, err := mem.Do(ctx, "r", w.name, func(context.Context) (agent.Record, error) { return w.rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	_, err := flow.Run(ctx, hideStore{MemStore: mem, name: attemptMarker("charge")}, "r", 5)
	var halt *HaltAmbiguous
	if !errors.As(err, &halt) || fired != 0 {
		t.Fatalf("claim lost to a side-effect attempt: err = %v, fired %d; want *HaltAmbiguous and 0", err, fired)
	}
}
