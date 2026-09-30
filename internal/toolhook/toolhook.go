// Package toolhook holds the hooks by which a tool wrapper in this module (audit's
// AttenuatingSubAgent) takes part in the agent's engine without exported API: the agent checks
// for these interfaces, and only packages of this module can name them.
package toolhook

import "context"

// RollbackBinder is a tool that wraps a sub-agent and runs the sub-run under a context of its
// own (a narrower grant, another identity). A saga rollback that recurses into the sub-run calls
// BindRollback first, so the sub-run's compensations run under the authority the delegation
// granted, never the parent's. subRunID is the sub-run the rollback walks; the wrapper reads what
// that sub-run journaled. An error stops the rollback, which reports the delegation uncompensated.
type RollbackBinder interface {
	BindRollback(ctx context.Context, subRunID string) (context.Context, error)
}
