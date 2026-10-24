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
// wrote holds it. It is the application's record, evidence for an approver; no gate reads it and the signed
// decision does not bind it. Every string is a name the host declared, cut to 128 bytes and escaped when it is
// not printable text. It is experimental.
type GateReason struct {
	// Kind is "taint" (the requesting turn had read untrusted content), "cap" (a cap rule applies), or "conflict"
	// (two records of the run name the call and disagree: no reason is shown, and the journal should be looked at).
	Kind string `json:"kind"`
	// Source, for a taint reason, is the declared source of the content the turn read.
	Source string `json:"source,omitempty"`
	// Ref, for a taint reason, is where it was read: "tool:<name>/<call>", "input" or "retrieval".
	Ref string `json:"ref,omitempty"`
	// Pos, for a taint reason, is the position of the mark in the run's journal.
	Pos int `json:"pos,omitempty"`
	// Rule, for a cap reason, is the name of the cap rule.
	Rule string `json:"rule,omitempty"`
	// Action, for a cap reason, is what the rule decided: "gate" or "refuse".
	Action string `json:"action,omitempty"`
	// Why, for a cap reason, is why the rule fired: "over", "not_assessable" or "currency".
	Why string `json:"why,omitempty"`
	// Currency, for a cap reason, is the currency of Amount and Limit.
	Currency string `json:"currency,omitempty"`
	// Amount, for a cap reason, is this call's amount in minor units of Currency; nil when it could not be assessed.
	Amount *int64 `json:"amount,omitempty"`
	// Limit, for a cap reason, is the rule's limit in minor units of Currency.
	Limit int64 `json:"limit,omitempty"`
	// At is when the engine recorded the reason (the ask), by the run's clock: the figures are as of then.
	At string `json:"at,omitempty"`
}

// Line is the reason as one line of text, for a terminal or a log: "untrusted content from inbound-email (read
// by tool read_email, call c3, journal position 4)", or "cap refund_one: this call 6000.00 USD, limit 5000.00 USD
// (over, gate)".
func (r GateReason) Line() string { return gatereason.Reason(r).Line() }

// gateReasons converts the reader's reasons (internal/gatereason, which cannot import this package) to
// GateReason. The conversion compiles only while the two structs have the same fields.
func gateReasons(rs []gatereason.Reason) []GateReason {
	if len(rs) == 0 {
		return nil
	}
	out := make([]GateReason, len(rs))
	for i, r := range rs {
		out[i] = GateReason(r)
	}
	return out
}
