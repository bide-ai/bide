package flowedit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/editrun"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
)

// A loop: seed, refine (the head), check (the Switch: again goes back to refine, at most 10 times; else
// done).
const loopRunCfg = `{"version":1,"flow":"countdown","in":"int","out":"string","entry":"seed",
 "nodes":[{"name":"seed","block":"seed"},{"name":"refine","block":"refine"},{"name":"check","block":"check"},{"name":"done","block":"done"}],
 "wiring":[{"edge":["seed","refine"]},{"edge":["refine","check"]},{"switch":"check","when":[{"pred":"again","to":"refine","loop_max":10}],"else":"done"}]}`

func loopRegistry(t *testing.T) *plan.Registry {
	t.Helper()
	reg := plan.NewRegistry()
	inc := func(_ context.Context, n int) (int, error) { return n + 1, nil }
	for _, err := range []error{
		reg.RegisterStep("seed", inc), reg.RegisterStep("refine", inc), reg.RegisterStep("check", inc),
		reg.RegisterStep("done", func(_ context.Context, n int) (string, error) { return strconv.Itoa(n), nil }),
		reg.RegisterPredicate("again", func(n int) bool { return n < 6 }),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// ranLoop runs the loop config with input n under run "r1".
func ranLoop(t *testing.T, n int) *agent.Journal {
	t.Helper()
	flow, err := plan.Load[int, string]([]byte(loopRunCfg), loopRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Run(context.Background(), j, "r1", n); err != nil {
		t.Fatal(err)
	}
	return j
}

func loopServer(t *testing.T, file string, src runview.Source, typed bool) *Server {
	t.Helper()
	p := filepath.Join(t.TempDir(), "flow.json")
	if err := os.WriteFile(p, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := []Option{editrun.NewOption(editrun.Run{ID: "r1", Source: src}).(Option)}
	if typed {
		opts = append(opts, WithRegistry(loopRegistry(t)))
	}
	s, err := newServer(t, p, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func nodeNamed(p *runPath, name string) (pathNode, bool) {
	for _, n := range p.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return pathNode{}, false
}

func hasEdge(p *runPath, from, to string) bool {
	return slices.ContainsFunc(p.Edges, func(e pathEdge) bool { return e.From == from && e.To == to })
}

// The path of a plain run is its nodes and the edge between them, with words.
func TestRunPathOfASimpleRun(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	for _, typed := range []bool{true, false} {
		r := askRun(t, runServer(t, runCfg, runview.JournalSource{J: j}, "r1", typed), runCfg, nil, false)
		p := r.Path
		a, okA := nodeNamed(p, "a")
		b, okB := nodeNamed(p, "b")
		if p == nil || !okA || !okB || a.Words != "ran" || b.Status != "ran" || !hasEdge(p, "a", "b") || len(p.OffCanvas) != 0 || p.NotRead != "" {
			t.Errorf("typed=%v: %+v", typed, p)
		}
	}
}

// A loop's body ran several times, the arm back was taken and the arm out too; with input past the bound the
// loop does not go round and the arm back is not marked.
func TestRunPathOfALoop(t *testing.T) {
	for _, typed := range []bool{true, false} {
		r := askRun(t, loopServer(t, loopRunCfg, runview.JournalSource{J: ranLoop(t, 1)}, typed), loopRunCfg, nil, false)
		p := r.Path
		refine, ok := nodeNamed(p, "refine")
		if !ok || refine.Iters < 2 || !strings.HasPrefix(refine.Words, "ran ") || !strings.HasSuffix(refine.Words, " times") {
			t.Fatalf("typed=%v: refine %+v in %+v", typed, refine, p)
		}
		if !hasEdge(p, "check", "refine") || !hasEdge(p, "check", "done") || !hasEdge(p, "seed", "refine") || !hasEdge(p, "refine", "check") {
			t.Errorf("typed=%v: edges %+v", typed, p.Edges)
		}
		// Input 9: seed 10, refine 11, check 12: not again, so the loop is not gone round.
		r = askRun(t, loopServer(t, loopRunCfg, runview.JournalSource{J: ranLoop(t, 9)}, typed), loopRunCfg, nil, false)
		p = r.Path
		if n, _ := nodeNamed(p, "refine"); n.Words != "ran" || hasEdge(p, "check", "refine") || !hasEdge(p, "check", "done") {
			t.Errorf("typed=%v, no loop: refine %+v edges %+v", typed, n, p.Edges)
		}
	}
}

// A node that did not run is not marked, and neither is an edge not taken.
func TestRunPathLeavesWhatDidNotRunUnmarked(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	recs, err := j.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	// The run stopped after a: b has neither an attempt nor a result.
	cut := runview.Exports{"r1": slices.DeleteFunc(slices.Clone(recs), func(r agent.Record) bool {
		return strings.HasSuffix(r.Name, ":b") || r.Name == "run:complete"
	})}
	p := askRun(t, runServer(t, runCfg, cut, "r1", true), runCfg, nil, false).Path
	if _, ok := nodeNamed(p, "b"); ok || hasEdge(p, "a", "b") {
		t.Errorf("an unrun node or edge is marked: %+v", p)
	}
	if n, ok := nodeNamed(p, "a"); !ok || n.Words != "ran" {
		t.Errorf("a: %+v", p)
	}
}

// A body that started and left no result is "started"; a gate waiting is "paused".
func TestRunPathStartedAndPaused(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	recs, _ := j.History(context.Background(), "r1")
	started := runview.Exports{"r1": slices.DeleteFunc(slices.Clone(recs), func(r agent.Record) bool {
		return r.Name == "node:b" || r.Name == "run:complete"
	})}
	p := askRun(t, runServer(t, runCfg, started, "r1", true), runCfg, nil, false).Path
	if n, ok := nodeNamed(p, "b"); !ok || n.Status != "started" || n.Words != "started, no result recorded" {
		t.Errorf("started: %+v", p)
	}
	// The same run, the file gating b, and a pending approval for it: paused.
	gated := strings.Replace(runCfg, `{"name":"b","block":"dbl"}`, `{"name":"b","block":"dbl","approval":{"need":1,"approvers":["ops"]}}`, 1)
	topo := plan.InspectConfig([]byte(gated), runRegistry(t)).Topology
	tb, _ := json.Marshal(topo)
	dg, _ := plan.TopologyDigest(topo)
	db, _ := json.Marshal(dg)
	paused := runview.Exports{"r1": slices.Clone(started["r1"])}
	for i := range paused["r1"] {
		switch paused["r1"][i].Name {
		case plan.TopologyStep:
			paused["r1"][i].Result = tb
		case "flow:digest":
			paused["r1"][i].Result = db
		}
	}
	paused["r1"] = slices.DeleteFunc(paused["r1"], func(r agent.Record) bool { return r.Name == "attempt:step:node:b" })
	pend := agent.Record{Kind: agent.StepValue, Name: agent.ApprovalPendingStep("node:b"), Result: json.RawMessage(`{}`)}
	paused["r1"] = append(paused["r1"], pend)
	p = askRun(t, runServer(t, gated, paused, "r1", true), gated, nil, false).Path
	if n, ok := nodeNamed(p, "b"); !ok || n.Status != "paused" || n.Words != "paused at its approval gate" {
		t.Errorf("paused: %+v", p)
	}
}

// A node the run ran that the file lacks, or has with another block, and a connection the file lacks, are
// listed beside the canvas, never dropped and never marked on a node that is not the same.
func TestRunPathListsWhatTheFileCannotShow(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	src := runview.JournalSource{J: j}
	gone := `{"version":1,"flow":"pay","in":"int","out":"int","entry":"a","nodes":[{"name":"a","block":"inc"}],"wiring":[]}`
	p := askRun(t, runServer(t, runCfg, src, "r1", false), gone, nil, false).Path
	if _, ok := nodeNamed(p, "b"); ok || len(p.Edges) != 0 || len(p.OffCanvas) != 2 ||
		!strings.Contains(strings.Join(p.OffCanvas, "\n"), "b: the run ran; this file has no node of that name.") ||
		!strings.Contains(strings.Join(p.OffCanvas, "\n"), "edge a -> b: the run took it") {
		t.Errorf("removed node: %+v", p)
	}
	other := strings.Replace(runCfg, `"block":"dbl"`, `"block":"inc"`, 1)
	p = askRun(t, runServer(t, runCfg, src, "r1", false), other, nil, false).Path
	if _, ok := nodeNamed(p, "b"); ok || !strings.Contains(strings.Join(p.OffCanvas, "\n"), "b: the run ran with block dbl; this file's b has block inc, so it is another node.") {
		t.Errorf("another block: %+v", p)
	}
	if _, ok := nodeNamed(p, "a"); !ok {
		t.Errorf("the unchanged node lost its mark: %+v", p)
	}
}

// A rename of the session carries the marks to the new name, and a swap of two names carries each mark to the
// node that is now called by the other's name.
func TestRunPathFollowsRenames(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	src := runview.JournalSource{J: j}
	renamed := strings.NewReplacer(`"name":"b"`, `"name":"c"`, `["a","b"]`, `["a","c"]`).Replace(runCfg)
	p := askRun(t, runServer(t, runCfg, src, "r1", false), renamed, []Rename{{From: "b", To: "c"}}, false).Path
	if _, ok := nodeNamed(p, "c"); !ok || !hasEdge(p, "a", "c") || len(p.OffCanvas) != 0 {
		t.Errorf("renamed: %+v", p)
	}
	if _, ok := nodeNamed(p, "b"); ok {
		t.Errorf("the old name is still marked: %+v", p)
	}
	// Without the rename list the run's b is not in the file.
	p = askRun(t, runServer(t, runCfg, src, "r1", false), renamed, nil, false).Path
	if _, ok := nodeNamed(p, "c"); ok || len(p.OffCanvas) == 0 {
		t.Errorf("no rename list: %+v", p)
	}
}

// The loop bound the run had and the file has are both said when they differ.
func TestRunPathSaysALoopBoundChanged(t *testing.T) {
	j := ranLoop(t, 1)
	smaller := strings.Replace(loopRunCfg, `"loop_max":10`, `"loop_max":5`, 1)
	p := askRun(t, loopServer(t, smaller, runview.JournalSource{J: j}, false), smaller, nil, false).Path
	i := slices.IndexFunc(p.Edges, func(e pathEdge) bool { return e.From == "check" && e.To == "refine" })
	if i < 0 || !strings.Contains(p.Edges[i].Words, "its loop bound was 10 in the run, 5 in this file") {
		t.Errorf("%+v", p.Edges)
	}
	same := askRun(t, loopServer(t, loopRunCfg, runview.JournalSource{J: j}, false), loopRunCfg, nil, false).Path
	for _, e := range same.Edges {
		if strings.Contains(e.Words, "loop bound") {
			t.Errorf("an unchanged bound is said to differ: %+v", e)
		}
	}
}

// The path is read by a bounded number of point reads per node, however long the run: the same count with
// a hundred more records, and none for a run of too many nodes.
func TestRunPathReadsAreBounded(t *testing.T) {
	count := func(extra int) int32 {
		flow, err := plan.Load[int, int]([]byte(runCfg), runRegistry(t))
		if err != nil {
			t.Fatal(err)
		}
		cs := &countingStore{Store: agent.NewMemStore()}
		j, _ := agent.NewJournal(cs)
		if _, err := flow.Run(context.Background(), j, "r1", 1); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < extra; i++ {
			n := "extra:" + strconv.Itoa(i)
			if _, _, err := cs.Store.Insert(context.Background(), "r1", n, []byte(`{"name":"`+n+`","kind":"value","result":1}`)); err != nil {
				t.Fatal(err)
			}
		}
		cs.yielded.Store(0)
		cs.gets.Store(0)
		askRun(t, runServer(t, runCfg, runview.JournalSource{J: j}, "r1", true), runCfg, nil, false)
		if cs.yielded.Load() > 1 {
			t.Errorf("%d entries pulled", cs.yielded.Load())
		}
		return cs.gets.Load()
	}
	short, long := count(0), count(100)
	if short != long || short > 20 {
		t.Errorf("reads: %d for a short run, %d for a long one", short, long)
	}
	big := plan.Topology{Flow: "f", Entry: "n0"}
	for i := 0; i <= maxPathNodes; i++ {
		big.Nodes = append(big.Nodes, plan.TopologyNode{Name: fmt.Sprint("n", i), Block: "b"})
	}
	reads := 0
	if _, err := readPathFacts(func(string) (agent.Record, bool, error) { reads++; return agent.Record{}, false, nil }, big); err == nil || reads != 0 {
		t.Errorf("too many nodes: %v after %d reads", err, reads)
	}
}

// A Switch renamed in the session keeps its chosen arm: the marks of the arm out of it, and of the arm back, follow
// the new name.
func TestRunPathFollowsARenamedSwitch(t *testing.T) {
	renamed := strings.NewReplacer(`"name":"check"`, `"name":"chk"`, `["refine","check"]`, `["refine","chk"]`, `"switch":"check"`, `"switch":"chk"`).Replace(loopRunCfg)
	for _, typed := range []bool{true, false} {
		s := loopServer(t, loopRunCfg, runview.JournalSource{J: ranLoop(t, 1)}, typed)
		p := askRun(t, s, renamed, []Rename{{From: "check", To: "chk"}}, false).Path
		if !hasEdge(p, "chk", "refine") || !hasEdge(p, "chk", "done") || !hasEdge(p, "refine", "chk") || len(p.OffCanvas) != 0 {
			t.Errorf("typed=%v: %+v off canvas %q", typed, p.Edges, p.OffCanvas)
		}
	}
}

// A connection between a marked node and a node the file has with another block is not drawn: it is listed.
func TestRunPathDoesNotDrawAnEdgeToAnotherNode(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	other := strings.Replace(runCfg, `"block":"dbl"`, `"block":"inc"`, 1)
	p := askRun(t, runServer(t, runCfg, runview.JournalSource{J: j}, "r1", false), other, nil, false).Path
	if hasEdge(p, "a", "b") || !strings.Contains(strings.Join(p.OffCanvas, "\n"), "edge a -> b: the run took it") {
		t.Errorf("edges %+v off canvas %q", p.Edges, p.OffCanvas)
	}
}

// failFirstNode fails its first read of a node result, as a connection that drops in the middle of the path reads.
type failFirstNode struct {
	agent.Store
	armed, failed atomic.Bool
}

func (f *failFirstNode) Get(ctx context.Context, run, name string) (agent.Entry, bool, error) {
	if f.armed.Load() && strings.HasPrefix(name, "node:") && f.failed.CompareAndSwap(false, true) {
		return agent.Entry{}, false, leak
	}
	return f.Store.Get(ctx, run, name)
}

// A path that could not be read says so in words (no text of the error), and is not kept as if it were the
// answer: it is read again after the backoff, and Reload reads at once.
func TestRunPathReadFailureIsNotKept(t *testing.T) {
	old := runBackoff
	runBackoff = 0
	t.Cleanup(func() { runBackoff = old })
	flow, err := plan.Load[int, int]([]byte(runCfg), runRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	ff := &failFirstNode{Store: agent.NewMemStore()}
	j, _ := agent.NewJournal(ff)
	if _, err := flow.Run(context.Background(), j, "r1", 1); err != nil {
		t.Fatal(err)
	}
	ff.armed.Store(true)
	s := runServer(t, runCfg, runview.JournalSource{J: j}, "r1", true)
	r := askRun(t, s, runCfg, nil, false)
	if r.Path == nil || r.Path.NotRead != "The run's path was not read (store error)." || len(r.Path.Nodes) != 0 {
		t.Fatalf("the failed read: %+v", r.Path)
	}
	for _, w := range leakWords {
		if b, _ := json.Marshal(r); strings.Contains(string(b), w) {
			t.Errorf("the reply holds %q", w)
		}
	}
	if r := askRun(t, s, runCfg, nil, false); r.Path == nil || r.Path.NotRead != "" || len(r.Path.Nodes) != 2 {
		t.Errorf("the next request did not read the path again: %+v", r.Path)
	}
}

// A run halted in the middle of a loop is told: the last iteration that has a result, and that the next one started or
// waits at its gate. Without that the halt point, the one thing an author wants of a path, is lost.
func TestRunPathSaysWhereALoopStopped(t *testing.T) {
	j := ranLoop(t, 1)
	recs, err := j.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	has := func(name string) bool {
		return slices.ContainsFunc(recs, func(r agent.Record) bool { return r.Name == name })
	}
	for _, n := range []string{"node:iter:0:refine", "node:iter:1:refine", "attempt:step:node:iter:1:refine", "switch:iter:0:check"} {
		if !has(n) {
			t.Fatalf("setup: the run has no %s", n)
		}
	}
	cut := func(drop ...string) runview.Exports {
		return runview.Exports{"r1": slices.DeleteFunc(slices.Clone(recs), func(r agent.Record) bool {
			return r.Name == "run:complete" || slices.Contains(drop, r.Name) || strings.Contains(r.Name, "iter:2") || strings.HasSuffix(r.Name, ":done") || strings.HasPrefix(r.Name, "switch:iter:1")
		})}
	}
	pathOf := func(src runview.Source, file string) *runPath {
		return askRun(t, loopServer(t, file, src, true), file, nil, false).Path
	}
	// The second iteration of the head started and left no result.
	p := pathOf(cut("node:iter:1:refine", "node:iter:1:check", "switch:iter:1:check"), loopRunCfg)
	if n, ok := nodeNamed(p, "refine"); !ok || n.Status != "started" || n.Words != "ran, then started again, no result recorded" || n.Iters != 1 {
		t.Errorf("started again: %+v", n)
	}
	// The Switch chose to go round and the next iteration had not begun: the head ran once and nothing else is said.
	p = pathOf(cut("node:iter:1:refine", "attempt:step:node:iter:1:refine", "node:iter:1:check", "attempt:step:node:iter:1:check"), loopRunCfg)
	if n, _ := nodeNamed(p, "refine"); n.Status != "ran" || n.Words != "ran" {
		t.Errorf("the next iteration had not begun: %+v", n)
	}
	// The body is gated and the second iteration waits at its gate.
	gated := strings.Replace(loopRunCfg, `{"name":"refine","block":"refine"}`, `{"name":"refine","block":"refine","approval":{"need":1,"approvers":["ops"]}}`, 1)
	topo := plan.InspectConfig([]byte(gated), loopRegistry(t)).Topology
	tb, _ := json.Marshal(topo)
	dg, _ := plan.TopologyDigest(topo)
	db, _ := json.Marshal(dg)
	g := cut("node:iter:1:refine", "attempt:step:node:iter:1:refine", "node:iter:1:check", "attempt:step:node:iter:1:check")
	for i := range g["r1"] {
		switch g["r1"][i].Name {
		case plan.TopologyStep:
			g["r1"][i].Result = tb
		case "flow:digest":
			g["r1"][i].Result = db
		}
	}
	g["r1"] = append(g["r1"], agent.Record{Kind: agent.StepValue, Name: agent.ApprovalPendingStep("node:iter:1:refine"), Result: json.RawMessage(`{}`)})
	p = pathOf(g, gated)
	if n, ok := nodeNamed(p, "refine"); !ok || n.Status != "paused" || n.Words != "ran, then paused at its approval gate" {
		t.Errorf("paused again: %+v", n)
	}
}

// The reads of a loop node scale with its iterations, up to the clamp and no further, whatever the topology claims
// of its bound: a hostile topology that says a million, with every iteration present, costs the clamp.
func TestRunPathClampsALoopsReads(t *testing.T) {
	topo := plan.Topology{Flow: "f", Entry: "n0"}
	body := []string{}
	for i := 0; i < maxPathNodes; i++ {
		name := "n" + strconv.Itoa(i)
		topo.Nodes = append(topo.Nodes, plan.TopologyNode{Name: name, Block: "b"})
		body = append(body, name)
	}
	topo.Loops = []plan.TopologyLoop{{Head: "n0", Over: "n0", Body: body, Max: 1_000_000_000}}
	reads := 0
	get := func(name string) (agent.Record, bool, error) {
		reads++
		return agent.Record{}, strings.HasPrefix(name, "node:iter:"), nil // every iteration of every node is there
	}
	f, err := readPathFacts(get, topo)
	if err != nil {
		t.Fatal(err)
	}
	if per := reads / maxPathNodes; per > maxPathIters+3 || per < maxPathIters {
		t.Errorf("%d reads per node, want about the clamp %d (and never more than %d)", per, maxPathIters, maxPathIters+3)
	}
	if f.Nodes["n7"].Iters != maxPathIters {
		t.Errorf("iterations %d", f.Nodes["n7"].Iters)
	}
}

// An edge the run took between two nodes the file has, that the file lacks, is said to be gone; one between nodes the
// file cannot show is said to be in another place.
func TestRunPathSaysAnEdgeIsGone(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	gone := `{"version":1,"flow":"pay","in":"int","out":"int","entry":"a","nodes":[{"name":"a","block":"inc"},{"name":"b","block":"dbl"}],"wiring":[]}`
	p := askRun(t, runServer(t, runCfg, runview.JournalSource{J: j}, "r1", false), gone, nil, false).Path
	if len(p.Edges) != 0 || !slices.Contains(p.OffCanvas, "edge a -> b: the run took it; this file has no such connection.") {
		t.Errorf("%+v", p)
	}
}
