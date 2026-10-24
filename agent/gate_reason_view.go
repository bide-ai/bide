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

// GateReason is one reason a call needs a gate beyond its tool's own, as the gate-reason record the engine
// wrote holds it: for the escalation gate of a tainted call, a source of untrusted content its turn had read
// and where (Kind "taint": Source, Ref "tool:<name>/<call>", "input" or "retrieval", and Pos, the position of
// the mark in the run's journal); for a gate a cap rule holds, the rule, what it decided and why it fired, and
// this call's amount and the rule's limit in minor units of Currency (Kind "cap"). At is when it was recorded,
// by the run's clock: the figures are as of then. Every string is a name the host declared, cut to 128 bytes and
// escaped when it is not printable text. It is the application's record, evidence for an approver; no gate reads
// it and the signed decision does not bind it. Line is the reason as one line of text. It is experimental.
type GateReason = gatereason.Reason
