package plan

import (
	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/planaccess"
)

// inspectRecords is what the typed editor compares before and after an edit (see planaccess.Record):
// the type problems of the config against the Registry or description, the retry safeties above what
// the blocks register, and the loop shapes Load refuses, each with the full names it is about. They are
// found from the config alone, whatever else is wrong with it. registry is a *Registry and description
// a *RegistryDescription (one of them, or neither for the structure alone).
func inspectRecords(data []byte, registry, description any) []planaccess.Record {
	var cfg config
	if err := parseConfig(data, &cfg); err != nil {
		return nil
	}
	var reg *Registry
	switch {
	case registry != nil:
		reg, _ = registry.(*Registry)
	case description != nil:
		if d, ok := description.(*RegistryDescription); ok && d != nil {
			if r, err := registryFromDescription(*d); err == nil {
				reg = r
			}
		}
	}
	var recs []planaccess.Record
	if reg != nil {
		recs = append(recs, checkTypes(&cfg, reg).recs...)
		_, safety := safetyAboveRegistration(&cfg, reg)
		recs = append(recs, safety...)
	}
	recs = append(recs, loopRecords(&cfg)...)
	for i := range recs {
		recs[i].Message = boundMessage(recs[i].Message)
	}
	return recs
}

// loopRecords are the loop and wiring shapes plan.Load refuses, found over the parts of the config
// whose names resolve: the structural check's (a loop with no exit arm, more than one loop-back arm, a
// head that is not an ancestor) and Build's (a loop whose body is not simple, an inner switch, an edge
// that leaves or enters a loop).
func loopRecords(cfg *config) []planaccess.Record {
	var recs []planaccess.Record
	scratch := ConfigReport{Approvals: map[string]agent.ApprovalPolicy{}}
	scratch.Topology = configTopology(cfg)
	scratch.Safety = configSafety(cfg)
	structural(cfg, &scratch, &recs)
	core := graphCore(resolvable(cfg))
	if core == nil {
		return keepLoopRecords(recs)
	}
	if _, err := core.topoOrder(); err != nil {
		return keepLoopRecords(recs)
	}
	recs = append(recs, core.loopProblemsPR().finds...)
	recs = append(recs, core.shapeProblemsPR().finds...)
	return keepLoopRecords(recs)
}

// keepLoopRecords drops the records of the wiring that the editor does not compare.
func keepLoopRecords(recs []planaccess.Record) []planaccess.Record {
	out := recs[:0]
	for _, r := range recs {
		if r.Class == "loop" {
			out = append(out, r)
		}
	}
	return out
}
