package plan

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// buildTriage builds a small switched flow shaped like the example, so the digest
// tests exercise nodes, an edge, and a Switch with When/Else arms. It fails the test
// on a build error rather than returning one.
func buildTriage(t *testing.T, name string) *Flow[int, string] {
	t.Helper()
	b := New[int, string](name)
	entry := b.Step("entry", func(n int) (int, error) { return n + 1, nil })
	high := b.Step("high", func(int) (string, error) { return "high", nil })
	low := b.Step("low", func(int) (string, error) { return "low", nil })
	b.Switch(entry,
		When(func(n int) bool { return n > 0 }, high),
		Else(low),
	)
	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build %q: %v", name, err)
	}
	return flow
}

// TestDigestDeterministic asserts two independent builds of the SAME topology produce
// byte-identical digests: the digest is a pure, map-free function of the insertion-
// ordered spec, so it is stable across builds and processes.
func TestDigestDeterministic(t *testing.T) {
	a := buildTriage(t, "triage")
	b := buildTriage(t, "triage")
	if a.Digest() != b.Digest() {
		t.Errorf("Digest is not deterministic across builds of the same topology:\n a=%s\n b=%s", a.Digest(), b.Digest())
	}
	// Repeated calls on one flow are also stable.
	if a.Digest() != a.Digest() {
		t.Error("Digest returned different values on repeated calls for one flow")
	}
	// A digest is a 64-hex-char SHA-256 sum.
	if got := a.Digest(); len(got) != 64 {
		t.Errorf("Digest length = %d, want 64 hex chars: %q", len(got), got)
	}
}

// TestDigestChangesOnTopologyChange asserts that a change to ANY facet of the declared
// topology (flow name, a node name, a node's I/O type, an edge, or a Switch arm order)
// changes the digest. Each variant differs from the triage baseline in exactly one way.
func TestDigestChangesOnTopologyChange(t *testing.T) {
	base := buildTriage(t, "triage").Digest()

	variants := map[string]func() *Flow[int, string]{
		// A different flow name.
		"renamed flow": func() *Flow[int, string] { return buildTriage(t, "triage-2") },
		// A renamed node.
		"renamed node": func() *Flow[int, string] {
			b := New[int, string]("triage")
			entry := b.Step("entry", func(n int) (int, error) { return n + 1, nil })
			high := b.Step("HIGH", func(int) (string, error) { return "high", nil }) // renamed
			low := b.Step("low", func(int) (string, error) { return "low", nil })
			b.Switch(entry, When(func(n int) bool { return n > 0 }, high), Else(low))
			f, err := b.Build()
			if err != nil {
				t.Fatalf("build renamed node: %v", err)
			}
			return f
		},
		// Reordered Switch arms (When and Else swapped in declared order).
		"reordered arms": func() *Flow[int, string] {
			b := New[int, string]("triage")
			entry := b.Step("entry", func(n int) (int, error) { return n + 1, nil })
			high := b.Step("high", func(int) (string, error) { return "high", nil })
			low := b.Step("low", func(int) (string, error) { return "low", nil })
			b.Switch(entry, Else(low), When(func(n int) bool { return n > 0 }, high)) // swapped
			f, err := b.Build()
			if err != nil {
				t.Fatalf("build reordered arms: %v", err)
			}
			return f
		},
	}

	for label, build := range variants {
		if got := build().Digest(); got == base {
			t.Errorf("%s: digest did not change from baseline (both %s)", label, got)
		}
	}
}

// TestDigestChangesOnBoundaryType asserts the pinned In/Out flow boundary types are
// part of the digest: a flow with the same node names but a different Out type has a
// different digest.
func TestDigestChangesOnBoundaryType(t *testing.T) {
	base := buildTriage(t, "triage").Digest()

	b := New[int, int]("triage") // Out is int, not string
	entry := b.Step("entry", func(n int) (int, error) { return n + 1, nil })
	high := b.Step("high", func(int) (int, error) { return 1, nil })
	low := b.Step("low", func(int) (int, error) { return 0, nil })
	b.Switch(entry, When(func(n int) bool { return n > 0 }, high), Else(low))
	other, err := b.Build()
	if err != nil {
		t.Fatalf("build int-out flow: %v", err)
	}
	if other.Digest() == base {
		t.Errorf("digest ignored the flow boundary type change (both %s)", base)
	}
}

// TestRunJournalsDigest asserts Run records the topology digest FIRST, under the
// reserved flow:digest name, and that the journaled digest equals the flow's Digest().
// This is the record the audit layer commits to.
func TestRunJournalsDigest(t *testing.T) {
	flow := buildTriage(t, "triage")
	mem := agent.NewMemStore()
	ctx := context.Background()
	if _, err := flow.Run(ctx, mem, "digest-run", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}
	recs, err := mem.History(ctx, "digest-run")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(recs) == 0 || recs[0].Name != flowDigestStep {
		t.Fatalf("first journal record is not %q: %+v", flowDigestStep, recs)
	}
	var got string
	if err := json.Unmarshal(recs[0].Result, &got); err != nil {
		t.Fatalf("decode journaled digest: %v", err)
	}
	if got != flow.Digest() {
		t.Errorf("journaled digest %q != flow.Digest() %q", got, flow.Digest())
	}
}

// TestConformDigestMatch asserts a run's journaled digest is accepted by Conform when
// it equals the flow's own Digest(): the reserved flow:digest record is not a
// divergence and the topologies match.
func TestConformDigestMatch(t *testing.T) {
	flow := buildTriage(t, "triage")
	mem := agent.NewMemStore()
	ctx := context.Background()
	if _, err := flow.Run(ctx, mem, "match", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ok, diffs, err := flow.Conform(ctx, mem, "match")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if !ok {
		t.Errorf("Conform flagged a matching digest as divergent: %v", diffs)
	}
}

// TestConformDigestMismatch asserts Conform reports a divergence when the journaled
// topology digest does NOT equal the flow's Digest(): the run followed a different
// declared topology than the flow now describes. It runs one flow to journal its
// digest, then conforms a DIFFERENT flow (a different topology) against that journal.
func TestConformDigestMismatch(t *testing.T) {
	ran := buildTriage(t, "triage")
	mem := agent.NewMemStore()
	ctx := context.Background()
	if _, err := ran.Run(ctx, mem, "mismatch", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// A flow with a different topology (renamed flow) conforming the same journal must
	// see the journaled digest differ from its own.
	other := buildTriage(t, "triage-different")
	ok, diffs, err := other.Conform(ctx, mem, "mismatch")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if ok {
		t.Fatal("Conform reported ok when the journaled topology digest differs from the flow's Digest()")
	}
	found := false
	for _, d := range diffs {
		if contains(d, flowDigestStep) && contains(d, "different topology") {
			found = true
		}
	}
	if !found {
		t.Errorf("Conform diffs %v do not flag the topology-digest mismatch", diffs)
	}
}
