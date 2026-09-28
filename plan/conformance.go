package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// Conform checks the journaled path for runID against f's declared topology. ok
// is true iff every journaled step name maps to a declared node (or is a
// recognised internal marker of one) and every journaled Switch choice picked a
// declared arm of that Switch. diffs lists divergences by name (an unexpected
// step, or an unreachable arm taken); when ok is true diffs is empty.
//
// This is the accountability property of docs/design/expression-surfaces.md:
// prove the run followed the declared graph, or point at where it diverged.
// Conform reads history via agent.Durable.History and compares it to the frozen
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
func (f *Flow[In, Out]) Conform(ctx context.Context, store agent.Durable, runID string) (ok bool, diffs []string, err error) {
	return f.core.conform(ctx, store, runID)
}

// conform is the unexported core of Flow.Conform: it reads the journal for runID
// and reconciles each recorded step against the declared topology carried by c.
//
// The record-name scheme Run writes (see Flow.Run) is:
//
//   - "flow:digest"    -- the frozen topology digest Run records first (StepValue whose
//     Result is the JSON-encoded hex digest). It is an INTERNAL record of the run, not a
//     declared node, so it is never an unexpected step; conform verifies it EQUALS the
//     current flow's Digest() and reports a mismatch as a divergence ("ran against a
//     different topology").
//   - "<N>"            -- node N's result (StepValue whose Result is N's JSON output).
//   - "attempt:<N>"    -- an attempt marker written BEFORE node N's body runs
//     (StepValue, empty Result). It is an INTERNAL step of node N, not a distinct
//     declared node, so it maps to N and is never a divergence on its own.
//   - "switch:<over>"  -- a switched node's journaled arm choice (StepValue whose
//     Result is the JSON-encoded chosen target step name). It maps to the Switch
//     declared over "<over>"; the recorded chosen target must be a declared arm of
//     that Switch, else an unreachable/undeclared arm was taken (a divergence).
//
// A HALTED run (Run returned *HaltAmbiguous) leaves "attempt:<N>" present with the
// "<N>" result missing for the halted node. That is a legitimate observable
// in-flight state, not a divergence: conform reports what is observable and does
// not require every attempted node to have completed. conform never panics on a
// partial, halted, or empty journal.
func (c *builderCore) conform(ctx context.Context, store agent.Durable, runID string) (bool, []string, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return false, nil, fmt.Errorf("plan: conform run %q: load history: %w", runID, err)
	}

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

	// The current flow's topology digest, so a journaled flow:digest record can be
	// checked for equality: a mismatch means the run executed against a DIFFERENT
	// declared topology than the flow now describes.
	want := c.digest()

	var diffs []string
	for _, r := range recs {
		switch {
		case r.Name == flowDigestStep:
			// The reserved topology-digest record Run writes first (see Flow.Run). It is
			// an internal record of the run, not a declared node, so it is never an
			// "unexpected step". It MUST equal the current flow's Digest(); a mismatch is a
			// divergence: the run followed a different declared topology than this flow.
			got, decErr := decodeSwitchChoice(r.Result)
			if decErr != nil {
				diffs = append(diffs, r.Name+" (unreadable topology digest)")
				continue
			}
			if got != want {
				diffs = append(diffs, r.Name+" (ran against a different topology)")
			}

		case strings.HasPrefix(r.Name, "switch:"):
			// A journaled Switch choice, possibly iteration-scoped for a bounded loop:
			// "switch:<over>" for a linear Switch, "switch:iter:<n>:<over>" for one
			// iteration of a loop Switch. Strip the "iter:<n>:" prefix to recover the
			// declared over-node, then validate exactly as for a linear Switch: the
			// over-node must be a declared Switch and the recorded chosen target one of its
			// declared arms (a loop-back arm targets the head, which is a declared arm).
			over := stripIterPrefix(strings.TrimPrefix(r.Name, "switch:"))
			arms, isSwitch := switchArms[over]
			if !isSwitch {
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
			}

		case strings.HasPrefix(r.Name, "attempt:"):
			// An internal attempt marker of a node, possibly iteration-scoped for a bounded
			// loop: "attempt:<N>" or "attempt:iter:<n>:<N>". Strip the "iter:<n>:" prefix to
			// recover the declared node it guards; only an attempt for an UNDECLARED node is
			// a divergence. A present attempt whose result is missing is a halted/in-flight
			// node, not a divergence, so we do not require the "<N>" result to also be present.
			guarded := stripIterPrefix(strings.TrimPrefix(r.Name, "attempt:"))
			if c.byName[guarded] == nil {
				diffs = append(diffs, r.Name+" (attempt for undeclared step)")
			}

		default:
			// A node result record, possibly iteration-scoped for a bounded loop:
			// "<N>" for a linear node, "iter:<n>:<N>" for one iteration of a loop-body
			// node. Strip the "iter:<n>:" prefix to map it back to its declared node.
			if c.byName[stripIterPrefix(r.Name)] == nil {
				diffs = append(diffs, r.Name+" (unexpected step)")
			}
		}
	}

	return len(diffs) == 0, diffs, nil
}

// stripIterPrefix removes a leading iteration scope "iter:<n>:" (where <n> is one or
// more decimal digits) from a journal key, so a bounded-loop node's iteration-scoped
// record maps back to its declared node. A key without a well-formed iteration prefix
// is returned unchanged, so a malformed or non-loop name is still matched (or flagged)
// against the declared topology as before. It mirrors iterKey/attemptMarker in flow.go.
func stripIterPrefix(name string) string {
	rest, ok := strings.CutPrefix(name, "iter:")
	if !ok {
		return name
	}
	digits, node, ok := strings.Cut(rest, ":")
	if !ok || digits == "" {
		return name // not "iter:<n>:<node>"; leave unchanged
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return name // the segment after "iter:" is not a number; not an iteration key
		}
	}
	return node
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
