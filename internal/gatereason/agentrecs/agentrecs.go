// Package agentrecs adapts a run's records to gatereason.Records, for the packages that read a journal
// through the agent package (the approvals intake, the run viewer, the audit CLI). The agent itself, which
// gatereason cannot import the other way, makes the same adapter inline.
package agentrecs

import (
	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/gatereason"
)

// Of yields the value records of recs by name and result.
func Of(recs []agent.Record) gatereason.Records {
	return func(yield func(string, []byte) bool) {
		for _, r := range recs {
			if r.Kind != agent.StepValue {
				continue
			}
			if !yield(r.Name, r.Result) {
				return
			}
		}
	}
}
