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
	"fmt"
	"sync"

	"github.com/bide-ai/bide/agent"
	gsm "github.com/blackwell-systems/gsm"
)

// Applied is the outcome of applying one governed event.
type Applied struct {
	// State is the shared normal form after the event.
	State gsm.State
	// Position is the event's place in the order State is a replay of: its position in the shared
	// EventLog for a log-backed governor, or in the governor's own sequence for the in-memory
	// Governor. Replaying the events at positions [0, Position] from the initial state through the
	// same policy reproduces State exactly, which is what lets an auditor check a recorded state.
	Position int64
}

// Applier applies a named governed event to shared state. Both the in-memory Governor and the
// log-backed PersistentGovernor satisfy it, so EventTool works with either.
type Applier interface {
	Apply(ctx context.Context, event string) (Applied, error)
	// State returns the governor's current view of the shared state. For a log-backed governor
	// it is as of the last Apply or Sync; other processes' later appends are folded in by the
	// next one.
	State() gsm.State
}

// Compile-time port/adapter contracts: any adapter that drifts from its port won't build.
var (
	_ Applier  = (*Governor)(nil)
	_ Applier  = (*PersistentGovernor)(nil)
	_ EventLog = (*MemEventLog)(nil)
)

// Governor applies agents' events to one in-memory, gsm-governed state. Apply is convergent and
// thread-safe: because the machine was verified at Build time (WFC + CC), any order of the same
// event set lands on the same valid normal form. Its state lives only in this process; use a
// PersistentGovernor over a shared EventLog when several processes act on the same state.
type Governor struct {
	m      *gsm.Machine
	events map[string]bool // the events m declares
	mu     sync.Mutex
	state  gsm.State
	next   int64 // position of the next event in this governor's sequence
}

// New wraps a built, verified gsm.Machine with an initial state.
func New(m *gsm.Machine, initial gsm.State) *Governor {
	return &Governor{m: m, events: eventSet(m), state: initial}
}

// Apply advances the shared state by one event (O(1) table lookup; compensation to a valid normal
// form is baked in). Position is the event's place in this governor's sequence. An event the
// machine does not declare is rejected with an ErrConfig error and Position -1, and the state is
// unchanged.
func (g *Governor) Apply(_ context.Context, event string) (Applied, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.events[event] {
		return Applied{State: g.state, Position: -1}, unknownEvent(g.m, event)
	}
	g.state = g.m.Apply(g.state, event)
	pos := g.next
	g.next++
	return Applied{State: g.state, Position: pos}, nil
}

// State returns the current shared normal-form state.
func (g *Governor) State() gsm.State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// eventSet returns the set of events m declares. gsm.Machine.Apply panics on any other name, so
// every governor checks a name against this set before applying it.
func eventSet(m *gsm.Machine) map[string]bool {
	set := map[string]bool{}
	for _, e := range m.Events() {
		set[e] = true
	}
	return set
}

func unknownEvent(m *gsm.Machine, event string) error {
	return fmt.Errorf("govern: machine %q has no event %q: %w", m.Name(), event, agent.ErrConfig)
}

// EventLog is an append-only, totally ordered log of governed events per entity: the durable
// shared record several processes' governors act on (MemEventLog below; govern/sqlitelog,
// govern/postgreslog, and govern/redislog for durable backends).
//
// Positions are dense and never change. The first event appended for an entity is at position 0,
// and once a reader has seen the events at positions [0, n), every later read returns those same
// events first. Governors rely on this to fold other processes' events into their state in exactly
// the order an auditor will replay them.
type EventLog interface {
	// Append records event at the end of entity's log and returns its position: the number of
	// the entity's events recorded before it. It must be safe to call concurrently from many
	// processes, which must never be assigned the same position.
	Append(ctx context.Context, entity, event string) (int64, error)
	// Events returns entity's events at positions from onward, in log order.
	Events(ctx context.Context, entity string, from int64) ([]string, error)
}

// MemEventLog is an in-memory EventLog for tests/local dev.
type MemEventLog struct {
	mu   sync.Mutex
	logs map[string][]string
}

// NewMemEventLog returns an empty in-memory EventLog.
func NewMemEventLog() *MemEventLog { return &MemEventLog{logs: map[string][]string{}} }

// Append records an event for the entity and returns its position.
func (l *MemEventLog) Append(_ context.Context, entity, event string) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	pos := int64(len(l.logs[entity]))
	l.logs[entity] = append(l.logs[entity], event)
	return pos, nil
}

// Events returns a copy of the entity's events at positions from onward.
func (l *MemEventLog) Events(_ context.Context, entity string, from int64) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	evs := l.logs[entity]
	if from < 0 {
		from = 0
	}
	if from >= int64(len(evs)) {
		return nil, nil
	}
	out := make([]string, len(evs)-int(from))
	copy(out, evs[from:])
	return out, nil
}

// PersistentGovernor is a governor over a shared, durable EventLog. Every event is appended to the
// log, and the governor's state is the log replayed through the machine, so any number of
// processes sharing the log act on the same state: before answering, Apply folds in every event
// the log holds up to and including its own, in log order, including other processes' events.
// The state an Apply returns is therefore exactly what an auditor gets by replaying the shared log
// through that event's position.
type PersistentGovernor struct {
	m      *gsm.Machine
	events map[string]bool // the events m declares
	log    EventLog
	entity string
	mu     sync.Mutex
	state  gsm.State
	next   int64 // position of the next log event to fold into state
}

// NewPersistent constructs a governor over the entity's log and reconstructs its state by replaying
// the log from the initial state. A log holding an event the machine does not declare (written by
// an older policy or a foreign writer) is an ErrProtocol error here and on every later fold.
func NewPersistent(ctx context.Context, m *gsm.Machine, log EventLog, entity string, initial gsm.State) (*PersistentGovernor, error) {
	pg := &PersistentGovernor{m: m, events: eventSet(m), log: log, entity: entity, state: initial}
	if _, err := pg.Sync(ctx); err != nil {
		return nil, err
	}
	return pg, nil
}

// Apply appends the event to the shared log, then folds the log into the state up to and including
// the event's position (so every earlier event, from any process, is applied first, in log order),
// and returns that state and position. Events appended after it by other processes are folded in by
// the next Apply or Sync. If the append succeeds but reading the log fails, the event is still
// recorded; the next Apply or Sync folds it in.
//
// The event is validated before the append: a name the machine does not declare is rejected with
// an ErrConfig error and Position -1, and nothing is written, so no process sharing the log ever
// reads it.
func (pg *PersistentGovernor) Apply(ctx context.Context, event string) (Applied, error) {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if !pg.events[event] {
		return Applied{State: pg.state, Position: -1}, unknownEvent(pg.m, event)
	}
	pos, err := pg.log.Append(ctx, pg.entity, event)
	if err != nil {
		return Applied{State: pg.state, Position: -1}, fmt.Errorf("govern: append log: %w (%w)", err, agent.ErrStorage)
	}
	if err := pg.foldThrough(ctx, pos); err != nil {
		return Applied{State: pg.state, Position: pos}, err
	}
	return Applied{State: pg.state, Position: pos}, nil
}

// Sync folds every event the log currently holds into the state and returns it, so the governor
// reflects other processes' events without applying one of its own.
func (pg *PersistentGovernor) Sync(ctx context.Context) (gsm.State, error) {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	return pg.state, pg.foldThrough(ctx, -1)
}

// foldThrough applies the log's events from pg.next through position last (or to the end of the
// log when last is -1) to the state. An event the machine does not declare stops the fold with an
// ErrProtocol error: the state stays at the last event before it, and the entry is not skipped,
// since skipping it would diverge from what an auditor replaying the log computes. The caller
// holds pg.mu.
func (pg *PersistentGovernor) foldThrough(ctx context.Context, last int64) error {
	if last >= 0 && last < pg.next {
		return nil
	}
	evs, err := pg.log.Events(ctx, pg.entity, pg.next)
	if err != nil {
		return fmt.Errorf("govern: read log: %w (%w)", err, agent.ErrStorage)
	}
	n := int64(len(evs))
	if last >= 0 {
		if want := last - pg.next + 1; n < want {
			return fmt.Errorf("govern: log for %q returned %d events from position %d, want at least %d: %w", pg.entity, n, pg.next, want, agent.ErrStorage)
		} else {
			n = want
		}
	}
	for _, e := range evs[:n] {
		if !pg.events[e] {
			return fmt.Errorf("govern: log for %q holds event %q at position %d, which machine %q does not declare: %w", pg.entity, e, pg.next, pg.m.Name(), agent.ErrProtocol)
		}
		pg.state = pg.m.Apply(pg.state, e)
		pg.next++
	}
	return nil
}

// State returns the governor's view of the shared state as of its last Apply or Sync.
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
			a, err := gov.Apply(ctx, event)
			if err != nil {
				return nil, err
			}
			return map[string]any{"event": event, "applied": true, "position": a.Position}, nil
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
			a, err := gov.Apply(ctx, event)
			if err != nil {
				return nil, err
			}
			// One leaf binds the action (this tool call), the policy that admitted it, the exact
			// resulting state, and the event's position in the governor's order. A verifier
			// replaying the policy over the governed events at positions [0, position] (the shared
			// log for a log-backed governor) reproduces state_digest, so the runtime's state is
			// checkable against the verified reference at every transition, however many processes
			// or runs share the state.
			result := map[string]any{
				"event":         event,
				"applied":       true,
				"policy_digest": policyDigest,
				"state_digest":  a.State.Digest(),
				"position":      a.Position,
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
