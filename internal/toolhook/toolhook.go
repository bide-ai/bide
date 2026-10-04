// Package toolhook holds the hooks by which a tool wrapper in this module (audit's
// AttenuatingSubAgent) takes part in the agent's engine without exported API: the agent checks
// for these interfaces, and only packages of this module can name them.
package toolhook

import "context"

// protocol:delegation begin RbBind DClass DGuard

// RollbackBinder is a tool that wraps a sub-agent and runs the sub-run under a context of its
// own (a narrower grant, another identity). A saga rollback that recurses into the sub-run calls
// BindRollback first, so the sub-run's compensations run under the authority the delegation
// granted, never the parent's. subRunID is the sub-run the rollback walks; the wrapper reads what
// that sub-run journaled. An error stops the rollback, which reports the delegation uncompensated.
type RollbackBinder interface {
	BindRollback(ctx context.Context, subRunID string) (context.Context, error)
}

// CheckTool refuses a tool the agent's New would refuse for how it wraps another (see
// agent.checkWrapper): a Compensator on its Unwrap chain, a timeout or another Safety over a
// sub-agent, an embedded tool whose approval gate or timeout its own spec hides, or a method of
// the old Tool method set that disagrees with the tool's spec (agent.checkOldMethods). The agent
// package sets it in init; plan calls it so a flow refuses what an agent refuses.
var CheckTool func(t any) error

// WithIdentity binds the acting identity (actor, on behalf of whom, under what authority) to ctx
// for the sub-run a tool wrapper starts, as a run's WithIdentity option binds a run's: the
// delegation's sub-run acts as the delegate, on behalf of the parent, under the child grant. The
// agent package sets it in init.
var WithIdentity func(ctx context.Context, actor, onBehalfOf, authorityRef string) context.Context

// protocol:toolcall begin WrongAuth

// Unrecorded is an error a tool wrapper of this module returns for a call that must leave nothing
// in the journal: not a result, not a saga failure. The run stops with Err, and a re-drive calls
// the tool again. AttenuatingSubAgent uses it when a delegation is resumed under authority other
// than it began with, or the bound grant it would mint from has expired, or it would mint a grant
// onto a sub-run that journaled no authority: the operator can bind the right grant and drive
// again, and the delegation continues rather than being failed for good. It also uses it when the
// store fails to read or write the delegation's authority, so a resume retries. (A journaled grant
// that has expired is a recorded failure, not this.) It is only for a tool
// whose call is safe to make again (the agent refuses nothing else for it; a side effect with an
// attempt marker halts on the re-drive, as any call that recorded nothing does).
type Unrecorded struct{ Err error }

// Error returns Err's text.
func (e *Unrecorded) Error() string { return e.Err.Error() }

// Unwrap returns Err, so errors.Is sees its category.
func (e *Unrecorded) Unwrap() error { return e.Err }

// protocol:toolcall end

// protocol:toolcall begin IEnter

// CallGuard, when set, is asked before every tool call reaches its tool, with the call's context:
// by the agent's base handler, where an error refuses the call, recorded as a known failure, and
// by a plan Tool node, where it fails the node; either way the tool is never called. The audit
// package sets it (in init) to refuse a call made under a bound grant that has expired, so a
// delegation cannot act past its grant's NotAfterUnix, however long its sub-run runs.
var CallGuard func(ctx context.Context) error

// protocol:toolcall end

// protocol:delegation end
