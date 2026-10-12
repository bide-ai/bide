package plan

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// TestTopologyDigest_IsDigest: the digest computed from a flow's Topology is its Digest, for a
// switched flow, a typed join and a loop, and survives a JSON round trip of the Topology.
func TestTopologyDigest_IsDigest(t *testing.T) {
	for name, f := range map[string]interface {
		Topology() Topology
		Digest() string
	}{"triage": buildTriage(t, "triage"), "fixture": topologyFixtureFlow()} {
		b, err := json.Marshal(f.Topology())
		if err != nil {
			t.Fatal(err)
		}
		var back Topology
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		got, err := TopologyDigest(back)
		if err != nil || got != f.Digest() {
			t.Errorf("%s: TopologyDigest of the round-tripped Topology = %s, %v; want Digest %s", name, got, err, f.Digest())
		}
	}
}

// TestRunJournalsTopologyOnce: a run's first drive journals the Topology once, its digest
// recomputes to the journaled flow:digest, and a resume writes it no more.
func TestRunJournalsTopologyOnce(t *testing.T) {
	ctx := context.Background()
	flow := buildTriage(t, "triage")
	cs := agenttest.NewCountingStore(agent.NewMemStore())
	j := agenttest.MustJournal(cs)
	if _, err := flow.Run(ctx, j, "r", 1); err != nil {
		t.Fatal(err)
	}
	inserts := 0
	for _, op := range cs.Counts().Names {
		if op == "insert "+TopologyStep {
			inserts++
		}
		if op == "get "+TopologyStep {
			t.Errorf("the first drive read %s; it records it with one Insert", TopologyStep)
		}
	}
	if inserts != 1 {
		t.Fatalf("the first drive inserted %s %d times, want 1", TopologyStep, inserts)
	}
	recs, err := j.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	top, digest, ok, err := JournaledTopology(recs)
	if !ok || err != nil || digest != flow.Digest() || !reflect.DeepEqual(top, flow.Topology()) {
		t.Fatalf("JournaledTopology = ok %v, err %v, digest %s (flow %s), topology equal %v", ok, err, digest, flow.Digest(), reflect.DeepEqual(top, flow.Topology()))
	}
	if d, err := TopologyDigest(top); err != nil || d != digest {
		t.Fatalf("the journaled topology recomputes to %s, %v; the journaled digest is %s", d, err, digest)
	}

	// A resume of an unfinished run: drive a run whose node crashed, then resume it.
	cs2 := agenttest.NewCountingStore(agent.NewMemStore())
	j2 := agenttest.MustJournal(cs2)
	crash := true
	b := New[int, string]("halts")
	b.Step("only", func(context.Context, int) (string, error) {
		if crash {
			return "", errors.New("transient")
		}
		return "done", nil
	}, ReadOnly())
	hf, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.Run(ctx, j2, "h", 1); err == nil {
		t.Fatal("the failing drive succeeded")
	}
	cs2.Reset()
	crash = false
	if out, err := hf.Run(ctx, j2, "h", 1); err != nil || out != "done" {
		t.Fatalf("resume = %q, %v", out, err)
	}
	for _, op := range cs2.Counts().Names {
		if op == "insert "+TopologyStep || op == "get "+TopologyStep {
			t.Fatalf("a resume touched %s: %v", TopologyStep, cs2.Counts().Names)
		}
	}
}

// TestRunStartedWithoutTopology: a run journaled before flow:topology existed (run:start and
// flow:digest only) resumes and completes without being given one, and conforms.
func TestRunStartedWithoutTopology(t *testing.T) {
	ctx := context.Background()
	flow := buildTriage(t, "triage")
	j := agenttest.MemJournal()
	for _, w := range []struct {
		name string
		rec  agent.Record
	}{
		{runStartStep, agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`{"input":{"role":"user","parts":[{"type":"text","text":"1"}]},"kind":"flow","flow":{"name":"triage"}}`)}},
		{flowDigestStep, agent.Record{Kind: agent.StepValue, Result: json.RawMessage(strconv.Quote(flow.Digest()))}},
	} {
		if _, err := journaltest.Put(ctx, j, "old", w.name, w.rec); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := flow.Run(ctx, j, "old", 1); err != nil || out != "high" {
		t.Fatalf("resume of an old run = %q, %v", out, err)
	}
	recs, err := j.History(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := JournaledTopology(recs); ok || err != nil {
		t.Fatalf("an old run was given a topology record (ok %v, err %v)", ok, err)
	}
	if ok, diffs, err := flow.Conform(ctx, j, "old"); !ok || err != nil {
		t.Fatalf("Conform of an old run = %v, %v, %v", ok, diffs, err)
	}
}

// TestJournaledTopologyMismatch: a topology record that is not the journaled digest's is an error
// for a reader and a divergence for Conform.
func TestJournaledTopologyMismatch(t *testing.T) {
	ctx := context.Background()
	flow := buildTriage(t, "triage")
	other := buildTriage(t, "other")
	j := agenttest.MemJournal()
	topo, _ := json.Marshal(other.Topology())
	if _, err := journaltest.Put(ctx, j, "r", TopologyStep, agent.Record{Kind: agent.StepValue, Result: topo}); err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Run(ctx, j, "r", 1); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(ctx, "r")
	if _, _, ok, err := JournaledTopology(recs); !ok || err == nil {
		t.Fatalf("JournaledTopology of another flow's topology = ok %v, err %v; want an error", ok, err)
	}
	ok, diffs, err := flow.Conform(ctx, j, "r")
	if ok || err != nil || len(diffs) != 1 || diffs[0] != TopologyStep+" (not this flow's topology)" {
		t.Fatalf("Conform = %v, %v, %v; want the one divergence of %s", ok, diffs, err, TopologyStep)
	}
}
