package agent

import (
	"context"
	"fmt"
	"sync"
)

// An agent tree is a top-level run and every run started from its tool calls: sub-agents, and
// their sub-agents. Its token spend is counted twice over, for two purposes.
//
// Durably, in the journal: a tool call's record carries the usage of the runs it started (see
// callUsage), so a run's journal holds its whole subtree's usage once those calls finish, and
// Result reports it.
//
// Live, in a budgetTree: every run's budgetNode counts the spend of its subtree known to this
// process, so a run checks its own WithTokenBudget and every enclosing run's before each model
// call. The spend of sub-runs cut off mid-run is in their own journals, not yet in their
// parent's, so a run starting in this process first loads the journals of its unfinished
// sub-agents (preloadSubRuns): a resumed tree is held to everything it has used before any of
// it calls the model again.

// budgetTree is the live token count of one agent tree in this process.
type budgetTree struct {
	mu    sync.Mutex
	nodes map[string]*budgetNode // by run ID
}

// budgetNode is one run's place in its budgetTree.
type budgetNode struct {
	tree   *budgetTree
	parent *budgetNode // the run whose tool call started this one; nil for the top-level run
	limit  int         // this run's WithTokenBudget; 0 = none
	spent  Usage       // the spend of this run and its sub-runs known so far
}

// add counts u against n and every run enclosing it.
func (n *budgetNode) add(u Usage) {
	n.tree.mu.Lock()
	defer n.tree.mu.Unlock()
	for m := n; m != nil; m = m.parent {
		addUsage(&m.spent, u)
	}
}

// exceeded returns an error wrapping ErrBudgetExceeded if n or a run enclosing it has used its
// token budget.
func (n *budgetNode) exceeded(runID string) error {
	n.tree.mu.Lock()
	defer n.tree.mu.Unlock()
	for m := n; m != nil; m = m.parent {
		if used := m.spent.TotalTokens(); m.limit > 0 && used >= m.limit {
			if m == n {
				return fmt.Errorf("run %s: %d tokens used, budget %d: %w", runID, used, m.limit, ErrBudgetExceeded)
			}
			return fmt.Errorf("run %s: its agent tree used %d tokens, an enclosing run's budget is %d: %w", runID, used, m.limit, ErrBudgetExceeded)
		}
	}
	return nil
}

// budgetNodeKey carries the budgetNode of the run whose tool call a context belongs to.
const budgetNodeKey ctxKey = 7

func withBudgetNode(ctx context.Context, n *budgetNode) context.Context {
	return context.WithValue(ctx, budgetNodeKey, n)
}

// joinBudgetTree returns runID's node in the tree of the run whose tool call ctx belongs to, or
// in a new tree when there is none (a top-level run). A new node starts with journaled, the
// spend already in the run's journal; created reports whether the node is new. A node that
// exists already counts its journal: it was preloaded by its parent, or the run was driven
// before in this process (a tool retried).
func joinBudgetTree(ctx context.Context, runID string, limit int, journaled Usage) (n *budgetNode, created bool) {
	parent, _ := ctx.Value(budgetNodeKey).(*budgetNode)
	tree := &budgetTree{nodes: map[string]*budgetNode{}}
	if parent != nil {
		tree = parent.tree
	}
	n, created = tree.node(runID, parent, limit)
	if created {
		n.add(journaled)
	}
	return n, created
}

// node returns runID's node, creating it under parent, with the run's budget limit, if the tree
// has none.
func (t *budgetTree) node(runID string, parent *budgetNode, limit int) (*budgetNode, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n, ok := t.nodes[runID]; ok {
		return n, false
	}
	n := &budgetNode{tree: t, parent: parent, limit: limit}
	t.nodes[runID] = n
	return n, true
}

// preloadSubRuns counts, against n, the journaled spend of every sub-agent call in recs (the
// journal of run runID) that has no recorded result, and of their unfinished sub-agent
// calls in turn. A call with a result carries its sub-run's usage in that record, which recs
// already counts. (A saga's failure record needs no check here: a saga that has one rolls back
// without running its loop.)
func (a *Agent) preloadSubRuns(ctx context.Context, runID string, recs []Record, n *budgetNode) error {
	done := map[string]bool{}
	for _, r := range recs {
		if r.Kind == StepToolResult {
			done[r.ToolUseID] = true
		}
	}
	for _, r := range recs {
		if r.Kind != StepModel || r.Message == nil {
			continue
		}
		for _, tu := range r.Message.toolUses() {
			st, ok := a.tools[tu.Name].(*subAgentTool)
			if done[tu.ID] || !ok {
				continue
			}
			subID := SubRunID(runID, tu.ID)
			child, created := n.tree.node(subID, n, st.sub.tokenBudget)
			if !created {
				continue
			}
			subRecs, err := st.sub.store.History(ctx, subID)
			if err != nil {
				return fmt.Errorf("load history %s: %w (%w)", subID, err, ErrStorage)
			}
			child.add(journalTotals(subRecs).spend)
			if err := st.sub.preloadSubRuns(ctx, subID, subRecs, child); err != nil {
				return err
			}
		}
	}
	return nil
}

// journalTotals is the usage journaled in recs.
func journalTotals(recs []Record) usageTotals {
	var t usageTotals
	for _, r := range recs {
		t.add(r)
	}
	return t
}

// callUsage collects, for one tool call, the usage of the runs the call started, so the call's
// record can carry it. A run reports its whole usage when it returns, keyed by its run ID: a run
// driven twice in one call (a tool retried) is counted once.
type callUsage struct {
	mu    sync.Mutex
	byRun map[string]usageTotals
}

const callUsageKey ctxKey = 8

func withCallUsage(ctx context.Context, c *callUsage) context.Context {
	return context.WithValue(ctx, callUsageKey, c)
}

// reportUsage records t as runID's usage with the tool call ctx belongs to, if any.
func reportUsage(ctx context.Context, runID string, t usageTotals) {
	c, ok := ctx.Value(callUsageKey).(*callUsage)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byRun == nil {
		c.byRun = map[string]usageTotals{}
	}
	c.byRun[runID] = t
}

// carry sets r's Usage and DiscardedUsage to the usage of the runs the call started, if any.
func (c *callUsage) carry(r *Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var t usageTotals
	for _, u := range c.byRun {
		addUsage(&t.answer, u.answer)
		addUsage(&t.spend, u.spend)
	}
	if t.answer != (Usage{}) {
		r.Usage = &t.answer
	}
	if d := discardedSpend(t.spend, t.answer); d != (Usage{}) {
		r.DiscardedUsage = &d
	}
}
