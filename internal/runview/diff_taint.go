package runview

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/bide-ai/bide/internal/taintwords"
	"github.com/bide-ai/bide/plan"
)

// The taint side of a diff (docs/design/flow-taint.md, section 6): what a change does to the nodes that may
// read untrusted data. It reads plan.ConfigReport.Taint of the two sides, so the editor's baseline, which
// calls BuildDiff, and bide check -diff agree.
//
//   - A source a node declares that the new side no longer declares is weakened: the nodes after it stop being
//     tainted by it. A source added is strengthened.
//   - A side-effecting node that newly reads a source is exposed. When the policy gates or refuses it, or the node
//     is exempt, it is covered and the change is listed ("now reads inbound-email, escalates", "exempt
//     (reason)"). When the policy is known to be absent (plan.InspectTaintPolicy with the zero policy) it is
//     uncovered and the change is weakened ("exposure"). When the policy is not given the diff cannot say, and it
//     is not certified. A node that read a source covered and is no longer covered is judged the same way.
//   - A node's own approval gate is not cover.

// exposureChanges returns the taint changes from o to n and the nodes whose exposure could not be compared.
func exposureChanges(o, n plan.ConfigReport, renRev map[string]string) (cs []Change, uncomparable []string) {
	if len(o.Taint) == 0 || len(n.Taint) == 0 {
		return nil, nil
	}
	sameKnowledge := taintKnown(o) == taintKnown(n)
	for _, x := range n.Topology.Nodes {
		from := x.Name
		if f, ok := renRev[x.Name]; ok {
			from = f
		}
		nt, ok := n.Taint[x.Name]
		if !ok {
			continue
		}
		ot, had := o.Taint[from]
		// A side that does not know its blocks states only what the config's keys say: its sources are not
		// compared with those of a side that knows them (taintBasis says the pair cannot be compared).
		if had && sameKnowledge {
			// A source the node declared is gone, or a new one is declared: the sources are what the nodes
			// after it read.
			for _, s := range ot.Declares {
				if !slices.Contains(nt.Declares, s) {
					cs = append(cs, taintChange(x.Name, "weakened", fmt.Sprintf("no longer declares source %s: the nodes after it stop reading it", s)))
				}
			}
			for _, s := range nt.Declares {
				if !slices.Contains(ot.Declares, s) {
					cs = append(cs, taintChange(x.Name, "strengthened", "now declares source "+s))
				}
			}
		} else if sameKnowledge {
			for _, s := range nt.Declares {
				cs = append(cs, taintChange(x.Name, "strengthened", "now declares source "+s))
			}
		}
		if nt.Verdict == "unknown" || had && ot.Verdict == "unknown" || n.Safety[x.Name] == plan.SafetyReadOnly || len(nt.Sources) == 0 {
			continue
		}
		var oldSources []string
		wasEffect := false
		if had {
			oldSources = ot.Sources
			wasEffect = o.Safety[from] != plan.SafetyReadOnly
		}
		var fresh []string
		for _, s := range nt.Sources {
			if !wasEffect || !slices.Contains(oldSources, s) {
				fresh = append(fresh, s)
			}
		}
		level := coverOf(nt)
		switch {
		case len(fresh) > 0:
			reads := "now reads " + strings.Join(fresh, ", ")
			switch level {
			case covered:
				cs = append(cs, taintChange(x.Name, "", reads+", "+coveredText(nt)))
			case uncovered:
				cs = append(cs, taintChange(x.Name, "weakened", reads+" and "+nt.Reason))
			default:
				uncomparable = append(uncomparable, x.Name)
				cs = append(cs, taintChange(x.Name, "", reads+"; "+nt.Reason))
			}
		case had && wasEffect && level == covered && coverOf(ot) == covered && coveredText(ot) != coveredText(nt):
			// Still covered, but not as it was: a gate turned into an exemption, or a refusal into a gate.
			cs = append(cs, taintChange(x.Name, "", "reads "+strings.Join(nt.Sources, ", ")+", "+coveredText(nt)+", was "+coveredText(ot)))
		case had && wasEffect && coverOf(ot) == covered && level != covered:
			// It read these sources before, covered, and the policy no longer covers it.
			if level == uncovered {
				cs = append(cs, taintChange(x.Name, "weakened", "reads "+strings.Join(nt.Sources, ", ")+" and "+nt.Reason+", was "+coveredText(ot)))
			} else {
				uncomparable = append(uncomparable, x.Name)
				cs = append(cs, taintChange(x.Name, "", "reads "+strings.Join(nt.Sources, ", ")+"; "+nt.Reason+", was "+coveredText(ot)))
			}
		}
	}
	return cs, uncomparable
}

// What the policy does to a node that can have a side effect and reads a source.
const (
	covered       = iota // the policy gates or refuses it, or it is exempt
	uncovered            // the policy is known to be absent: nothing gates it
	notComparable        // the policy is not given
)

// coverOf is the cover of a node that reads a source (its verdict says which; the reasons of plan.NodeTaint
// tell a policy that is known to be absent from one that is not given).
func coverOf(t plan.NodeTaint) int {
	switch {
	case t.Verdict == "escalates" || t.Verdict == "refused" || t.Verdict == "exempt":
		return covered
	case strings.HasPrefix(t.Reason, taintwords.NoPolicy):
		return uncovered
	}
	return notComparable
}

// coveredText says how a covered node is covered, in the words of its verdict.
func coveredText(t plan.NodeTaint) string {
	if t.Verdict == "exempt" {
		return "exempt (" + t.Exempt + ")"
	}
	return t.Verdict
}

// taintChange is a change of node x's taint. gate is "weakened", "strengthened" or "" (listed only); a
// weakened one has the Weakening "exposure".
func taintChange(node, gate, what string) Change {
	c := Change{Change: "changed", What: "node", Node: node, taint: true, Details: []string{"taint: " + what}}
	if gate != "" {
		c.Gate = gate
		c.Details[0] += " (" + gate + ")"
	}
	if gate == "weakened" {
		c.Weakening = "exposure"
		c.summary = node + ": " + what
	}
	return c
}

// uncomparableNote is the not-certified text for exposure that could not be compared.
func uncomparableNote(nodes []string) string {
	return "not certified: taint could not be compared: the flow's taint policy is not given for " + strings.Join(nodes, ", ") + ", which now read untrusted data (check against a Registry or description that gives it)"
}

// taintKnown reports whether r's declarations are known: some node has a verdict other than "unknown".
func taintKnown(r plan.ConfigReport) bool {
	return slices.ContainsFunc(slices.Collect(maps.Values(r.Taint)), func(t plan.NodeTaint) bool { return t.Verdict != "unknown" })
}

// taintStated reports whether r says anything about a source: some node declares one or may read one.
func taintStated(r plan.ConfigReport) bool {
	return slices.ContainsFunc(slices.Collect(maps.Values(r.Taint)), func(t plan.NodeTaint) bool { return len(t.Declares) > 0 || len(t.Sources) > 0 })
}

// taintBasis says, for a diff of two reports that do not both know their taint, what could not be compared.
// note is an informational line (neither side knows: the diff's certification is unchanged); unsure is set when
// one side knows and says something about a source the other cannot show, so the diff cannot be certified.
// knownSide is "old" or "new" for a mixed pair, and sideOnly marks sides that were made structural to be compared
// (a Registry or description for one side only), where the report that knew is passed as stated.
func taintBasis(o, n plan.ConfigReport, sideOnly, stated bool) (note, unsure string) {
	if len(o.Taint) == 0 || len(n.Taint) == 0 {
		return "", ""
	}
	ok, nk := taintKnown(o), taintKnown(n)
	switch {
	case sideOnly:
		if stated {
			return "", "taint could not be compared: the blocks are known on only one side, so what they declare as sources is not compared"
		}
		return "taint not compared (no -blocks on one side)", ""
	case !ok && !nk:
		if slices.ContainsFunc(slices.Collect(maps.Values(n.Taint)), func(t plan.NodeTaint) bool { return strings.HasPrefix(t.Reason, taintwords.DescriptionV1) }) {
			return "taint not compared (" + taintwords.DescriptionV1 + ")", ""
		}
		return "taint not compared (no -blocks)", ""
	case ok != nk:
		known, side := o, "old"
		if nk {
			known, side = n, "new"
		}
		if taintStated(known) {
			return "", "taint could not be compared: the " + side + " side's blocks declare taint and the other side's do not carry it"
		}
	}
	return "", ""
}
