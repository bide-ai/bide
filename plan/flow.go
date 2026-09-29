package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"text/template"

	"github.com/bide-ai/bide/agent"
)

// Flow[In, Out] is a built, frozen flow: the reified topology validated by
// Build, ready to run durably. In and Out are the pinned flow boundary types.
// Its spec is sealed at Build time, so a Flow value is immutable and safe to run
// repeatedly (each Run keyed by a distinct runID). Flow adds no executor: Run
// lowers the topology onto the journal-backed runtime via agent.Step.
type Flow[In, Out any] struct {
	core *builderCore // frozen at Build; never mutated after
}

// Run executes the lowered flow durably for runID against store, resuming from
// the journal if steps already exist. In is fed to the entry step; the value the
// terminal step produces is decoded into Out and returned.
//
// Run is STRICTLY SEQUENTIAL and adds no executor: it walks the declared
// topology one node at a time and drives each node as a named durable step
// through store.Do, exactly as plain Go control flow over agent.Step would (see
// docs/guides/flows.md). It therefore inherits the
// substrate's guarantees unchanged: a completed step returns its recorded value
// without re-running (at-most-once by name), and a resumed run replays recorded
// steps rather than re-executing them.
//
// Halt-on-ambiguity is AUTOMATIC and applies to every node, with no per-step
// opt-in (see runNode). Each node runs under the substrate's two-phase
// attempt/result protocol: Run records an attempt marker before invoking the
// node body, then records the result. On resume, a node whose attempt marker is
// present but whose result is missing crashed mid-effect with an unknown outcome,
// so Run HALTS (returns *HaltAmbiguous) rather than re-running the body. This is
// the plan surface inheriting the crown-jewel property of the substrate: a
// non-idempotent side effect fires at most once across a crash, for free, without
// the step author declaring any Safety.
//
// A Switch is lowered the same way: Run reads the switched node's journaled
// output, evaluates the arm predicates ONCE, and records the chosen arm target
// as its own durable step named "switch:"+over. On resume the recorded choice is
// replayed rather than re-decided, so the predicates must be pure over the
// switched value (see When). Only the taken arm's downstream path executes.
//
// Journal-record name scheme Run writes:
//   - "flow:digest" -- the frozen topology digest (StepValue whose Result is the
//     JSON-encoded hex digest), recorded FIRST, before any node runs, and memoized on
//     resume. It is an internal record of the run, not a declared node; conformance
//     recognises it and verifies it equals the current flow's Digest() (see Conform).
//
// then, per node named N:
//   - "attempt:"+N  -- the attempt marker (StepValue, no Result), recorded before N runs;
//   - N             -- N's result (StepValue whose Result is the JSON-encoded output);
//   - "switch:"+over -- for a switched node, the journaled arm choice (StepValue whose
//     Result is the JSON-encoded chosen target step name).
//
// The attempt marker "attempt:"+N is an internal step of node N, not a distinct
// declared node; conformance treats it as belonging to N.
func (f *Flow[In, Out]) Run(ctx context.Context, store agent.Durable, runID string, in In) (Out, error) {
	var out Out
	c := f.core

	// The FIRST thing Run records is the frozen flow's topology digest, under the
	// reserved step name flow:digest, so the audit layer's Merkle tree and signed
	// tree head cover it (see Digest). store.Do memoizes it by name, so a resumed run
	// replays the recorded digest rather than recomputing and re-recording it. This is
	// what makes it offline-verifiable that the run followed THIS declared topology.
	want, encErr := json.Marshal(c.digest())
	if encErr != nil {
		return out, fmt.Errorf("plan: run %q: encode topology digest: %w", c.flowName, encErr)
	}
	rec, err := store.Do(ctx, runID, flowDigestStep, func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: want}, nil
	})
	if err != nil {
		return out, fmt.Errorf("plan: run %q: record topology digest: %w", c.flowName, err)
	}
	// A resumed run must follow the topology it started under: its journal (node keys, branch
	// choices, loop iterations) only means what it meant under that flow. Resuming it with a
	// changed flow would reuse stale results under a different graph, so refuse.
	var got string
	if err := json.Unmarshal(rec.Result, &got); err != nil || got != c.digest() {
		return out, fmt.Errorf("plan: run %q: run %s was started under flow digest %s, not this flow's %s; resume it with the flow it started with: %w", c.flowName, runID, rec.Result, c.digest(), agent.ErrConfig)
	}

	if c.byName[c.entry] == nil {
		return out, fmt.Errorf("plan: run %q: no entry step (flow was not built)", c.flowName)
	}

	// Execute the reachable DAG in a deterministic topological order, strictly
	// sequentially: one node at a time, each under the automatic halt-on-ambiguity
	// guard (see runNode). This is a strict superset of the old single-active-path
	// walk: a linear or switched flow visits the same nodes in the same order, while
	// a fan-out (a node with several successors) and a fan-in (a Join with several
	// inputs) are handled by ordering alone. There is NO goroutine, channel, or
	// scheduler: a Join "barrier" is realized purely by topological order (a Join
	// appears after all its inputs in topoOrder, so their results are already
	// journaled when it runs), not by concurrency.
	order, err := c.topoOrder()
	if err != nil {
		return out, fmt.Errorf("plan: run %q: %w", c.flowName, err)
	}

	// Index the topology once: edge successors, per-node branch, and each node's
	// recorded output for a downstream node (or Join) to consume as input.
	outEdges := make(map[string][]string, len(c.edges))
	for _, e := range c.edges {
		outEdges[e.from] = append(outEdges[e.from], e.to)
	}
	branchOf := make(map[string]branch, len(c.branches))
	for _, br := range c.branches {
		branchOf[br.over] = br
	}

	// Index the bounded loops by their head node. When the walk reaches a live loop
	// head it hands the whole loop region (head..switch inclusive) to runLoop, which
	// drives the body iteratively under iteration-scoped journal keys and returns the
	// exit choice; the flat walk then continues forward from past the loop switch. A
	// loop's Switch is therefore NOT handled by the ordinary switched-node path below
	// (loopSwitches records it so the walk skips it if reached directly).
	loopByHead := make(map[string]*loopSpec, len(c.loops))
	loopSwitches := make(map[string]bool, len(c.loops))
	for i := range c.loops {
		lp := &c.loops[i]
		loopByHead[lp.head] = lp
		loopSwitches[lp.over] = true
	}

	// live marks a node the walk has decided must execute: the entry is live, an
	// edge target becomes live when its (non-switched) source runs, and a switched
	// node's CHOSEN arm becomes live (the other arms stay pruned). results holds each
	// executed node's journaled output so a successor or Join can decode it as input.
	live := map[string]bool{c.entry: true}
	results := make(map[string]json.RawMessage, len(c.nodes))

	// haveTerminal/terminalOut carry the output of the last live terminal reached in
	// topological order (a live node with no outgoing edge and not switched over). A
	// diamond reconverges to a single Join terminal, so this is unambiguous for the
	// intended fan-out-then-join shape; a bare fan-out with several live terminals
	// resolves to the last one in topological order, deterministically.
	var terminalOut json.RawMessage
	haveTerminal := false

	// Walk the forward order with an explicit cursor rather than a range, so a loop
	// region can advance the cursor past its body once runLoop has driven it. The walk
	// is still strictly sequential: one node (or one whole loop region) at a time, no
	// goroutine, channel, or scheduler.
	for cursor := 0; cursor < len(order); cursor++ {
		name := order[cursor]
		if !live[name] {
			continue // pruned: reachable only via a not-taken Switch arm, or never scheduled
		}

		// A live loop head hands the whole region to runLoop, which iterates the body
		// under iteration-scoped keys, then routes to the exit arm and returns the exit
		// target and its input value. The walk skips over the body (already driven) and
		// continues from the exit target.
		if lp, isHead := loopByHead[name]; isHead {
			headInput, inErr := f.nodeInput(c, c.byName[name], in, results, live, outEdges, branchOf)
			if inErr != nil {
				return out, inErr
			}
			exitTarget, exitVal, loopErr := f.runLoop(ctx, store, runID, c, lp, in, results, live, branchOf, outEdges, headInput)
			if loopErr != nil {
				return out, loopErr
			}
			// The exit target consumes the switched value the loop Switch routed to it, so
			// seed results under the loop Switch's name (its journaled arm input source) and
			// mark the exit target live. Advance the cursor past the loop region.
			results[lp.over] = exitVal
			live[exitTarget] = true
			cursor = lp.overIdx // the loop for-post increments to overIdx+1
			continue
		}

		// A loop Switch reached directly on the forward walk means the loop head above it
		// was not live, which cannot happen for a reachable loop; guard defensively.
		if loopSwitches[name] {
			return out, fmt.Errorf("plan: run %q: loop Switch over %q reached without its loop head running", c.flowName, name)
		}

		node := c.byName[name]

		// Resolve this node's input. A Join gathers its ordered inputs from the
		// already-journaled results of its input sources (topological order guarantees
		// they ran first). Every other node consumes the single upstream value: the flow
		// input for the entry, or the recorded output of its one live predecessor.
		input, inErr := f.nodeInput(c, node, in, results, live, outEdges, branchOf)
		if inErr != nil {
			return out, inErr
		}

		// Drive the node as a durable step under the automatic attempt/result guard
		// (see runNode): a recorded result replays without re-running; an attempt with
		// no result halts rather than re-firing a possibly-completed effect; a fresh
		// node records an attempt, runs, then records its result. This holds for a Join
		// exactly as for any node: it is a plain sequential step, so a non-idempotent
		// merge halts on the ambiguous crash unless marked retry-safe.
		rec, runErr := runNode(ctx, store, runID, c.model, node, input)
		if runErr != nil {
			return out, runErr
		}
		results[name] = rec.Result

		// Route onward, marking live successors. A switched node routes ONLY through
		// its chosen arm (its edge successors, if any, are not followed), mirroring the
		// old walk's precedence; the choice is journaled so a resume replays it. Every
		// other node makes all its edge targets live (fan-out).
		if br, switched := branchOf[name]; switched {
			target, chooseErr := f.chooseArm(ctx, store, runID, br, node.outType, rec.Result)
			if chooseErr != nil {
				return out, chooseErr
			}
			if target == "" {
				return out, fmt.Errorf("plan: run %q: Switch over %q matched no arm and has no Else", c.flowName, br.over)
			}
			if c.byName[target] == nil {
				return out, fmt.Errorf("plan: run %q: Switch over %q routes to unknown step %q", c.flowName, br.over, target)
			}
			live[target] = true
			continue
		}

		succ := outEdges[name]
		if len(succ) == 0 {
			// Terminal: no outgoing edge and not switched. Record its output as a
			// candidate flow output.
			terminalOut = rec.Result
			haveTerminal = true
			continue
		}
		for _, to := range succ {
			live[to] = true
		}
	}

	if !haveTerminal {
		return out, fmt.Errorf("plan: run %q: no live terminal reached", c.flowName)
	}
	if len(terminalOut) > 0 {
		if decErr := json.Unmarshal(terminalOut, &out); decErr != nil {
			return out, fmt.Errorf("plan: run %q: decode terminal output: %w", c.flowName, decErr)
		}
	}
	return out, nil
}

// runLoop drives one bounded loop region (head..switch inclusive) iteratively and
// returns the exit arm's target and the switched value routed to it. It is a plain
// sequential Go for-loop bounded by lp.max: NO goroutine, channel, or scheduler. Each
// iteration runs the body nodes in forward order under ITERATION-SCOPED journal keys
// (iter:<n>:<node>, and switch:iter:<n>:<over> for the loop Switch), so the two-phase
// attempt/result guard, at-most-once, HaltAmbiguous, and resume all hold per
// iteration exactly as for a linear flow: a completed iteration replays from the
// journal (each runNodeKeyed falls into the memoized case), and the first incomplete
// iteration resumes mid-body.
//
// The head's input is headInput on iteration 0 (its forward predecessor's value) and
// the previous iteration's switched value on iteration n > 0 (the value the back-edge
// re-routes into the head). Body nodes read the current iteration's upstream values
// from the shared results map, which each iteration overwrites, so nodeInput resolves
// them against this iteration's outputs.
//
// If the loop would re-enter the head more than lp.max times without taking an exit
// arm, runLoop returns a runaway-loop error rather than looping forever. The runtime
// iteration count is NOT part of the digest (it is runtime, like Safety); only the
// loop structure and its max bound are.
func (f *Flow[In, Out]) runLoop(ctx context.Context, store agent.Durable, runID string, c *builderCore, lp *loopSpec, in In, results map[string]json.RawMessage, live map[string]bool, branchOf map[string]branch, outEdges map[string][]string, headInput any) (string, json.RawMessage, error) {
	br := branchOf[lp.over]
	for iter := 0; ; iter++ {
		if iter >= lp.max {
			return "", nil, fmt.Errorf("plan: run %q: loop over %q exceeded its bound of %d iterations without taking an exit arm (runaway loop)", c.flowName, lp.over, lp.max)
		}

		// Run each body node in forward order under this iteration's scoped key. The head
		// takes the iteration input directly (headInput on iteration 0, the previous
		// iteration's switched value thereafter); every other body node resolves its input
		// from the results this iteration has already produced (nodeInput reads the shared
		// results map, which the body overwrites in place each iteration). All body nodes
		// are marked live so nodeInput's live-predecessor lookup succeeds inside the region.
		for _, name := range lp.body {
			live[name] = true
		}
		var switchOut json.RawMessage
		for _, name := range lp.body {
			node := c.byName[name]
			var input any
			if name == lp.head {
				input = headInput
			} else {
				resolved, inErr := f.nodeInput(c, node, in, results, live, outEdges, branchOf)
				if inErr != nil {
					return "", nil, inErr
				}
				input = resolved
			}

			rec, runErr := runNodeKeyed(ctx, store, runID, c.model, node, iterKey(iter, name), input)
			if runErr != nil {
				return "", nil, runErr
			}
			results[name] = rec.Result
			if name == lp.over {
				switchOut = rec.Result
			}
		}

		// Evaluate the loop Switch for this iteration, journaled under an iteration-scoped
		// key so the choice is made once and replayed on resume. The loop-back arm
		// re-enters the head with the switched value; any other (exit) arm returns to Run.
		target, chooseErr := f.chooseArmKeyed(ctx, store, runID, iterSwitchKey(iter, lp.over), br, c.byName[lp.over].outType, switchOut)
		if chooseErr != nil {
			return "", nil, chooseErr
		}
		if target == "" {
			return "", nil, fmt.Errorf("plan: run %q: loop Switch over %q matched no arm and has no Else", c.flowName, lp.over)
		}
		if c.byName[target] == nil {
			return "", nil, fmt.Errorf("plan: run %q: loop Switch over %q routes to unknown step %q", c.flowName, lp.over, target)
		}
		if target == lp.head {
			// Back-edge: re-enter the head with the switched value and run the body again.
			headInput, chooseErr = decodeInto(switchOut, c.byName[lp.head].inType)
			if chooseErr != nil {
				return "", nil, fmt.Errorf("plan: run %q: loop over %q decode back-edge value for head %q: %w", c.flowName, lp.over, lp.head, chooseErr)
			}
			continue
		}
		// Exit arm: hand the switched value and the exit target back to Run.
		return target, switchOut, nil
	}
}

// nodeInput resolves the decoded input a node consumes when Run reaches it. A Join
// gathers its ordered inputs from the already-journaled results of its input source
// nodes (topological order guarantees they ran first) and returns them as a []any
// positionally aligned with the merge function's parameters; the merge closure
// installed by Join2/Join3 asserts each element's concrete type. Every other node
// consumes a single upstream value: the flow input in for the entry, the switched
// node's output for a Switch arm target, or the recorded output of its one live
// predecessor edge for an ordinary edge target. Each value is decoded into the
// consumer's concrete Go type so a Step/Tool/Model body (or a Switch predicate)
// receives the type it expects rather than the neutral JSON shape.
func (f *Flow[In, Out]) nodeInput(c *builderCore, node *node, in In, results map[string]json.RawMessage, live map[string]bool, outEdges map[string][]string, branchOf map[string]branch) (any, error) {
	if node.kind == kindJoin {
		inputs := make([]any, len(node.joinInputs))
		for i, src := range node.joinInputs {
			decoded, decErr := decodeInto(results[src], node.joinInTypes[i])
			if decErr != nil {
				return nil, fmt.Errorf("plan: run %q: decode join %q input %q: %w", c.flowName, node.name, src, decErr)
			}
			inputs[i] = decoded
		}
		return inputs, nil
	}

	if node.name == c.entry {
		return any(in), nil
	}

	// Find the live predecessor whose output feeds this node. It is either the
	// switched node that chose this node as its arm target, or the single ordinary
	// edge source that ran. There is exactly one in a valid rung-1 topology (fan-in
	// is only via Join, which is handled above); Build enforces that.
	for _, br := range branchOf {
		for _, a := range br.arms {
			if a.target == node.name && live[br.over] {
				decoded, decErr := decodeInto(results[br.over], node.inType)
				if decErr != nil {
					return nil, fmt.Errorf("plan: run %q: decode switch input for arm %q: %w", c.flowName, node.name, decErr)
				}
				return decoded, nil
			}
		}
	}
	for src, tos := range outEdges {
		for _, to := range tos {
			if to == node.name && live[src] && results[src] != nil {
				decoded, decErr := decodeInto(results[src], node.inType)
				if decErr != nil {
					return nil, fmt.Errorf("plan: run %q: decode edge input for step %q: %w", c.flowName, node.name, decErr)
				}
				return decoded, nil
			}
		}
	}
	return nil, fmt.Errorf("plan: run %q: node %q has no journaled predecessor input", c.flowName, node.name)
}

// HaltAmbiguous is returned by Run when a resumed node has a recorded attempt
// marker but no recorded result: the node's effect may have fired before the
// crash, so its outcome is unknown. Run stops rather than re-run the body and
// risk a double side effect, mirroring the core runtime's ResumeHalt. Step names
// the node that halted. Resolve it out of band (confirm whether the effect landed
// and record the result, or discard the run); Run does not decide that.
type HaltAmbiguous struct {
	RunID string
	Step  string
}

func (e *HaltAmbiguous) Error() string {
	return fmt.Sprintf("plan: run %s halted at step %q: an attempt was recorded but no result, so the outcome is unknown and re-running could double-fire; confirm before continuing", e.RunID, e.Step)
}

// attemptMarker is the journal name of a node's attempt marker: the "about to run
// this node's effect" record written before the node body runs. It is an internal
// step of the node named name, not a separate declared node.
func attemptMarker(name string) string { return "attempt:" + name }

// iterKey returns the ITERATION-SCOPED journal key for a node executed on iteration
// iter of a bounded loop body: "iter:<n>:<node>". Run journals every loop-body node
// under this key, so the two-phase attempt/result guard, at-most-once,
// HaltAmbiguous, and resume all hold per iteration. attemptMarker prefixes it to
// form "attempt:iter:<n>:<node>". conform strips the "iter:<n>:" prefix to map the
// key back to its declared node. iteration 0 is the first pass through the head.
func iterKey(iter int, node string) string {
	return "iter:" + strconv.Itoa(iter) + ":" + node
}

// iterSwitchKey returns the iteration-scoped journal key for a loop Switch's choice
// on iteration iter: "switch:iter:<n>:<over>". It mirrors the linear "switch:<over>"
// key so each iteration records and replays its own branch decision.
func iterSwitchKey(iter int, over string) string {
	return "switch:iter:" + strconv.Itoa(iter) + ":" + over
}

// runNode drives one node as a durable step under the automatic two-phase
// attempt/result guard, so at-most-once and halt-on-ambiguity are inherited by
// every node with no per-step opt-in. It uses only the existing substrate
// primitives (store.History and store.Do); it adds no new Durable, no goroutine,
// no scheduler.
//
// Three cases, checked against the journal:
//  1. the node's result record already exists  -> return it (memoized; body not re-run);
//  2. an attempt marker exists but the result does not -> the node crashed mid-effect
//     with an unknown outcome. If the node is retry-safe (nodeRetriableOnResume: its
//     Safety is ReadOnly or Idempotent, or carries an IdempotencyKey, mirroring the
//     core loop's classification), RE-RUN the body and record the result. Otherwise
//     HALT (*HaltAmbiguous), because re-running a non-idempotent effect could double-fire;
//  3. fresh -> record the attempt marker, invoke node.run, then record the result.
//
// The happy path (no crash) is: attempt, run, result. Because store.Do memoizes
// each record by name, a clean resume falls into case 1 for every completed node.
// Only case 2 consults node.safety; the happy path and the completed-result replay
// path (case 1) are unchanged, so a node's Safety opt-in changes resume behavior
// only, never a clean run.
func runNode(ctx context.Context, store agent.Durable, runID string, model agent.Model, node *node, input any) (agent.Record, error) {
	return runNodeKeyed(ctx, store, runID, model, node, node.name, input)
}

// runNodeKeyed is runNode with an explicit journal key, so a node inside a bounded
// loop body can be journaled under an ITERATION-SCOPED key (iter:<n>:<node>) while
// still dispatching the same node body. The key names both the result record and,
// via attemptMarker, the attempt marker, so the two-phase guard, at-most-once,
// HaltAmbiguous, and resume all hold per iteration exactly as they do per node for a
// linear flow. For a non-loop node the key is just node.name and the behavior is
// identical to before. It adds no new primitive: still only store.History/store.Do.
func runNodeKeyed(ctx context.Context, store agent.Durable, runID string, model agent.Model, node *node, key string, input any) (agent.Record, error) {
	// Read the journal once to classify this node (case 1/2/3). History is the same
	// primitive the core loop uses for its resume gate.
	recs, err := store.History(ctx, runID)
	if err != nil {
		return agent.Record{}, fmt.Errorf("plan: run %s: load history for step %q: %w", runID, key, err)
	}
	var haveResult, haveAttempt bool
	var resultRec agent.Record
	marker := attemptMarker(key)
	for _, r := range recs {
		switch r.Name {
		case key:
			haveResult = true
			resultRec = r
		case marker:
			haveAttempt = true
		}
	}
	if haveResult {
		return resultRec, nil // case 1: memoized result, do not re-run the body
	}
	if haveAttempt {
		// case 2: the effect was attempted but its result was lost to a crash. A
		// retry-safe node (ReadOnly/Idempotent, mirroring the core loop) may safely
		// re-run its body from the top, so fall through to record the result below (the
		// attempt marker is already persisted, so it is not re-recorded). A
		// non-idempotent node HALTS rather than risk a double side effect.
		if !nodeRetriableOnResume(node.safety) {
			return agent.Record{}, &HaltAmbiguous{RunID: runID, Step: key}
		}
	} else {
		// case 3 (fresh): record the attempt marker BEFORE running the body, so a crash
		// between the effect and its result leaves the marker persisted and the result
		// missing, which case 2 detects on resume. A retry-safe node in case 2 skips this
		// because its marker already exists. The marker is an exclusive claim
		// (agent.ClaimAttempt): if another driver of this run claimed the step first, it owns
		// the body, and a non-idempotent step halts here rather than run it a second time.
		won, _, err := agent.ClaimAttempt(ctx, store, runID, marker, agent.Record{Kind: agent.StepValue})
		if err != nil {
			return agent.Record{}, fmt.Errorf("plan: run %s: record attempt for step %q: %w", runID, key, err)
		}
		if !won && !nodeRetriableOnResume(node.safety) {
			return agent.Record{}, &HaltAmbiguous{RunID: runID, Step: key}
		}
	}

	// Run the body and record its result. store.Do memoizes by name, so a resume
	// after a clean result falls into case 1 above. For a retry-safe node resuming
	// from case 2, this re-runs the body and records the result the crash lost.
	return store.Do(ctx, runID, key, func(ctx context.Context) (agent.Record, error) {
		var result any
		var runErr error
		switch node.kind {
		case kindModel:
			// A Model node has no run closure: it renders its prompt from the input,
			// calls the flow's bound model, and decodes the structured result into the
			// node's output type. The model is bound to the flow (WithModel) and read at
			// run time. Build guarantees it is non-nil for a flow with a Model node.
			result, runErr = runModel(ctx, model, node, input)
		case kindJoin:
			// A Join node has no run closure: it dispatches to its erased merge, which
			// receives the ordered inputs (a []any assembled by nodeInput from the
			// already-journaled input results) and returns the merged output. Because the
			// join runs after all its inputs in topological order, this is a plain
			// sequential step, guarded exactly like every other node.
			inputs, ok := input.([]any)
			if !ok {
				runErr = fmt.Errorf("plan: join %q expected ordered inputs, got %T", node.name, input)
			} else {
				result, runErr = node.merge(ctx, inputs)
			}
		default:
			result, runErr = node.run(ctx, input)
		}
		if runErr != nil {
			return agent.Record{}, runErr
		}
		encoded, encErr := json.Marshal(result)
		if encErr != nil {
			return agent.Record{}, fmt.Errorf("plan: step %q encode result: %w", node.name, encErr)
		}
		return agent.Record{Kind: agent.StepValue, Result: encoded}, nil
	})
}

// runModel is the body of a kindModel node: it renders the node's prompt as a Go
// text/template with the decoded input as data, calls the flow's bound model, and
// decodes the model's text response as JSON into a fresh value of the node's output
// type (returned boxed as any, mirroring a Step's run closure). This mirrors the
// core's typed-output decode path (RunTypedNative json.Unmarshal-s the assistant's
// text into the typed result): because the plan surface imports only stdlib and the
// core agent (not the schema helper, which is generic over a type parameter and
// cannot be reached from a reflect.Type here), the output type O must be JSON-shaped
// and the prompt must instruct the model to answer with matching JSON.
func runModel(ctx context.Context, model agent.Model, node *node, input any) (any, error) {
	if model == nil {
		// Defensive: Build rejects a Model node with no bound model, so this should be
		// unreachable. Kept so a hand-assembled core (bypassing Build) fails loudly with
		// the node name rather than nil-dereferencing.
		return nil, fmt.Errorf("plan: model step %q has no bound model", node.name)
	}

	// Render the prompt as a text/template with the decoded input as data. A parse or
	// execute error names the node so an author's template mistake is legible.
	tmpl, err := template.New(node.name).Parse(node.prompt)
	if err != nil {
		return nil, fmt.Errorf("plan: model step %q parse prompt template: %w", node.name, err)
	}
	var rendered strings.Builder
	if err := tmpl.Execute(&rendered, input); err != nil {
		return nil, fmt.Errorf("plan: model step %q render prompt: %w", node.name, err)
	}

	// Send the rendered prompt as a single user message and drain the model's turn.
	msg, _, err := agent.Generate(ctx, model, agent.Request{
		Messages: []agent.Message{agent.UserText(rendered.String())},
	})
	if err != nil {
		return nil, fmt.Errorf("plan: model step %q generate: %w", node.name, err)
	}

	// Decode the assistant's text as JSON into a fresh value of the node's output
	// type, so the next node (or the flow terminal) receives the concrete Go type.
	out, err := decodeInto(json.RawMessage(msg.Text()), node.outType)
	if err != nil {
		return nil, fmt.Errorf("plan: model step %q decode structured output into %s (the model must answer with JSON for this type): %w", node.name, typeName(node.outType), err)
	}
	return out, nil
}

// chooseArm evaluates a Switch over the journaled output of the switched node and
// journals the chosen arm target as its OWN durable step named "switch:"+over.
// The choice is recorded (StepValue whose Result is the JSON-encoded target
// name), so a resumed run replays the recorded branch rather than re-evaluating
// the predicates: this is why the predicates must be pure over the switched value
// (see When). It returns the chosen target step name; the name is "" when no When
// arm matched and there is no Else.
func (f *Flow[In, Out]) chooseArm(ctx context.Context, store agent.Durable, runID string, br branch, switchedType reflect.Type, switchedOut json.RawMessage) (string, error) {
	return f.chooseArmKeyed(ctx, store, runID, "switch:"+br.over, br, switchedType, switchedOut)
}

// chooseArmKeyed is chooseArm with an explicit journal key, so a loop Switch can
// journal each iteration's choice under an iteration-scoped key
// (switch:iter:<n>:<over>). The recorded choice is replayed on resume exactly as for
// a linear Switch, so each iteration's branch decision is made once and the
// predicates stay pure over the switched value.
func (f *Flow[In, Out]) chooseArmKeyed(ctx context.Context, store agent.Durable, runID, key string, br branch, switchedType reflect.Type, switchedOut json.RawMessage) (string, error) {
	rec, err := store.Do(ctx, runID, key, func(context.Context) (agent.Record, error) {
		// Decode the switched output into the switched node's concrete Go type, so a
		// predicate typed to that value (the type-erased closure asserts v.(M), see
		// wiring.go) receives the Go value rather than the neutral JSON shape a plain
		// decode into any produces (an int would arrive as float64, a struct as
		// map[string]any, and the assertion would fail).
		value, decErr := decodeInto(switchedOut, switchedType)
		if decErr != nil {
			return agent.Record{}, fmt.Errorf("plan: switch over %q decode output: %w", br.over, decErr)
		}
		target := ""
		for _, a := range br.arms {
			if a.isElse {
				continue // Else is the fallback; considered only after When arms
			}
			if a.pred != nil && a.pred(value) {
				target = a.target
				break
			}
		}
		if target == "" { // no When matched: take the Else if present
			for _, a := range br.arms {
				if a.isElse {
					target = a.target
					break
				}
			}
		}
		encoded, encErr := json.Marshal(target)
		if encErr != nil {
			return agent.Record{}, fmt.Errorf("plan: switch over %q encode choice: %w", br.over, encErr)
		}
		return agent.Record{Kind: agent.StepValue, Result: encoded}, nil
	})
	if err != nil {
		return "", err
	}
	var target string
	if len(rec.Result) > 0 {
		if decErr := json.Unmarshal(rec.Result, &target); decErr != nil {
			return "", fmt.Errorf("plan: switch over %q decode recorded choice: %w", br.over, decErr)
		}
	}
	return target, nil
}

// decodeInto decodes a journaled JSON result into a fresh value of the given
// reflect type and returns it boxed as any, so the next node's type-erased run
// closure (and a Switch predicate) receives the concrete Go type it expects
// rather than the neutral shape a decode into interface{} yields. A nil type or
// empty result yields a nil any.
func decodeInto(raw json.RawMessage, into reflect.Type) (any, error) {
	if into == nil || len(raw) == 0 {
		return nil, nil
	}
	ptr := reflect.New(into) // *into
	if err := json.Unmarshal(raw, ptr.Interface()); err != nil {
		return nil, err
	}
	return ptr.Elem().Interface(), nil
}

// RenderMermaid returns a Mermaid flowchart of the DECLARED topology carried by
// this frozen Flow, each node labelled by its journal key and its I,O types. It
// forwards to the pure builderCore renderer (Agent C); it is the authored
// diagram, as opposed to agent.RenderMermaid, which derives the actual-ran
// diagram from the journal, and Conform compares the two.
func (f *Flow[In, Out]) RenderMermaid() string { return f.core.renderMermaid() }
