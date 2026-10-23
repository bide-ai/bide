package agent

import (
	"fmt"
	"slices"

	"github.com/bide-ai/bide/internal/amount"
)

// The exported names of the cap options are all here, so renaming one is a change to this file and its
// documentation (the names are the user's to confirm; see docs/design/untrusted-content-and-amount-caps.md,
// decision 17).

// WithCaps declares amount-cap rules on a tool: one line per rule, in the grammar of the design note
// (docs/guides/amount-caps.md):
//
//	refund_per_call: refund|refund_large.amount[minor, currency=currency] > 500 USD -> refuse
//
// A rule names the tools it covers (a set), one argument (an object member, or a path of them) and the
// currency member, an upper-case limit in major units, and the action "-> refuse": the call is not run
// and not asked. Every tool a rule names must carry the same line. The lines are checked against the
// tool's input schema when the tool is registered (agent.New, WithTools), and a line that does not
// parse, names a member the schema lacks or that is not an integer, number or string, has a limit
// above 2^53 minor units, gates ("-> gate": not yet supported, it arrives with the escalation gate) or
// is cumulative ("sum ... per day": not yet supported, it needs the cap ledger) is ErrConfig.
//
// Per call, every rule is decided twice: on the arguments the model sent, before the call is claimed,
// and on the arguments the tool will receive after any tool middleware, so a middleware that raises
// an amount is refused. An amount that is negative, absent, not a plain decimal, has more fraction
// digits than its currency allows, or arguments that repeat a member or differ in a member's case, is
// not assessable and gets the rule's action; so does a call whose currency no rule of the argument
// covers (compared byte for byte), with the strictest action among them. No float is used. Plan flows
// do not carry caps (the plan registry refuses a tool that does).
func WithCaps(lines ...string) ToolOption {
	return toolOption(func(c *toolConfig) error {
		if len(lines) == 0 {
			return fmt.Errorf("WithCaps: no rule lines: %w", ErrConfig)
		}
		if _, err := amount.ParseRules(lines...); err != nil {
			return fmt.Errorf("WithCaps: %v: %w", err, ErrConfig)
		}
		c.spec.Caps = slices.Concat(c.spec.Caps, lines)
		return nil
	})
}
