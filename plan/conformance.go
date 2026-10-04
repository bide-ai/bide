package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journalhook"
)

// Conform checks the journaled path for runID against f's declared topology. ok
// is true iff every journaled step name maps to a declared node (or is a
// recognised internal marker of one) and every journaled Switch choice picked a
// declared arm of that Switch. diffs lists divergences by name (an unexpected
// step, or an unreachable arm taken); when ok is true diffs is empty.
//
// This is the accountability property of a declared flow (docs/guides/flows.md):
// prove the run followed the declared graph, or point at where it diverged.
// Conform reads history via agent.Journal.History and compares it to the frozen
// spec; it runs nothing, adds no executor, and never mutates the store.
//
// Conform is DECLARATION-vs-JOURNAL, not value verification. Its blind spot: it
// sees THAT a declared Step ran and which arm a Switch took, but not what the
// arbitrary Go inside a Step body actually did. A Step that runs but computes the
// wrong value still conforms; conformance is about topology and routing, not
// about the correctness of a node's computation.
//
// Conform forwards to the internal (*builderCore).conform so a single method owns
// the logic against the frozen spec; the exported method only unwraps the Flow.
func (f *Flow[In, Out]) Conform(ctx context.Context, store *agent.Journal, runID string) (ok bool, diffs []string, err error) {
	return f.core.conform(ctx, store, runID)
}

// conform is the unexported core of Flow.Conform: it reads the journal for runID
// and reconciles each recorded step against the declared topology carried by c.
//
// The record-name scheme Run writes (see Flow.Run) is:
//
//   - "run:start"      -- how the run started (see agent.RunStart). It must record a
//     flow run of this flow's name.
//   - "flow:digest"    -- the frozen topology digest Run records (StepValue whose
//     Result is the JSON-encoded hex digest). conform verifies it EQUALS the current
//     flow's Digest() and reports a mismatch as a divergence ("ran against a different
//     topology"). A journal that records any node, choice or completion must hold both
//     run:start and flow:digest.
//   - "node:<N>"       -- node N's result (StepValue whose Result is N's JSON output),
//     or "node:iter:<i>:<N>" for iteration i of a loop body; "<node key>:step:<S>" for
//     the Step S node N's body ran.
//   - the Step's attempt markers: "attempt:step:<key>" and its numbered re-attempts
//     "attempt:retry:<g>:step:<key>" (StepAttempt) for a node key or a node's Step key.
//   - "switch:<over>"  -- a switched node's journaled arm choice (StepValue whose
//     Result is the JSON-encoded chosen target step name), or "switch:iter:<i>:<over>"
//     for a loop Switch's iteration i. The chosen target must be a declared arm.
//   - "run:complete"   -- the run's completion, recording this flow's name and its output.
//
// conform replays the run's routing from its recorded choices, as Run walks it: a
// record of a node the walk did not reach (a node on an arm its Switch did not take),
// of a loop iteration the run did not reach, of a loop body node outside an iteration
// or of a node outside any loop inside one, is a divergence.
//
// The journal header and a claim's bookkeeping (a claim recorded as not started) say
// how the journal is written and how a claim was decided, not what the flow did, so
// conform ignores them.
//
// A HALTED run (Run returned *agent.OutcomeUnknown) leaves a node's attempt marker
// present with its result missing. That is a legitimate observable in-flight state,
// not a divergence: conform reports what is observable and does not require every
// attempted node to have completed. conform never panics on a partial, halted, or
// empty journal.
func (c *builderCore) conform(ctx context.Context, store *agent.Journal, runID string) (bool, []string, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return false, nil, fmt.Errorf("plan: conform run %q: load history: %w", runID, err)
	}
	recs = slices.DeleteFunc(slices.Clone(recs), func(r agent.Record) bool {
		return r.Kind == agent.StepHeader || r.Kind == agent.StepNotStarted
	})

	// Index the declared topology. byName covers every declared node; the Switch
	// set and each Switch's declared arm targets let us validate a recorded choice.
	switchArms := make(map[string]map[string]bool, len(c.branches)) // over -> set of declared arm target names
	for _, br := range c.branches {
		targets := make(map[string]bool, len(br.arms))
		for _, a := range br.arms {
			targets[a.target] = true
		}
		switchArms[br.over] = targets
	}
	loopOf := make(map[string]*loopSpec) // body node -> its loop
	loopSwitch := make(map[string]bool)  // over -> it is a loop's Switch
	for i := range c.loops {
		lp := &c.loops[i]
		loopSwitch[lp.over] = true
		for _, n := range lp.body {
			loopOf[n] = lp
		}
	}

	// The recorded choices, by key, to replay the run's routing below.
	choices := make(map[string]string)
	for _, r := range recs {
		if strings.HasPrefix(r.Name, "switch:") {
			if chosen, err := decodeSwitchChoice(r.Result); err == nil {
				choices[r.Name] = chosen
			}
		}
	}
	live, iters := c.replayRouting(choices)

	// The current flow's topology digest, so a journaled flow:digest record can be
	// checked for equality: a mismatch means the run executed against a DIFFERENT
	// declared topology than the flow now describes.
	want := c.digest()

	var diffs []string
	var haveStart, haveDigest, haveProgress bool
	var completed *completion
	// reached reports the divergence, if any, of a record of node n (at loop iteration iter,
	// -1 outside an iteration) against the replayed routing.
	reached := func(name, n string, iter int) {
		lp := loopOf[n]
		switch {
		case iter >= 0 && lp == nil:
			diffs = append(diffs, name+" (an iteration of a node outside any loop)")
		case iter < 0 && lp != nil:
			diffs = append(diffs, name+" (a loop body node recorded outside an iteration)")
		case iter >= 0 && iter >= iters[lp.head]:
			diffs = append(diffs, name+" (a loop iteration the run did not reach)")
		case iter < 0 && !live[n]:
			diffs = append(diffs, name+" (a node on a path the run did not take)")
		}
	}
	for _, r := range recs {
		switch {
		case r.Name == runStartStep:
			haveStart = true
			var st agent.RunStart
			if json.Unmarshal(r.Result, &st) != nil {
				diffs = append(diffs, r.Name+" (unreadable run start)")
				continue
			}
			if st.Kind != agent.RunKindFlow || st.Flow == nil || st.Flow.Name != c.flowName {
				diffs = append(diffs, r.Name+" (not a run of this flow)")
			}

		case r.Name == flowDigestStep:
			haveDigest = true
			got, decErr := decodeSwitchChoice(r.Result)
			if decErr != nil {
				diffs = append(diffs, r.Name+" (unreadable topology digest)")
				continue
			}
			switch {
			case got == want:
			case got == c.digestV1():
				diffs = append(diffs, r.Name+" (recorded under topology digest v1, which cannot show the run followed this flow)")
			default:
				diffs = append(diffs, r.Name+" (ran against a different topology)")
			}

		case r.Name == runCompleteStep:
			haveProgress = true
			var done completion
			if json.Unmarshal(r.Result, &done) != nil || done.Flow != c.flowName {
				diffs = append(diffs, r.Name+" (not completed by this flow)")
				continue
			}
			completed = &done

		case strings.HasPrefix(r.Name, "switch:"):
			// A journaled Switch choice: "switch:<over>" for a linear Switch, and
			// "switch:iter:<n>:<over>" for one iteration of a loop Switch. The over-node must be
			// a declared Switch of that kind, the Switch must have been reached, and the recorded
			// chosen target one of its declared arms (a loop-back arm targets the head).
			haveProgress = true
			over, iter := splitIter(strings.TrimPrefix(r.Name, "switch:"))
			arms, isSwitch := switchArms[over]
			if !isSwitch || (iter >= 0) != loopSwitch[over] {
				diffs = append(diffs, r.Name+" (switch over undeclared node)")
				continue
			}
			chosen, decErr := decodeSwitchChoice(r.Result)
			if decErr != nil {
				// A malformed choice record is observable divergence, not a panic cause.
				diffs = append(diffs, r.Name+" (unreadable switch choice)")
				continue
			}
			// An empty choice means "matched no arm and no Else" (halted routing); it
			// takes no arm, so there is no undeclared-arm divergence to report.
			if chosen != "" && !arms[chosen] {
				diffs = append(diffs, r.Name+" -> "+chosen+" (unreachable arm taken)")
				continue
			}
			reached(r.Name, over, iter)

		case strings.HasPrefix(r.Name, "attempt:"):
			// A node's attempt marker, or one of a Step its body ran. Only a marker of a key of a
			// DECLARED node is internal; any other is a divergence. A present marker whose result
			// is missing is a halted/in-flight node, not a divergence.
			key, ok := attemptedStep(r.Name)
			if !ok || r.Kind != agent.StepAttempt {
				diffs = append(diffs, r.Name+" (unexpected step)")
				continue
			}
			n, iter, isNode := parseNodeKey(key)
			if !isNode || c.byName[n] == nil {
				diffs = append(diffs, r.Name+" (attempt for undeclared step)")
				continue
			}
			haveProgress = true
			reached(r.Name, n, iter)

		default:
			// A node's result, or that of a Step its body ran.
			n, iter, isNode := parseNodeKey(r.Name)
			if !isNode || c.byName[n] == nil {
				diffs = append(diffs, r.Name+" (unexpected step)")
				continue
			}
			haveProgress = true
			reached(r.Name, n, iter)
		}
	}
	if haveProgress && !haveStart {
		diffs = append(diffs, runStartStep+" (missing: the run records nodes but not how it started)")
	}
	if haveProgress && !haveDigest {
		diffs = append(diffs, flowDigestStep+" (missing: the run records nodes but not its topology)")
	}
	if completed != nil {
		diffs = append(diffs, c.checkCompletion(completed, recs, choices, live, iters, loopOf)...)
	}

	return len(diffs) == 0, diffs, nil
}

// checkCompletion checks a completed run's journal against the routing replayed from its choices
// (live, iters): Run records every node it reaches, every choice it makes and every loop iteration
// it runs before it records the completion, and the completion's output is the recorded output of
// the last terminal node (no outgoing edge and no Switch over it), in topological order, that the
// routing reaches. A missing record, or another output, is a divergence.
func (c *builderCore) checkCompletion(done *completion, recs []agent.Record, choices map[string]string, live map[string]bool, iters map[string]int, loopOf map[string]*loopSpec) []string {
	results := make(map[string]json.RawMessage, len(recs))
	for _, r := range recs {
		if r.Kind == agent.StepValue {
			results[r.Name] = r.Result
		}
	}
	order, err := c.topoOrder()
	if err != nil {
		return []string{runCompleteStep + " (the flow has no topological order)"}
	}
	hasOut := make(map[string]bool, len(c.edges)+len(c.branches))
	switched := make(map[string]bool, len(c.branches))
	for _, e := range c.edges {
		hasOut[e.from] = true
	}
	for _, br := range c.branches {
		hasOut[br.over] = true
		switched[br.over] = true
	}
	var diffs []string
	missing := func(key string) {
		diffs = append(diffs, key+" (missing: the run completed without it)")
	}
	terminal := ""
	for _, n := range order {
		if lp := loopOf[n]; lp != nil {
			if n != lp.head {
				continue
			}
			for i := range iters[n] {
				for _, body := range lp.body {
					if _, ok := results[iterNodeKey(i, body)]; !ok {
						missing(iterNodeKey(i, body))
					}
				}
				if _, ok := choices[iterSwitchKey(i, lp.over)]; !ok {
					missing(iterSwitchKey(i, lp.over))
				}
			}
			continue
		}
		if !live[n] {
			continue
		}
		if _, ok := results[nodeKey(n)]; !ok {
			missing(nodeKey(n))
		}
		if switched[n] {
			if _, ok := choices["switch:"+n]; !ok {
				missing("switch:" + n)
			}
		}
		if !hasOut[n] {
			terminal = n
		}
	}
	switch out, ok := results[nodeKey(terminal)]; {
	case terminal == "":
		diffs = append(diffs, runCompleteStep+" (the run reached no terminal node)")
	case !ok:
		// reported missing above
	case !journalhook.SameJSON(out, done.Output):
		diffs = append(diffs, runCompleteStep+" (its output is not the terminal node's)")
	}
	return diffs
}

// replayRouting walks c's topology as Run does, from the recorded Switch choices, and returns
// the nodes the walk reaches outside loops (live) and, per loop head, how many iterations of the
// loop the run reached (iters). A Switch with no recorded choice ends its path there, as does a
// loop iteration whose Switch has none: the run has not routed past it yet.
func (c *builderCore) replayRouting(choices map[string]string) (live map[string]bool, iters map[string]int) {
	live = map[string]bool{c.entry: true}
	iters = map[string]int{}
	order, err := c.topoOrder()
	if err != nil {
		return live, iters
	}
	outEdges := make(map[string][]string, len(c.edges))
	for _, e := range c.edges {
		outEdges[e.from] = append(outEdges[e.from], e.to)
	}
	switched := make(map[string]bool, len(c.branches))
	for _, br := range c.branches {
		switched[br.over] = true
	}
	loopByHead := make(map[string]*loopSpec, len(c.loops))
	for i := range c.loops {
		loopByHead[c.loops[i].head] = &c.loops[i]
	}
	for cursor := 0; cursor < len(order); cursor++ {
		name := order[cursor]
		if !live[name] {
			continue
		}
		if lp, ok := loopByHead[name]; ok {
			for iter := 0; iter < lp.max; iter++ {
				iters[lp.head] = iter + 1
				target, ok := choices[iterSwitchKey(iter, lp.over)]
				if !ok || target == "" {
					break
				}
				if target != lp.head {
					live[target] = true
					break
				}
			}
			cursor = lp.overIdx
			continue
		}
		if switched[name] {
			if target, ok := choices["switch:"+name]; ok && target != "" {
				live[target] = true
			}
			continue
		}
		for _, to := range outEdges[name] {
			live[to] = true
		}
	}
	return live, iters
}

// runStartStep and runCompleteStep are the keys of the records of how a run started (see
// agent.RunStart) and that it completed.
const (
	runStartStep    = "run:start"
	runCompleteStep = "run:complete"
)

// parseNodeKey maps a key Run writes for a node ("node:<N>", "node:iter:<i>:<N>" for a loop
// body's iteration i, see nodeKey and iterNodeKey), or for a Step node N's body ran (that key,
// then ":step:" and the Step's name), back to N and i (-1 outside an iteration). ok is false for
// any other key. It mirrors agent's parsing of the keys its step hook runs.
func parseNodeKey(key string) (node string, iter int, ok bool) {
	rest, ok := strings.CutPrefix(key, "node:")
	if !ok {
		return "", -1, false
	}
	n, iter := splitIter(rest)
	n, tail, scoped := strings.Cut(n, ":")
	if n == "" || scoped && (!strings.HasPrefix(":"+tail, ":step:") || len(tail) == len("step:")) {
		return "", -1, false
	}
	return n, iter, true
}

// nodeOfKey is parseNodeKey's node, for a key of a node itself or of a Step its body ran.
func nodeOfKey(key string) (string, bool) {
	n, _, ok := parseNodeKey(key)
	return n, ok
}

// splitIter removes a leading iteration scope "iter:<n>:" from a key segment and returns the
// rest and n, or the segment unchanged and -1 when it has none. <n> is written as strconv.Itoa
// writes it (no leading zero), so "iter:01:x" has no iteration scope and names the node "iter".
func splitIter(name string) (string, int) {
	rest, ok := strings.CutPrefix(name, "iter:")
	if !ok {
		return name, -1
	}
	digits, node, ok := strings.Cut(rest, ":")
	if !ok || !isDigits(digits) || len(digits) > 1 && digits[0] == '0' {
		return name, -1
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return name, -1
	}
	return node, n
}

// attemptedStep returns the step key an attempt marker guards: name without
// "attempt:step:" for a first attempt, or without "attempt:retry:<g>:step:" for a
// numbered re-attempt (see agent.Journal.Step). ok is false for any other key, a tool
// call's marker among them.
func attemptedStep(name string) (string, bool) {
	if key, ok := strings.CutPrefix(name, "attempt:step:"); ok {
		return key, true
	}
	rest, ok := strings.CutPrefix(name, "attempt:retry:")
	if !ok {
		return "", false
	}
	digits, tail, ok := strings.Cut(rest, ":")
	if !ok || !isDigits(digits) {
		return "", false
	}
	return strings.CutPrefix(tail, "step:")
}

// isDigits reports whether s is one or more decimal digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// decodeSwitchChoice reads the chosen arm target from a "switch:<over>" record's
// Result (a JSON-encoded string, see Flow.chooseArm). An empty Result decodes to
// the empty target, meaning no arm was taken (no When matched and no Else).
func decodeSwitchChoice(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var target string
	if err := json.Unmarshal(raw, &target); err != nil {
		return "", err
	}
	return target, nil
}
