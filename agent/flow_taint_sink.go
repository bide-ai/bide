package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/bide-ai/bide/internal/strictjson"
)

// Taint coming up out of a nested flow (docs/design/flow-taint.md, decision 10). A flow run started by the body
// of a node of a run under taint is a child of that node. The node's body runs under a sink the engine binds:
// where the child sends the marks it learned, as taint-mark records for the node in the parent's journal,
// where the read set of the nodes after it is counted from them. The child sends its own marks only, never the
// ones it inherited: a mark a grandchild learned is the child's own (the grandchild's sink wrote it into the
// child's journal for the child's node), so it reaches the root through each level's record.

// markSink is where a child run sends the marks it learned: the parent's journal, run and node key.
type markSink struct {
	j       *Journal
	runID   string
	nodeKey string
}

type markSinkKey struct{}

// withMarkSink binds to ctx the sink for the runs the body of node nodeKey of run runID starts.
func withMarkSink(ctx context.Context, j *Journal, runID, nodeKey string) context.Context {
	return context.WithValue(ctx, markSinkKey{}, markSink{j: j, runID: runID, nodeKey: nodeKey})
}

// markSinkFrom is the sink ctx carries, and whether it carries one.
func markSinkFrom(ctx context.Context) (markSink, bool) {
	s, ok := ctx.Value(markSinkKey{}).(markSink)
	return s, ok
}

// taintChildPrefix is the name prefix of a taint-child record, reserved to the engine.
const taintChildPrefix = "taint-child:"

// taintChildFormat tags a taint-child record.
const taintChildFormat = "bide.taint-child.v1"

// taintChildStep is the journal name of the record that the run childRun is a child of node nodeKey.
func taintChildStep(nodeKey, childRun string) string {
	return taintChildPrefix + encodeID(nodeKey) + ":" + encodeID(childRun)
}

// taintChildRecord is what a taint-child record holds: a run a node's body started under the node's sink. A
// ResolveHalt of the node reads it to find the runs whose marks it must fold, since a body names its child runs
// as it likes and nothing else derives them.
type taintChildRecord struct {
	Format  string `json:"format"`
	NodeKey string `json:"node_key"`
	RunID   string `json:"run_id"`
	// Kind is what the run is: a plan flow's ("flow"), whose journal the fold reads, or an agent's ("agent"),
	// whose marks need its tools' declarations, which the journal does not hold.
	Kind string `json:"kind"`
}

// The kinds of a taint-child record.
const (
	childKindFlow  = "flow"
	childKindAgent = "agent"
)

// putTaintChild records, insert-once, that childRun is a child of node nodeKey of run runID.
func putTaintChild(ctx context.Context, j *Journal, runID, nodeKey, childRun, kind string) error {
	if !planNodeKey(nodeKey) {
		return fmt.Errorf("agent: a taint child names %q, which is not a plan node key: %w", nodeKey, ErrConfig)
	}
	b, err := marshalJournal(taintChildRecord{Format: taintChildFormat, NodeKey: nodeKey, RunID: childRun, Kind: kind})
	if err != nil {
		return fmt.Errorf("encode %s (run %s): %w (%w)", taintChildStep(nodeKey, childRun), runID, err, ErrConfig)
	}
	got, _, err := j.putNew(ctx, runID, taintChildStep(nodeKey, childRun), Record{Kind: StepValue, Result: b})
	if err != nil {
		return fmt.Errorf("record %s (run %s): %w", taintChildStep(nodeKey, childRun), runID, err)
	}
	_, err = decodeTaintChild(got)
	return err
}

// decodeTaintChild reads a taint-child record strictly, under the name its body determines. A record that does
// not read is ErrStorage: a fold never takes it for no child.
func decodeTaintChild(r Record) (taintChildRecord, error) {
	var rec taintChildRecord
	if err := strictjson.Unmarshal(r.Result, &rec, nil); err != nil || rec.Format != taintChildFormat || r.Name != taintChildStep(rec.NodeKey, rec.RunID) || !planNodeKey(rec.NodeKey) || rec.RunID == "" || rec.Kind != childKindFlow && rec.Kind != childKindAgent {
		return taintChildRecord{}, fmt.Errorf("agent: the record %q does not read as a %s record: %w", r.Name, taintChildFormat, ErrStorage)
	}
	return rec, nil
}

// linkChild records the run runID, which is about to start (or be driven), as a child of the node whose sink
// ctx carries, and holds it to the sink's journal: the marks go up as records of that journal, and a
// ResolveHalt of the node finds the child in it, so a child in another journal could not be folded.
func linkChild(ctx context.Context, j *Journal, runID, kind string) error {
	sink, ok := markSinkFrom(ctx)
	if !ok {
		return nil
	}
	// An agent run may keep its own store (it never sends marks into the journal: the node collects them), a
	// flow run's marks are written into the sink's journal and its own is read by the fold.
	if kind == childKindFlow && sink.j.id != j.id {
		return fmt.Errorf("agent: run %s is started by node %q of run %s, which is under taint, and uses another journal; a flow started inside a run under taint must use its parent's journal, so its marks can go up into it: %w", runID, sink.nodeKey, sink.runID, ErrConfig)
	}
	if runID == sink.runID {
		return fmt.Errorf("agent: node %q of run %s starts a flow under the run's own ID; a child needs an ID of its own: %w", sink.nodeKey, sink.runID, ErrConfig)
	}
	return putTaintChild(ctx, sink.j, sink.runID, sink.nodeKey, runID, kind)
}

// ownMarkSources are the sources of the marks a flow run's journal recs holds for the node with key nodeKey,
// or for every node when nodeKey is "": the sources the node declares, once its result is recorded, and those
// of the taint-mark records written for it (a mark its own child brought up). They are the run's own marks: the
// marks it inherited are not among them. Sorted, once each. An error is a record that does not read.
func ownMarkSources(recs []Record, decls flowTaintDecls, nodeKey string) ([]string, error) {
	nodes := decls.normalized().Nodes
	seen := map[string]bool{}
	for _, r := range recs {
		if r.Kind != StepValue {
			continue
		}
		switch {
		case strings.HasPrefix(r.Name, taintMarkPrefix):
			rec, err := decodeTaintMark(r)
			if err != nil {
				return nil, err
			}
			if nodeKey == "" || rec.NodeKey == nodeKey {
				seen[rec.Source] = true
			}
		case nodeKey == "" || r.Name == nodeKey:
			if name, ok := nodeNameOfKey(r.Name); ok {
				for _, s := range nodes[name].Untrusted {
					seen[s] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	slices.Sort(out)
	return out, nil
}

// flowTaintSink is journalhook.FlowTaintSink.
func flowTaintSink(ctx context.Context, j any, runID, nodeKey string) error {
	sink, ok := markSinkFrom(ctx)
	if !ok {
		return nil
	}
	jr, ok := j.(*Journal)
	if !ok {
		return fmt.Errorf("agent: FlowTaintSink: %T is not a journal: %w", j, ErrConfig)
	}
	recs, err := jr.History(ctx, runID)
	if err != nil {
		return fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	start := startOfRecs(recs)
	if start.taintDigest() == "" {
		return nil
	}
	decls, _, err := start.flowTaintDecls()
	if err != nil {
		return err
	}
	sources, err := ownMarkSources(recs, decls, nodeKey)
	if err != nil {
		return err
	}
	for _, s := range sources {
		if _, err := putTaintMark(ctx, sink.j, sink.runID, sink.nodeKey, s); err != nil {
			return err
		}
	}
	return nil
}

// maxChildDepth bounds how far a fold follows the children of children.
const maxChildDepth = 64

// foldChildMarks is what ResolveHalt does for node nodeKey of run runID before it records the node's result:
// the marks each child run the node started has learned go into the run's journal as taint-mark records for the
// node (insert-once), so the nodes after it are read under them. The child's journal is read, so the fold does
// not depend on the child having been driven again; its children are followed the same way. Nothing is written
// for a run that is not under taint. The marks are written before the result, so a crash between leaves marks
// the node's child did bring up, never fewer.
func foldChildMarks(ctx context.Context, j *Journal, runID, nodeKey string) error {
	recs, err := j.History(ctx, runID)
	if err != nil {
		return fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	if startOfRecs(recs).taintDigest() == "" {
		return nil
	}
	seen := map[string]bool{runID: true}
	var sources []string
	var walk func(recs []Record, nodeKey string, depth int) error
	walk = func(recs []Record, nodeKey string, depth int) error {
		for _, r := range recs {
			if r.Kind != StepValue || !strings.HasPrefix(r.Name, taintChildPrefix) {
				continue
			}
			rec, err := decodeTaintChild(r)
			if err != nil {
				return err
			}
			if nodeKey != "" && rec.NodeKey != nodeKey || seen[rec.RunID] {
				continue
			}
			if depth >= maxChildDepth {
				return fmt.Errorf("agent: run %s: the runs below node %q nest deeper than %d; their marks cannot be folded: %w", runID, rec.NodeKey, maxChildDepth, ErrConfig)
			}
			seen[rec.RunID] = true
			if rec.Kind == childKindAgent {
				// The agent's marks cannot be read from its journal, which does not hold its tools'
				// declarations: the node is marked as if the agent had read something untrusted (it over-taints,
				// and fails closed). Only this path is an approximation.
				if name, ok := nodeNameOfKey(rec.NodeKey); ok {
					sources = append(sources, subAgentMarkSource(name))
				}
				continue
			}
			child, err := j.History(ctx, rec.RunID)
			if err != nil {
				return fmt.Errorf("load history %s: %w (%w)", rec.RunID, err, ErrStorage)
			}
			start := startOfRecs(child)
			if start.taintDigest() == "" {
				continue
			}
			decls, _, err := start.flowTaintDecls()
			if err != nil {
				return err
			}
			own, err := ownMarkSources(child, decls, "")
			if err != nil {
				return err
			}
			sources = append(sources, own...)
			if err := walk(child, "", depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(recs, nodeKey, 0); err != nil {
		return err
	}
	slices.Sort(sources)
	for _, s := range slices.Compact(sources) {
		if _, err := putTaintMark(ctx, j, runID, nodeKey, s); err != nil {
			return err
		}
	}
	return nil
}

// flowEndedTainted reports whether the plan flow run runID of journal j ended with a mark: one it inherited, or
// one of its own (ownMarkSources). A run that is not under taint has none.
func flowEndedTainted(ctx context.Context, j *Journal, runID string) (bool, error) {
	recs, err := j.History(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	start := startOfRecs(recs)
	if start.taintDigest() == "" {
		return false, nil
	}
	if len(start.inheritedMarks()) > 0 {
		return true, nil
	}
	decls, _, err := start.flowTaintDecls()
	if err != nil {
		return false, err
	}
	own, err := ownMarkSources(recs, decls, "")
	return len(own) > 0, err
}

// flowTaintDriven is journalhook.FlowTaintDriven.
func flowTaintDriven(ctx context.Context, j any, runID string) {
	if rm := resultMarkFrom(ctx); rm != nil {
		if jr, ok := j.(*Journal); ok {
			rm.addFlow(jr, runID) // a run a call drives: its result carries what the run read
		}
	}
}

// ownEndedSources are the sources of the marks the agent run runID of a's store read itself: those it ended
// with, less the ones it started with (inherited from the node, which already has them).
func (a *Agent) ownEndedSources(ctx context.Context, runID string) ([]string, error) {
	ended, _, err := a.endedMarks(ctx, runID)
	if err != nil || len(ended) == 0 {
		return nil, err
	}
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	inherited := map[string]bool{}
	for _, m := range startInherited(recs) {
		inherited[m.Source] = true
	}
	var out []string
	for _, m := range ended {
		if !inherited[m.Source] {
			out = append(out, m.Source)
		}
	}
	return out, nil
}

// flowTaintBodyDone is journalhook.FlowTaintBodyDone.
func flowTaintBodyDone(ctx context.Context, j any, runID, nodeKey string) error {
	rm := resultMarkFrom(ctx)
	if rm == nil {
		return nil
	}
	jr, ok := j.(*Journal)
	if !ok {
		return fmt.Errorf("agent: FlowTaintBodyDone: %T is not a journal: %w", j, ErrConfig)
	}
	rm.mu.Lock()
	runs := slices.Clone(rm.runs)
	rm.mu.Unlock()
	for _, r := range runs {
		if r.a == nil {
			continue // a flow run sends its own marks (flowTaintSink)
		}
		sources, err := r.a.ownEndedSources(ctx, r.id)
		if err != nil {
			return err
		}
		for _, s := range sources {
			if _, err := putTaintMark(ctx, jr, runID, nodeKey, s); err != nil {
				return err
			}
		}
	}
	return nil
}
