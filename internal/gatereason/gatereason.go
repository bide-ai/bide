// Package gatereason reads the gate-reason record of a call (docs/design/untrusted-content-and-amount-
// caps.md, sections 5.3 and 7) into the reasons an approver, a notifier and a viewer are shown: why the
// call needs a gate beyond its tool's own. A reason is evidence and the application's claim, never
// authority: nothing read here decides a gate, and the signed decision does not bind it. The package
// imports no other bide package that imports the agent, so the agent's notifier payload can carry its
// type.
package gatereason

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bide-ai/bide/internal/amount"
	"github.com/bide-ai/bide/internal/escalation"
	"github.com/bide-ai/bide/internal/printable"
	"github.com/bide-ai/bide/internal/strictjson"
)

// Prefix is the name prefix of a gate-reason record, reserved to the engine.
const Prefix = "gate-reason:"

// recordFormat tags the record (agent.gateReasonFormat).
const recordFormat = "bide.gate-reason.v1"

// Reason kinds.
const (
	// KindTaint: the requesting turn had read untrusted content.
	KindTaint = "taint"
	// KindCap: a cap rule applies.
	KindCap = "cap"
)

// MaxText bounds every host-declared string of a reason, which is shown to people.
const MaxText = 128

// maxReasons bounds how many reasons of one call are shown.
const maxReasons = 64

// Reason is one reason a call is gated, as the record holds it. Every string is a host-declared name (a
// source, a tool and call, a rule), cut to MaxText bytes and escaped when it is not printable text, so it
// can be shown as it is.
type Reason struct {
	// Kind is KindTaint or KindCap.
	Kind string `json:"kind"`
	// Source and Ref (taint): the declared source of the content, and where it was read: "tool:<name>/<call>",
	// "input" or "retrieval". Pos is the position of the mark in the run's journal.
	Source string `json:"source,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Pos    int    `json:"pos,omitempty"`
	// Rule, Action, Why, Currency, Amount and Limit (cap): the rule's name, what it decided ("gate" or
	// "refuse"), why it fired ("over", "not_assessable" or "currency"), and this call's amount and the
	// rule's limit in minor units of Currency. Amount is nil when the call's amount could not be assessed.
	Rule     string `json:"rule,omitempty"`
	Action   string `json:"action,omitempty"`
	Why      string `json:"why,omitempty"`
	Currency string `json:"currency,omitempty"`
	Amount   *int64 `json:"amount,omitempty"`
	Limit    int64  `json:"limit,omitempty"`
	// At is when the engine recorded the reason (the ask), by the run's clock: the figures are as of then.
	At string `json:"at,omitempty"`
}

// wireReason mirrors the engine's reason.
type wireReason struct {
	Kind     string `json:"kind"`
	Source   string `json:"source"`
	Ref      string `json:"ref"`
	Pos      int    `json:"pos"`
	Rule     string `json:"rule"`
	Action   string `json:"action"`
	Why      string `json:"why"`
	Currency string `json:"currency"`
	Amount   *int64 `json:"amount"`
	Limit    int64  `json:"limit"`
}

// wireRecord mirrors the engine's record (agent.gateReasonRecord). It is read as strictly as the engine
// reads it, so a record the engine would refuse shows nothing.
type wireRecord struct {
	Format       string       `json:"format"`
	ToolUseID    string       `json:"tool_use_id"`
	Reasons      []wireReason `json:"reasons"`
	PolicyDigest string       `json:"policy_digest"`
	Gate         any          `json:"gate"`
	Exempt       string       `json:"exempt"`
	RecordedAt   string       `json:"recorded_at"`
}

// Records yields the name and the result bytes of the value records of a run, in journal order.
type Records func(yield func(name string, result []byte) bool)

// Find is the reasons for the gate instance subject: for the escalation instance of a call
// ("<call>#taint"), the taint reasons of the call's record; for the call's own gate, its cap reasons. It
// returns none when the run has no record for the call, or one that does not read.
func Find(subject string, records Records) []Reason {
	call, instance := escalation.Base(subject)
	if !instance {
		call = subject
	}
	var out []Reason
	records(func(name string, result []byte) bool {
		if !strings.HasPrefix(name, Prefix) {
			return true
		}
		var rec wireRecord
		if strictjson.Unmarshal(result, &rec, nil) != nil || rec.Format != recordFormat || rec.ToolUseID != call {
			return true
		}
		for _, r := range rec.Reasons {
			if instance != (r.Kind == KindTaint) || r.Kind != KindTaint && r.Kind != KindCap {
				continue // the escalation instance shows taint, the call's own gate shows caps
			}
			if len(out) == maxReasons {
				break
			}
			out = append(out, Reason{
				Kind: r.Kind, Source: clean(r.Source), Ref: clean(r.Ref), Pos: r.Pos,
				Rule: clean(r.Rule), Action: clean(r.Action), Why: clean(r.Why), Currency: clean(r.Currency),
				Amount: r.Amount, Limit: r.Limit, At: clean(rec.RecordedAt),
			})
		}
		return false // the engine's record is the first that reads
	})
	return out
}

// clean makes a host-declared string safe to show: within MaxText bytes, and, when it is not plain
// printable text, quoted with its non-printing characters escaped.
func clean(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > MaxText {
		cut := MaxText
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	if _, bad := printable.Bad(s); bad || !utf8.ValidString(s) {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// Line is a reason as one line of text for a terminal or a log: "untrusted content from inbound-email (read
// by tool read_email, call c3, journal position 4)", or "cap refund_one: this call 6,000.00 USD over the
// limit 5,000.00 USD (refuse)".
func (r Reason) Line() string {
	switch r.Kind {
	case KindTaint:
		where := r.Ref
		switch {
		case strings.HasPrefix(where, "tool:"):
			name, call, _ := strings.Cut(strings.TrimPrefix(where, "tool:"), "/")
			where = fmt.Sprintf("read by tool %s, call %s", name, call)
		case where == "input":
			where = "the run's input"
		case where == "retrieval":
			where = "retrieved documents"
		}
		return fmt.Sprintf("untrusted content from %s (%s, journal position %d)", r.Source, where, r.Pos)
	case KindCap:
		amt := "an amount that could not be read"
		if r.Amount != nil {
			amt = "this call " + Money(*r.Amount, r.Currency)
		}
		return fmt.Sprintf("cap %s: %s, limit %s (%s, %s)", r.Rule, amt, Money(r.Limit, r.Currency), r.Why, r.Action)
	}
	return r.Kind
}

// Money is minor units in a currency, as the decimal text the figures are shown in, using the ISO 4217
// exponent (internal/amount). A currency it does not know is shown in minor units.
func Money(minor int64, currency string) string {
	exp, err := amount.Exponent(currency)
	if err != nil {
		return fmt.Sprintf("%d minor units of %q", minor, currency)
	}
	sign := ""
	u := uint64(minor)
	if minor < 0 {
		sign, u = "-", -u
	}
	if exp == 0 {
		return fmt.Sprintf("%s%d %s", sign, u, currency)
	}
	p := uint64(1)
	for range exp {
		p *= 10
	}
	return fmt.Sprintf("%s%d.%0*d %s", sign, u/p, exp, u%p, currency)
}
