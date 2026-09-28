// Package govern is the Tier-2 (distributed / concurrent) compensating-agent layer: it
// integrates the gsm "Governed State Machine" library so that MULTIPLE concurrent agents
// mutating SHARED state converge to the same valid state regardless of interleaving —
// PROVABLY, via gsm's build-time normalization-confluence verification (WFC + CC).
//
// This is the fundamentally different tier from the sequential/hierarchical saga in the
// core package: there is no single causal order to reverse, so correctness can't be
// "reverse-the-log." Instead each agent's action is a gsm Event, shared business state is
// gsm's finite-domain state, business rules are Invariants, and compensations are Repairs.
// gsm proves at build time that every interleaving reaches the same normal form.
//
// It lives OUTSIDE the core (which imports no gsm) — an edge integration, so the lean
// hexagonal core is untouched.
package govern

import (
	"context"
	"sync"

	"github.com/bide-ai/bide/agent"
	gsm "github.com/blackwell-systems/gsm"
)

// Applier applies a named governed event to shared state, returning the new normal form.
// Both the in-memory Governor and the crash-recoverable PersistentGovernor satisfy it, so
// EventTool works with either.
type Applier interface {
	Apply(ctx context.Context, event string) (gsm.State, error)
	State() gsm.State
}

// Compile-time port/adapter contracts: any adapter that drifts from its port won't build.
var (
	_ Applier  = (*Governor)(nil)
	_ Applier  = (*PersistentGovernor)(nil)
	_ EventLog = (*MemEventLog)(nil)
)

// Governor applies agents' events to one in-memory, gsm-governed state. Apply is
// convergent and thread-safe: because the machine was verified at Build time (WFC + CC),
// any order of the same event set lands on the same valid normal form.
type Governor struct {
	m     *gsm.Machine
	mu    sync.Mutex
	state gsm.State
}

// New wraps a built, verified gsm.Machine with an initial state.
func New(m *gsm.Machine, initial gsm.State) *Governor {
	return &Governor{m: m, state: initial}
}

// Apply advances the shared state by one event (O(1) table lookup; compensation to a
// valid normal form is baked in). The error is always nil for the in-memory Governor; it
// exists to satisfy Applier alongside PersistentGovernor.
func (g *Governor) Apply(_ context.Context, event string) (gsm.State, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state = g.m.Apply(g.state, event)
	return g.state, nil
}

// State returns the current shared normal-form state.
func (g *Governor) State() gsm.State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// EventLog is an append-only log of governed events per entity. Order is preserved on
// append but is NOT required for correctness — gsm convergence guarantees any replay order
// of the same event set reaches the same state. Implementations: MemEventLog (below),
// and any durable backend (SQLite/Postgres) with the same two methods.
type EventLog interface {
	Append(ctx context.Context, entity, event string) error
	Events(ctx context.Context, entity string) ([]string, error)
}

// MemEventLog is an in-memory EventLog for tests/local dev.
type MemEventLog struct {
	mu   sync.Mutex
	logs map[string][]string
}

// NewMemEventLog returns an empty in-memory EventLog.
func NewMemEventLog() *MemEventLog { return &MemEventLog{logs: map[string][]string{}} }

// Append records an event for the entity, preserving append order.
func (l *MemEventLog) Append(_ context.Context, entity, event string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logs[entity] = append(l.logs[entity], event)
	return nil
}

// Events returns a copy of the entity's events in append order.
func (l *MemEventLog) Events(_ context.Context, entity string) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.logs[entity]))
	copy(out, l.logs[entity])
	return out, nil
}

// PersistentGovernor is a crash-recoverable Governor: every applied event is durably
// appended to an EventLog, and the current state is reconstructed by REPLAYING the log
// through the machine on startup. Because gsm.Apply is convergent, replay is exact — and
// (for the events gsm proved independent) order-independent. Back it with a shared durable
// EventLog and multiple processes converge on the same governed state.
type PersistentGovernor struct {
	m       *gsm.Machine
	log     EventLog
	entity  string
	initial gsm.State
	mu      sync.Mutex
	state   gsm.State
}

// NewPersistent constructs a governor and reconstructs its state by replaying the entity's
// event log from the initial state.
func NewPersistent(ctx context.Context, m *gsm.Machine, log EventLog, entity string, initial gsm.State) (*PersistentGovernor, error) {
	pg := &PersistentGovernor{m: m, log: log, entity: entity, initial: initial, state: initial}
	events, err := log.Events(ctx, entity)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		pg.state = m.Apply(pg.state, e) // replay-to-reconstruct
	}
	return pg, nil
}

// Apply durably records the event, then advances the shared state. On a later restart,
// NewPersistent replays the log to the same state.
func (pg *PersistentGovernor) Apply(ctx context.Context, event string) (gsm.State, error) {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if err := pg.log.Append(ctx, pg.entity, event); err != nil {
		return pg.state, err
	}
	pg.state = pg.m.Apply(pg.state, event)
	return pg.state, nil
}

// State returns the current shared normal-form state.
func (pg *PersistentGovernor) State() gsm.State {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	return pg.state
}

// EventTool gives an agent a GOVERNED action: when the agent's LLM calls it, `event` is
// applied to the shared governed state (convergent + thread-safe). Multiple agents that
// share one governor converge regardless of how their governed calls interleave — the
// tool-boundary bridge from real agent loops to gsm's proven convergence. gsm Guards
// handle preconditions (a guarded-out event is a no-op).
//
// This is the "governor middleware": the point where an agent's tool call becomes a
// verified event on shared state, rather than an unchecked side effect. Works with either
// Governor (in-memory) or PersistentGovernor (crash-recoverable).
func EventTool(gov Applier, name, description, event string, safety agent.Safety) agent.Tool {
	return agent.Func(name, description, safety,
		func(ctx context.Context, _ struct{}) (map[string]any, error) {
			if _, err := gov.Apply(ctx, event); err != nil {
				return nil, err
			}
			return map[string]any{"event": event, "applied": true}, nil
		})
}

// AttestedEventTool is EventTool that also records, in the tool's durable result, WHICH
// governed policy admitted the action: it embeds policyDigest (a stable identifier of the
// combinator policy, e.g. gsm.Registry.PolicyDigest) alongside the event. Because the result
// is part of the journaled record, any audit over the journal (a signed tree head, or a
// ProofBundle from ProveToolCall) then commits cryptographically to the policy the action ran
// under, not merely that the action happened. An auditor recomputes the digest from the
// published policy bytes and runs the external verified oracle on them (see
// `bide-audit verify-governance`), tying the cryptographic root (the log) to the
// mathematical root (the proof) over one artifact.
//
// policyDigest is treated as an opaque string on purpose: the SDK does not depend on gsm's
// serialization format, it only records the identifier the policy's owner published.
func AttestedEventTool(gov Applier, name, description, event, policyDigest string, safety agent.Safety) agent.Tool {
	return agent.Func(name, description, safety,
		func(ctx context.Context, _ struct{}) (map[string]any, error) {
			st, err := gov.Apply(ctx, event)
			if err != nil {
				return nil, err
			}
			// One leaf binds the action (this tool call), the policy that admitted it, and the
			// exact resulting state. A verifier replaying the policy over the run's governed
			// events reproduces each state_digest, so the runtime's state is checkable against
			// the verified reference at every transition.
			result := map[string]any{
				"event":         event,
				"applied":       true,
				"policy_digest": policyDigest,
				"state_digest":  st.Digest(),
			}
			// If the deployment bound an acting identity to the run (agent.WithIdentity), stamp it
			// into the same leaf, so an inclusion proof commits to WHO acted, on whose behalf, and
			// under what authority, not merely that the action happened under the policy.
			if id, ok := agent.IdentityFrom(ctx); ok && !id.Empty() {
				if id.Actor != "" {
					result["actor"] = id.Actor
				}
				if id.OnBehalfOf != "" {
					result["on_behalf_of"] = id.OnBehalfOf
				}
				if id.AuthorityRef != "" {
					result["authority_ref"] = id.AuthorityRef
				}
			}
			return result, nil
		})
}
