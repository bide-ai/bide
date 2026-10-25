package flowedit

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

// maxPathNodes bounds the nodes whose journal keys are read for the run's path: each costs a few point
// reads. Past it the path is not read, and the panel says so.
const maxPathNodes = 300

// maxPathIters bounds the loop iterations counted for one node.
const maxPathIters = 100

// getFunc reads one record of the run by its journal key.
type getFunc func(name string) (agent.Record, bool, error)

// pathFact is what the run's journal says of one node, by the node's name in the run's topology.
type pathFact struct {
	// Iters is how many times the node's result is recorded (a loop body can have several), 1 for a node
	// outside a loop that ran.
	Iters int
	// Attempted is set when the node's attempt marker is recorded and no result is: the body started and
	// its end is not recorded.
	Attempted bool
	// Paused is set for a gated node whose approval is pending and which has no result.
	Paused bool
}

// pathFacts are the facts of every node that left a record, and the target each Switch last chose.
type pathFacts struct {
	Nodes  map[string]pathFact
	Chosen map[string]string
}

// readPathFacts reads the run's path by point reads of the keys plan.Run writes: "node:<N>" (or
// "node:iter:<i>:<N>" in a loop body) for a result, "attempt:step:<node key>" for a started body,
// "approval-pending:<node key>" for a gate waiting, and "switch:<over>" (or "switch:iter:<i>:<over>") for a
// choice. The cost is a few reads per node, bounded by maxPathNodes and maxPathIters. A node name is
// checked by plan to hold no ':', so a key is that node's and no other's.
func readPathFacts(get getFunc, t plan.Topology) (*pathFacts, error) {
	if len(t.Nodes) > maxPathNodes {
		return nil, fmt.Errorf("the topology has more than %d nodes", maxPathNodes)
	}
	loopMax := map[string]int{}
	for _, l := range t.Loops {
		for _, n := range l.Body {
			loopMax[n] = max(loopMax[n], min(l.Max, maxPathIters))
		}
		loopMax[l.Over] = max(loopMax[l.Over], min(l.Max, maxPathIters))
	}
	f := &pathFacts{Nodes: map[string]pathFact{}, Chosen: map[string]string{}}
	has := func(key string) (bool, error) {
		_, ok, err := get(key)
		return ok, err
	}
	for _, n := range t.Nodes {
		var fact pathFact
		if limit := loopMax[n.Name]; limit > 0 {
			for i := 0; i < limit; i++ {
				ok, err := has("node:iter:" + strconv.Itoa(i) + ":" + n.Name)
				if err != nil {
					return nil, err
				}
				if !ok {
					break
				}
				fact.Iters++
			}
		} else {
			ok, err := has("node:" + n.Name)
			if err != nil {
				return nil, err
			}
			if ok {
				fact.Iters = 1
			}
		}
		if fact.Iters == 0 {
			key := "node:" + n.Name
			if loopMax[n.Name] > 0 {
				key = "node:iter:0:" + n.Name
			}
			var err error
			if fact.Attempted, err = has("attempt:step:" + key); err != nil {
				return nil, err
			}
			if n.Gate != nil {
				if fact.Paused, err = has(agent.ApprovalPendingStep(key)); err != nil {
					return nil, err
				}
			}
		}
		if fact.Iters > 0 || fact.Attempted || fact.Paused {
			f.Nodes[n.Name] = fact
		}
	}
	for _, b := range t.Branches {
		key := "switch:" + b.Over
		last := ""
		read := func(key string) (bool, error) {
			rec, ok, err := get(key)
			if err != nil || !ok {
				return false, err
			}
			var target string
			if json.Unmarshal(rec.Result, &target) == nil {
				last = target
			}
			return true, nil
		}
		if limit := loopMax[b.Over]; limit > 0 {
			for i := 0; i < limit; i++ {
				ok, err := read("switch:iter:" + strconv.Itoa(i) + ":" + b.Over)
				if err != nil {
					return nil, err
				}
				if !ok {
					break
				}
			}
		} else if _, err := read(key); err != nil {
			return nil, err
		}
		if last != "" {
			f.Chosen[b.Over] = last
		}
	}
	return f, nil
}

// getOf is how the facts of an exported journal, already in memory, are read.
func getOf(recs []agent.Record) getFunc {
	by := make(map[string]agent.Record, len(recs))
	for _, r := range recs {
		by[r.Name] = r
	}
	return func(name string) (agent.Record, bool, error) {
		r, ok := by[name]
		return r, ok, nil
	}
}

// runPath is the run's path projected on the file's graph: what the page marks on the canvas, and what it
// lists beside it because the file has no place for it.
type runPath struct {
	// NotRead says why the path was not read, when it was not.
	NotRead string `json:"not_read,omitempty"`
	// Nodes are the file's nodes the run ran, started or paused at, and Edges the file's connections the run
	// took; each carries the words the page writes on the canvas.
	Nodes []pathNode `json:"nodes"`
	Edges []pathEdge `json:"edges"`
	// OffCanvas lists, one sentence each, what the run did that this file cannot show: a node it ran that
	// the file lacks, or has with another block, and a connection it took that the file lacks.
	OffCanvas []string `json:"off_canvas"`
}

type pathNode struct {
	Name  string `json:"name"`
	Words string `json:"words"`
	// Status is "ran", "started" or "paused".
	Status string `json:"status"`
	Iters  int    `json:"iters,omitempty"`
}

type pathEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Words string `json:"words"`
}

// currentNames maps each node name of the run's topology to its name after the session's renames, replayed in
// order as renameTopology replays them.
func currentNames(t plan.Topology, made []Rename) map[string]string {
	cur := make(map[string]string, len(t.Nodes))
	for _, n := range t.Nodes {
		cur[n.Name] = n.Name
	}
	for _, r := range made {
		for k, v := range cur {
			if v == r.From {
				cur[k] = r.To
			}
		}
	}
	return cur
}

func nodeWords(f pathFact) (status, words string) {
	switch {
	case f.Iters > 1:
		return "ran", fmt.Sprintf("ran %d times", f.Iters)
	case f.Iters == 1:
		return "ran", "ran"
	case f.Paused:
		return "paused", "paused at its approval gate"
	}
	return "started", "started, no result recorded"
}

// projectPath puts the run's path on the file's topology. run is the run's topology after the session's
// renames (made), facts are the journal's by the names it had; file is the file's. A node is marked only
// when the file has a node of that name with the same block (and, when typed, the same kind): a name with
// another block is another node, listed beside the canvas. A connection the run took is marked when the file
// has it between marked nodes, and listed otherwise.
func projectPath(facts *pathFacts, orig plan.Topology, run plan.Topology, made []Rename, file plan.Topology, typed bool) *runPath {
	p := &runPath{Nodes: []pathNode{}, Edges: []pathEdge{}, OffCanvas: []string{}}
	cur := currentNames(orig, made)
	fact := map[string]pathFact{}
	for o, f := range facts.Nodes {
		fact[cur[o]] = f
	}
	chosen := map[string]string{}
	for over, target := range facts.Chosen {
		chosen[cur[over]] = target
		if c, ok := cur[target]; ok {
			chosen[cur[over]] = c
		}
	}
	fileNode := map[string]plan.TopologyNode{}
	for _, n := range file.Nodes {
		fileNode[n.Name] = n
	}
	onCanvas := map[string]bool{}
	for _, n := range run.Nodes {
		f, ok := fact[n.Name]
		if !ok {
			continue
		}
		st, words := nodeWords(f)
		fn, there := fileNode[n.Name]
		switch {
		case !there:
			p.OffCanvas = append(p.OffCanvas, fmt.Sprintf("%s: the run %s; this file has no node of that name.", n.Name, words))
		case fn.Block != n.Block || (typed && fn.Kind != n.Kind && fn.Kind != "" && n.Kind != ""):
			p.OffCanvas = append(p.OffCanvas, fmt.Sprintf("%s: the run %s with block %s; this file's %s has block %s, so it is another node.", n.Name, words, n.Block, n.Name, fn.Block))
		default:
			onCanvas[n.Name] = true
			p.Nodes = append(p.Nodes, pathNode{Name: n.Name, Status: st, Words: words, Iters: f.Iters})
		}
	}
	reached := func(n string) bool { f, ok := fact[n]; return ok && (f.Iters > 0 || f.Attempted || f.Paused) }
	ran := func(n string) bool { return fact[n].Iters > 0 }
	fileEdge := map[[2]string]bool{}
	for _, e := range file.Edges {
		fileEdge[[2]string{e.From, e.To}] = true
	}
	fileArm := map[[2]string]plan.TopologyBranchArm{}
	for _, b := range file.Branches {
		for _, a := range b.Arms {
			fileArm[[2]string{b.Over, a.Target}] = a
		}
	}
	taken := func(from, to, label string, ok bool, exists bool, words string) {
		switch {
		case !ok:
		case exists && onCanvas[from] && onCanvas[to]:
			p.Edges = append(p.Edges, pathEdge{From: from, To: to, Words: words})
		default:
			p.OffCanvas = append(p.OffCanvas, fmt.Sprintf("%s %s -> %s: the run took it; this file does not have it between nodes the run ran.", label, from, to))
		}
	}
	for _, e := range run.Edges {
		taken(e.From, e.To, "edge", ran(e.From) && reached(e.To), fileEdge[[2]string{e.From, e.To}], "taken by the run")
	}
	for _, b := range run.Branches {
		for _, a := range b.Arms {
			tk := ran(b.Over) && reached(a.Target) && (chosen[b.Over] == a.Target || a.LoopBack && fact[a.Target].Iters > 1)
			fa, exists := fileArm[[2]string{b.Over, a.Target}]
			words := "taken by the run"
			if exists && a.LoopBack && fa.LoopMax != a.LoopMax {
				words = fmt.Sprintf("taken by the run; its loop bound was %d in the run, %d in this file", a.LoopMax, fa.LoopMax)
			}
			taken(b.Over, a.Target, "arm", tk, exists, words)
		}
	}
	return p
}
