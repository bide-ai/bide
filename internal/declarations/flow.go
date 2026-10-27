package declarations

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The declarations of a flow (docs/design/flow-taint.md, section 7). A flow's declarations are the
// blocks and the policy of a plan.RegistryDescription of version 2, which the caller converts to a
// FlowDocument. The package reads no description itself: plan imports agent, which imports this package.

// FlowDocument is the declarations of one flow's registered blocks.
type FlowDocument struct {
	// TaintKnown says the description carried the taint declarations (RegistryDescription.TaintDeclared). When
	// it is false nothing below says anything about taint, and an absent declaration is not read as none.
	TaintKnown bool
	// Policy is the taint policy the description was given, nil when none was given.
	Policy *Policy
	// Blocks are the Step, Tool, Model and Join blocks, sorted by name within each namespace (blocks, then merges).
	Blocks []FlowBlock
}

// FlowBlock is one registered block or merge.
type FlowBlock struct {
	Name string
	// Merge is set for a fan-in merge, a namespace of its own: a name may be both a block and a merge.
	Merge bool
	// Effect: the block can have a side effect (its safety is not readonly). Idempotent: it is retry-safe.
	Effect, Idempotent bool
	OwnGate            *Gate
	// Untrusted are the declared sources, sorted.
	Untrusted []string
	Trusted   string
	Exempt    string
}

func (b FlowBlock) subject() string {
	if b.Merge {
		return "merge " + b.Name
	}
	return "block " + b.Name
}

// Findings reads one flow document. A document that does not carry the declarations has one finding that
// says so; otherwise the notes on the policy and the blocks, which are notes, not verdicts.
func (d FlowDocument) Findings() []Finding {
	var out []Finding
	add := func(subject, format string, args ...any) {
		out = append(out, Finding{Subject: subject, Message: fmt.Sprintf(format, args...)})
	}
	if !d.TaintKnown {
		add("declarations", "the description carries no taint declarations (a version 1 description): which blocks declare a source, a trusted output or an exemption, and the taint policy, are not known; regenerate it with this version")
		return out
	}
	if d.Policy == nil {
		effects := 0
		for _, b := range d.Blocks {
			if b.Effect {
				effects++
			}
		}
		if effects > 0 {
			add("policy", "no taint policy was given to this description (a Registry does not hold it; set it with plan.DescribeTaintPolicy), and %d block(s) can have a side effect; if the flow has none, a run that has read untrusted content does not gate them", effects)
		}
		for _, b := range d.Blocks {
			if len(b.Untrusted) > 0 {
				add(b.subject(), "declares untrusted source(s) %s, and no taint policy was given to this description; if the flow has none, marks are recorded and nothing is gated", strings.Join(b.Untrusted, ", "))
			}
		}
	} else {
		policyFindings(add, d.Policy)
		if !d.Policy.RequireProvenance {
			for _, b := range d.Blocks {
				if len(b.Untrusted) == 0 && b.Trusted == "" {
					add(b.subject(), "declares no provenance, and the taint policy does not require one: if its results can carry outside text, declare Untrusted; if they are yours, declare Trusted; then require provenance")
				}
			}
		}
	}
	for _, b := range d.Blocks {
		if b.Exempt != "" {
			add(b.subject(), "is exempt from the escalation gate (%q)", b.Exempt)
		}
	}
	return out
}

// FlowDiff is the changes from old to new, in a fixed order. A pair of documents of which either does not
// carry the declarations cannot be compared for taint: that is one weakened change, so a pipeline stops on
// it, and the other differences are still listed.
func FlowDiff(old, new FlowDocument) []Change {
	var out []Change
	taint := old.TaintKnown && new.TaintKnown
	if !taint {
		out = append(out, Change{Weakened, "declarations", "a description does not carry taint declarations (version 1), so the sources, exemptions and policy cannot be compared; regenerate the baseline with this version", false})
	} else if old.Policy == nil || new.Policy == nil {
		// A nil policy is "not given", not "no policy": nothing says whether the flow gained or lost one, so
		// the policies are not compared (a refuse turned into a gate could hide behind it).
		which := "the old description"
		switch {
		case old.Policy == nil && new.Policy == nil:
			which = "either description"
		case new.Policy == nil:
			which = "the new description"
		}
		out = append(out, Change{Weakened, "policy", "no taint policy was given to " + which + ", so the policies cannot be compared; give both descriptions the flow's policy (plan.DescribeTaintPolicy)", false})
	} else {
		out = append(out, diffPolicies(old.Policy, new.Policy, "block")...)
	}
	key := func(b FlowBlock) string { return b.subject() }
	oldBlocks, newBlocks := map[string]*FlowBlock{}, map[string]*FlowBlock{}
	for i := range old.Blocks {
		oldBlocks[key(old.Blocks[i])] = &old.Blocks[i]
	}
	for i := range new.Blocks {
		newBlocks[key(new.Blocks[i])] = &new.Blocks[i]
	}
	names := map[string]bool{}
	for n := range oldBlocks {
		names[n] = true
	}
	for n := range newBlocks {
		names[n] = true
	}
	for _, n := range slices.Sorted(maps.Keys(names)) {
		out = append(out, diffFlowBlock(n, oldBlocks[n], newBlocks[n], taint)...)
	}
	return out
}

func diffFlowBlock(subject string, old, new *FlowBlock, taint bool) []Change {
	var out []Change
	add := func(c Class, what string, covered bool) { out = append(out, Change{c, subject, what, covered}) }
	switch {
	case old == nil:
		switch {
		case taint && new.Exempt != "":
			add(Weakened, fmt.Sprintf("added with TaintExempt (%q)", new.Exempt), true)
		case taint && len(new.Untrusted) > 0:
			add(Strengthened, "added with a declaration (untrusted source)", true)
		default:
			add(Changed, "added", false)
		}
		return out
	case new == nil:
		switch {
		case taint && len(old.Untrusted) > 0:
			add(Weakened, fmt.Sprintf("removed; it declared untrusted source(s) %s", strings.Join(old.Untrusted, ", ")), true)
		case taint && old.Exempt != "":
			add(Strengthened, "removed; it was TaintExempt", true)
		default:
			add(Changed, "removed", false)
		}
		return out
	}
	if taint {
		for _, s := range old.Untrusted {
			if !slices.Contains(new.Untrusted, s) {
				add(Weakened, fmt.Sprintf("Untrusted(%q) removed", s), true)
			}
		}
		for _, s := range new.Untrusted {
			if !slices.Contains(old.Untrusted, s) {
				add(Strengthened, fmt.Sprintf("Untrusted(%q) added", s), true)
			}
		}
		switch {
		case old.Trusted == "" && new.Trusted != "":
			add(Changed, fmt.Sprintf("a trusted output is declared (%q)", new.Trusted), true)
		case old.Trusted != "" && new.Trusted == "":
			add(Changed, "the trusted output declaration is removed", true)
		case old.Trusted != new.Trusted:
			add(Changed, fmt.Sprintf("the trusted output reason is %q, was %q", new.Trusted, old.Trusted), true)
		}
		switch {
		case old.Exempt == "" && new.Exempt != "":
			add(Weakened, fmt.Sprintf("TaintExempt(%q) added", new.Exempt), true)
		case old.Exempt != "" && new.Exempt == "":
			add(Strengthened, "TaintExempt removed", true)
		case old.Exempt != new.Exempt:
			add(Weakened, fmt.Sprintf("TaintExempt reason changed to %q, was %q", new.Exempt, old.Exempt), true)
		}
	}
	if old.Effect && new.Effect && old.Idempotent != new.Idempotent {
		if new.Idempotent {
			add(Weakened, "now Idempotent: a resume re-runs it instead of halting on an unknown outcome", false)
		} else {
			add(Strengthened, "no longer Idempotent: a resume halts on an unknown outcome instead of re-running it", false)
		}
	}
	if old.Effect != new.Effect {
		if new.Effect {
			add(Strengthened, "now has side effects: taint gates it", false)
		} else {
			add(Weakened, "no longer has side effects (readonly): taint no longer gates it", false)
		}
	}
	out = append(out, diffGate(subject+" own gate", old.OwnGate, new.OwnGate, false)...)
	return out
}
