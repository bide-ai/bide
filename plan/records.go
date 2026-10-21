package plan

import (
	"strings"

	"github.com/bide-ai/bide/agent"
)

// candidateFindings are the findings plan can state about cfg, found from the config and the registered
// types alone, whatever else is wrong with it: the type problems against reg, the retry safeties above
// what the blocks register, and the loop and wiring shapes. reg is nil for the structure alone. A
// candidate becomes a Finding of the report when its message is that of one of the report's problems
// (see attachFindings); some candidates are not, because the report has the same problem under
// another check.
func candidateFindings(cfg *config, reg *Registry) []Finding {
	var recs []Finding
	if reg != nil {
		recs = append(recs, checkTypes(cfg, reg).recs...)
		_, safety := safetyAboveRegistration(cfg, reg)
		recs = append(recs, safety...)
	}
	scratch := ConfigReport{Approvals: map[string]agent.ApprovalPolicy{}}
	scratch.Topology = configTopology(cfg)
	scratch.Safety = configSafety(cfg)
	structural(cfg, &scratch, &recs)
	if core := graphCore(resolvable(cfg)); core != nil {
		if _, err := core.topoOrder(); err == nil {
			recs = append(recs, core.loopProblemsPR().finds...)
			recs = append(recs, core.shapeProblemsPR().finds...)
		}
	}
	return recs
}

// isTypeKind reports whether k is one of the type kinds, which a report lists in full in Findings
// while Problems lists the first hundred.
func isTypeKind(k ProblemKind) bool {
	switch k {
	case ProblemEdgeTypes, ProblemArmTypes, ProblemJoinInputTypes, ProblemTerminalTypes:
		return true
	}
	return false
}

// attachFindings sets r.Findings: one Finding for each error in r.Problems, in order, the specific
// one when a candidate has the problem's message and ProblemOther when none does, then the type
// findings the report's cap left out of Problems. It runs before the messages are shortened for display.
func attachFindings(r *ConfigReport, candidates []Finding) {
	byMessage := map[string][]int{}
	for i, c := range candidates {
		byMessage[c.Message] = append(byMessage[c.Message], i)
	}
	used := make([]bool, len(candidates))
	var out []Finding
	for _, p := range r.Problems {
		if p.Severity != "error" {
			continue
		}
		if is := byMessage[p.Message]; len(is) > 0 {
			out = append(out, candidates[is[0]])
			used[is[0]] = true
			byMessage[p.Message] = is[1:]
			continue
		}
		if isMoreTypeProblems(p.Message) {
			continue // the count line that ends a long list of type problems: no finding of its own
		}
		out = append(out, Finding{Kind: ProblemOther, Message: p.Message})
	}
	// The type findings the cap left out of Problems, in the order they were found.
	for i, c := range candidates {
		if isTypeKind(c.Kind) && !used[i] {
			out = append(out, c)
		}
	}
	r.Findings = out
}

// isMoreTypeProblems reports whether m is the line "and N more type problems (...)" that ends a
// capped list of type problems.
func isMoreTypeProblems(m string) bool {
	return strings.HasPrefix(m, "and ") && strings.Contains(m, " more type problems (")
}
