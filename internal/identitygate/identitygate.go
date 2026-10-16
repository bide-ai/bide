// Package identitygate holds the switch that lets an approval gate use certificate-bound approvers.
//
// The pieces of the approver identity feature land in several changes (the counting rule, the gate's
// anchoring and identity records, the re-ask and pre-execution paths). Until the last that makes the gate
// safe to use lands, a policy whose resolver returns a certificate-bound verifier is refused when the gate
// is configured (agent.ErrConfig), so no deployment can reach a half-built gate. That change turned
// Enabled on by default.
package identitygate

import "sync/atomic"

// Enabled reports whether the gate accepts certificate-bound approvers. It is on: the last change of the
// feature (the pre-execution status check on every path to the call) turned it on. Tests of the pieces
// still switch it to check the refusal.
var Enabled atomic.Bool

func init() { Enabled.Store(true) }
