// Package escalation names the subject of the escalation gate instance of a tool call (docs/design/
// untrusted-content-and-amount-caps.md, section 5.3), for the packages of this module that must tell it
// from the call itself: the agent engine, which asks the instance, and the approvals intake, which must
// not decide it under the tool's own gate. It is internal so that no new exported name is needed.
package escalation

import "strings"

// Suffix turns a call's ID into the subject of its escalation instance. A real tool-use ID never carries
// it: a model turn with a '#' in an ID is refused (agent.ErrToolUseIDInvalid).
const Suffix = "#taint"

// ID is the subject of the escalation gate instance of the call.
func ID(call string) string { return call + Suffix }

// Base is the call a gate subject belongs to, and whether the subject is an escalation instance.
func Base(subject string) (string, bool) { return strings.CutSuffix(subject, Suffix) }

// IsSubject reports whether subject is an escalation instance, not a call.
func IsSubject(subject string) bool {
	_, ok := Base(subject)
	return ok
}
