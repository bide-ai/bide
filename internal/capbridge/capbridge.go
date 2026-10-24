// Package capbridge lets bide audit caps ask the question only package agent can answer: whether the
// journal of a run records the attempt behind a cap-ledger slot as never started (the attempt marker
// and the not-started record that answers it are agent's private knowledge). Package agent registers
// the function once, in its init; the audit reads it and cannot replace it. The package is internal,
// so no code outside this module can reach it, and it adds nothing to the exported API of bide.
//
// The registered function only reads.
package capbridge

import (
	"context"
	"sync"

	"github.com/bide-ai/bide/internal/capledger"
)

// VoidedFunc reports whether the journal j (an *agent.Journal) records the attempt behind the live slot
// s as never started. It reads and writes nothing.
type VoidedFunc func(ctx context.Context, j any, s capledger.Slot) (bool, error)

var (
	mu     sync.Mutex
	voided VoidedFunc
)

// RegisterVoided sets the function Voided returns. It panics on a nil function and on a second call:
// the bridge is set once by package agent and is never replaced.
func RegisterVoided(f VoidedFunc) {
	mu.Lock()
	defer mu.Unlock()
	if f == nil {
		panic("capbridge: RegisterVoided: nil function")
	}
	if voided != nil {
		panic("capbridge: RegisterVoided: already registered")
	}
	voided = f
}

// Voided returns the registered function, or nil if package agent has not registered one (a program
// that links this package without agent).
func Voided() VoidedFunc {
	mu.Lock()
	defer mu.Unlock()
	return voided
}
