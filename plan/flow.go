package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	agent "github.com/dayna/go-agents"
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
// docs/design/expression-surfaces.md, rung 0). It therefore inherits the
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
// Journal-record name scheme Run writes, per node named N:
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

	// Walk from the entry, carrying the current node and its decoded input. Each
	// node is driven as one durable step; the walk advances to the edge target or,
	// for a switched node, to the journaled arm target. It ends at a terminal node
	// (no outgoing edge, not switched over), whose output is the flow output.
	cur := c.byName[c.entry]
	if cur == nil {
		return out, fmt.Errorf("plan: run %q: no entry step (flow was not built)", c.flowName)
	}

	// Index the topology once for the walk: edge targets and per-node branch.
	edgeTo := make(map[string]string, len(c.edges))
	for _, e := range c.edges {
		edgeTo[e.from] = e.to
	}
	branchOf := make(map[string]branch, len(c.branches))
	for _, br := range c.branches {
		branchOf[br.over] = br
	}

	input := any(in)
	for {
		// Drive the current node as a durable step under the automatic
		// halt-on-ambiguity guard (see runNode): a recorded result replays without
		// re-running; an attempt marker with no result halts rather than re-firing a
		// possibly-completed effect; a fresh node records an attempt, runs, then
		// records its result. This makes at-most-once inherited for free by every
		// node, with no per-step opt-in.
		node := cur
		rec, err := runNode(ctx, store, runID, node, input)
		if err != nil {
			return out, err
		}

		// Route onward. Precedence mirrors Build's terminal rule: a switched node
		// routes through its arms; otherwise an edge advances the walk; otherwise the
		// node is terminal and its output is the flow output.
		if br, switched := branchOf[node.name]; switched {
			target, chooseErr := f.chooseArm(ctx, store, runID, br, node.outType, rec.Result)
			if chooseErr != nil {
				return out, chooseErr
			}
			if target == "" {
				return out, fmt.Errorf("plan: run %q: Switch over %q matched no arm and has no Else", c.flowName, br.over)
			}
			next := c.byName[target]
			if next == nil {
				return out, fmt.Errorf("plan: run %q: Switch over %q routes to unknown step %q", c.flowName, br.over, target)
			}
			// The taken arm consumes the switched node's output as its input.
			in, decErr := decodeInto(rec.Result, next.inType)
			if decErr != nil {
				return out, fmt.Errorf("plan: run %q: decode switch input for arm %q: %w", c.flowName, target, decErr)
			}
			cur = next
			input = in
			continue
		}

		if target, has := edgeTo[node.name]; has {
			next := c.byName[target]
			if next == nil {
				return out, fmt.Errorf("plan: run %q: edge routes to unknown step %q", c.flowName, target)
			}
			in, decErr := decodeInto(rec.Result, next.inType)
			if decErr != nil {
				return out, fmt.Errorf("plan: run %q: decode edge input for step %q: %w", c.flowName, target, decErr)
			}
			cur = next
			input = in
			continue
		}

		// Terminal: decode the recorded output into Out and return it.
		if len(rec.Result) > 0 {
			if decErr := json.Unmarshal(rec.Result, &out); decErr != nil {
				return out, fmt.Errorf("plan: run %q: decode terminal output of step %q: %w", c.flowName, node.name, decErr)
			}
		}
		return out, nil
	}
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

// runNode drives one node as a durable step under the automatic two-phase
// attempt/result guard, so at-most-once and halt-on-ambiguity are inherited by
// every node with no per-step opt-in. It uses only the existing substrate
// primitives (store.History and store.Do); it adds no new Durable, no goroutine,
// no scheduler.
//
// Three cases, checked against the journal:
//  1. the node's result record already exists  -> return it (memoized; body not re-run);
//  2. an attempt marker exists but the result does not -> HALT (*HaltAmbiguous), because
//     the effect may have fired before a crash and re-running could double-fire;
//  3. fresh -> record the attempt marker, invoke node.run, then record the result.
//
// The happy path (no crash) is: attempt, run, result. Because store.Do memoizes
// each record by name, a clean resume falls into case 1 for every completed node.
func runNode(ctx context.Context, store agent.Durable, runID string, node *node, input any) (agent.Record, error) {
	// Read the journal once to classify this node (case 1/2/3). History is the same
	// primitive the core loop uses for its resume gate.
	recs, err := store.History(ctx, runID)
	if err != nil {
		return agent.Record{}, fmt.Errorf("plan: run %s: load history for step %q: %w", runID, node.name, err)
	}
	var haveResult, haveAttempt bool
	var resultRec agent.Record
	marker := attemptMarker(node.name)
	for _, r := range recs {
		switch r.Name {
		case node.name:
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
		// case 2: the effect was attempted but its result was lost to a crash.
		return agent.Record{}, &HaltAmbiguous{RunID: runID, Step: node.name}
	}

	// case 3 (fresh): record the attempt marker BEFORE running the body, so a crash
	// between the effect and its result leaves the marker persisted and the result
	// missing, which case 2 detects on resume. The marker carries no payload.
	if _, err := store.Do(ctx, runID, marker, func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue}, nil
	}); err != nil {
		return agent.Record{}, fmt.Errorf("plan: run %s: record attempt for step %q: %w", runID, node.name, err)
	}

	// Run the body and record its result. store.Do memoizes by name, so a resume
	// after a clean result falls into case 1 above.
	return store.Do(ctx, runID, node.name, func(ctx context.Context) (agent.Record, error) {
		result, runErr := node.run(ctx, input)
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

// chooseArm evaluates a Switch over the journaled output of the switched node and
// journals the chosen arm target as its OWN durable step named "switch:"+over.
// The choice is recorded (StepValue whose Result is the JSON-encoded target
// name), so a resumed run replays the recorded branch rather than re-evaluating
// the predicates: this is why the predicates must be pure over the switched value
// (see When). It returns the chosen target step name; the name is "" when no When
// arm matched and there is no Else.
func (f *Flow[In, Out]) chooseArm(ctx context.Context, store agent.Durable, runID string, br branch, switchedType reflect.Type, switchedOut json.RawMessage) (string, error) {
	rec, err := store.Do(ctx, runID, "switch:"+br.over, func(context.Context) (agent.Record, error) {
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
