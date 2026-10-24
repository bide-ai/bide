package agent

import "github.com/bide-ai/bide/internal/gatereason"

// valueRecords yields the value records of recs by name and result, for the gate-reason reader.
func valueRecords(recs []Record) gatereason.Records {
	return func(yield func(string, []byte) bool) {
		for _, r := range recs {
			if r.Kind != StepValue {
				continue
			}
			if !yield(r.Name, r.Result) {
				return
			}
		}
	}
}
