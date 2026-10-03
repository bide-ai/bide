// Package govern is the Tier-2 (distributed / concurrent) compensating-agent layer: it
// integrates the gsm "Governed State Machine" library so that MULTIPLE concurrent agents
// mutating SHARED state converge to the same valid state regardless of interleaving, as
// certified by gsm's build-time normalization-confluence verification (WFC + CC).
//
// This is the fundamentally different tier from the sequential/hierarchical saga in the
// core package: there is no single causal order to reverse, so correctness can't be
// "reverse-the-log." Instead each agent's action is a gsm Event, shared business state is
// gsm's finite-domain state, business rules are Invariants, and compensations are Repairs.
// gsm checks at build time that every interleaving reaches the same normal form.
//
// It lives OUTSIDE the core, in its own module (github.com/bide-ai/bide/govern), so the core
// module never depends on gsm: an edge integration, and the lean hexagonal core is untouched.
package govern

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	// Apply applies event as a new action, once per call.
	Apply(ctx context.Context, event string) (Applied, error)
	// ApplyOnce applies event at most once per id. A later call with the same id applies nothing
	// and returns what the first call applied: the same position, and the state replayed through
	// it. Reusing an id with a different event, or an empty id, is an ErrConfig error. EventTool
	// keys it by the tool call, so a call that runs again (its process died after applying the
	// event but before the call's result was recorded) does not apply its event twice.
	ApplyOnce(ctx context.Context, id, event string) (Applied, error)
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
	next   int64                  // position of the next event in this governor's sequence
	once   map[string]onceApplied // ApplyOnce ids already applied
}

// onceApplied is what an ApplyOnce id applied: its event and the result it returned.
type onceApplied struct {
	event   string
	applied Applied
}

// New wraps a built, verified gsm.Machine with an initial state.
func New(m *gsm.Machine, initial gsm.State) *Governor {
	return &Governor{m: m, events: eventSet(m), state: initial, once: map[string]onceApplied{}}
}

// Apply advances the shared state by one event (O(1) table lookup; compensation to a valid normal
// form is baked in). Position is the event's place in this governor's sequence. An event the
// machine does not declare is rejected with an ErrConfig error and Position -1, and the state is
// unchanged.
func (g *Governor) Apply(_ context.Context, event string) (Applied, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.apply(event)
}

// ApplyOnce is Apply at most once per id (see Applier). The ids live in this governor's memory,
// like its state.
func (g *Governor) ApplyOnce(_ context.Context, id, event string) (Applied, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if id == "" {
		return Applied{State: g.state, Position: -1}, errEmptyID
	}
	if prev, ok := g.once[id]; ok {
		if prev.event != event {
			return Applied{State: g.state, Position: -1}, reusedID(id, prev.event, event)
		}
		return prev.applied, nil
	}
	a, err := g.apply(event)
	if err != nil {
		return a, err
	}
	g.once[id] = onceApplied{event: event, applied: a}
	return a, nil
}

// apply applies one event. The caller holds g.mu.
func (g *Governor) apply(event string) (Applied, error) {
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

var errEmptyID = fmt.Errorf("govern: empty apply id: %w", agent.ErrConfig)

func reusedID(id, first, now string) error {
	return fmt.Errorf("govern: apply id %q was used for event %q, not %q: %w", id, first, now, agent.ErrConfig)
}

// newApplyID returns a fresh random id for an Apply call, so the append it makes is still
// idempotent when a log adapter's transport retries it.
func newApplyID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("govern: generate apply id: %w (%w)", err, agent.ErrStorage)
	}
	return "apply:" + hex.EncodeToString(b[:]), nil
}

// callApplyID is the ApplyOnce id for the next apply the tool call running in ctx makes: the
// call's hierarchical run scope (run ID, then tool call ID) and the apply's number within the call
// (agent.NextOnceKey), which are the same every time that call runs. Every apply of one call gets
// its own id, so a composite tool that applies two events records both. Outside a run it is
// empty, and the tools fall back to Apply.
func callApplyID(ctx context.Context) string {
	if key := agent.NextOnceKey(ctx); key != "" {
		return "tool:" + key
	}
	return ""
}

func applyForCall(ctx context.Context, gov Applier, event string) (Applied, error) {
	if id := callApplyID(ctx); id != "" {
		return gov.ApplyOnce(ctx, id, event)
	}
	return gov.Apply(ctx, event)
}

// EventLog is an append-only, totally ordered log of governed events per entity: the durable
// shared record several processes' governors act on (MemEventLog below; govern/sqlitelog,
// govern/postgreslog, and govern/redislog for durable backends).
//
// Positions are dense and never change. The first event appended for an entity is at position 0,
// and once a reader has seen the events at positions [0, n), every later read returns those same
// events first. Governors rely on this to fold other processes' events into their state in exactly
// the order an auditor will replay them.
//
// Appends are idempotent by id. A governor gives every append an id (a random one for Apply, the
// caller's for ApplyOnce) and may send the same append again: a transport retries a request whose
// reply was lost, or a tool call runs again after its process died. The log records each
// (entity, id) once, so a repeated append cannot record its event twice.
type EventLog interface {
	// Append records event at the end of entity's log under id and returns its position: the
	// number of the entity's events recorded before it. If entity's log already holds an append
	// with this id, Append records nothing and returns that append's position; if that append's
	// event differs from event, or id is empty, it returns an error wrapping agent.ErrConfig. It
	// must be safe to call concurrently from many processes, which must never be assigned the
	// same position, and concurrent appends with the same id must record one event.
	Append(ctx context.Context, entity, id, event string) (int64, error)
	// Events returns entity's events at positions from onward, in log order: the i-th event
	// returned is the one at position from+i. A log that cannot return them so (its positions
	// have a gap, say, because a record was removed outside the adapter) must return an error
	// wrapping agent.ErrProtocol rather than a shorter list.
	Events(ctx context.Context, entity string, from int64) ([]string, error)
}

// MemEventLog is an in-memory EventLog for tests/local dev.
type MemEventLog struct {
	mu   sync.Mutex
	logs map[string][]string
	ids  map[string]map[string]int64 // entity -> append id -> position
}

// NewMemEventLog returns an empty in-memory EventLog.
func NewMemEventLog() *MemEventLog {
	return &MemEventLog{logs: map[string][]string{}, ids: map[string]map[string]int64{}}
}

// Append records an event for the entity under id and returns its position, or returns the
// position already recorded for id.
func (l *MemEventLog) Append(_ context.Context, entity, id, event string) (int64, error) {
	if id == "" {
		return 0, fmt.Errorf("govern: empty append id: %w", agent.ErrConfig)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if pos, ok := l.ids[entity][id]; ok {
		if prev := l.logs[entity][pos]; prev != event {
			return 0, fmt.Errorf("govern: append id %q holds event %q, not %q: %w", id, prev, event, agent.ErrConfig)
		}
		return pos, nil
	}
	pos := int64(len(l.logs[entity]))
	l.logs[entity] = append(l.logs[entity], event)
	if l.ids[entity] == nil {
		l.ids[entity] = map[string]int64{}
	}
	l.ids[entity][id] = pos
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
	m       *gsm.Machine
	events  map[string]bool // the events m declares
	log     EventLog
	entity  string
	initial gsm.State // the state the log is replayed from
	mu      sync.Mutex
	state   gsm.State
	next    int64 // position of the next log event to fold into state
}

// NewPersistent constructs a governor over the entity's log and reconstructs its state by replaying
// the log from the initial state. A log holding an event the machine does not declare (written by
// an older policy or a foreign writer) is an ErrProtocol error here and on every later fold.
func NewPersistent(ctx context.Context, m *gsm.Machine, log EventLog, entity string, initial gsm.State) (*PersistentGovernor, error) {
	pg := &PersistentGovernor{m: m, events: eventSet(m), log: log, entity: entity, initial: initial, state: initial}
	if _, err := pg.Sync(ctx); err != nil {
		return nil, err
	}
	return pg, nil
}

// Apply appends the event to the shared log, then folds the log into the state up to and including
// the event's position (so every earlier event, from any process, is applied first, in log order),
// and returns that state and position. Events appended after it by other processes are folded in by
// the next Apply or Sync. If the append succeeds but reading the log fails, the event is still
// recorded (the error says at which position); the next Apply or Sync folds it in.
//
// The event is validated before the append: a name the machine does not declare is rejected with
// an ErrConfig error and Position -1, and nothing is written, so no process sharing the log ever
// reads it. The append carries a fresh random id, so a log adapter whose transport retries it
// still records it once.
func (pg *PersistentGovernor) Apply(ctx context.Context, event string) (Applied, error) {
	id, err := newApplyID()
	if err != nil {
		return Applied{State: pg.State(), Position: -1}, err
	}
	return pg.ApplyOnce(ctx, id, event)
}

// ApplyOnce is Apply at most once per id (see Applier), across every process sharing the log: the
// id travels with the append, and the log records each id once. A repeated id returns the first
// append's position and the state an auditor gets by replaying the log through it, even if this
// governor has since folded in later events.
func (pg *PersistentGovernor) ApplyOnce(ctx context.Context, id, event string) (Applied, error) {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if !pg.events[event] {
		return Applied{State: pg.state, Position: -1}, unknownEvent(pg.m, event)
	}
	pos, err := pg.log.Append(ctx, pg.entity, id, event)
	if errors.Is(err, agent.ErrConfig) {
		return Applied{State: pg.state, Position: -1}, fmt.Errorf("govern: append log: %w", err)
	}
	if err != nil {
		return Applied{State: pg.state, Position: -1}, fmt.Errorf("govern: append log: %w (%w)", err, agent.ErrStorage)
	}
	if err := checkPosition(pg.entity, pos); err != nil {
		return Applied{State: pg.state, Position: -1}, err
	}
	if pos < pg.next {
		// The log already held this id, and this governor has folded past it: rebuild the state
		// as of its position rather than report the current one.
		st, err := pg.replayThrough(ctx, pos)
		if err != nil {
			return Applied{State: pg.state, Position: pos}, recordedButUnknown(event, pos, err)
		}
		return Applied{State: st, Position: pos}, nil
	}
	if err := pg.foldThrough(ctx, pos); err != nil {
		return Applied{State: pg.state, Position: pos}, recordedButUnknown(event, pos, err)
	}
	return Applied{State: pg.state, Position: pos}, nil
}

// checkPosition refuses a position an EventLog's Append reported for entity that no append can
// have: a negative one. The governor slices the log's events by position, so it must never act on
// one. The event may still have been recorded; the error says the log is broken.
func checkPosition(entity string, pos int64) error {
	if pos < 0 {
		return fmt.Errorf("govern: log for %q reported position %d for an append; a position is at least 0: %w", entity, pos, agent.ErrProtocol)
	}
	return nil
}

// recordedButUnknown reports an append that succeeded when the state after it could not be
// computed, so a caller does not mistake it for an event that was not recorded.
func recordedButUnknown(event string, pos int64, err error) error {
	return fmt.Errorf("govern: event %q is recorded at position %d, but the state after it could not be computed: %w", event, pos, err)
}

// replayThrough replays the log's events at positions [0, last] from the initial state and returns
// the result, without changing the governor's state. The caller holds pg.mu.
func (pg *PersistentGovernor) replayThrough(ctx context.Context, last int64) (gsm.State, error) {
	evs, err := pg.log.Events(ctx, pg.entity, 0)
	if err != nil {
		return gsm.State{}, fmt.Errorf("govern: read log: %w (%w)", err, agent.ErrStorage)
	}
	if int64(len(evs)) <= last {
		return gsm.State{}, fmt.Errorf("govern: log for %q returned %d events, want at least %d: %w", pg.entity, len(evs), last+1, agent.ErrStorage)
	}
	st := pg.initial
	for i, e := range evs[:last+1] {
		if !pg.events[e] {
			return gsm.State{}, pg.undeclared(e, int64(i))
		}
		st = pg.m.Apply(st, e)
	}
	return st, nil
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
			return pg.undeclared(e, pg.next)
		}
		pg.state = pg.m.Apply(pg.state, e)
		pg.next++
	}
	return nil
}

// undeclared reports a log entry the machine does not declare.
func (pg *PersistentGovernor) undeclared(event string, pos int64) error {
	return fmt.Errorf("govern: log for %q holds event %q at position %d, which machine %q does not declare: %w", pg.entity, event, pos, pg.m.Name(), agent.ErrProtocol)
}

// State returns the governor's view of the shared state as of its last Apply or Sync.
func (pg *PersistentGovernor) State() gsm.State {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	return pg.state
}

// EventToolConfig configures EventTool.
type EventToolConfig struct {
	// Name is the tool's name, as the model calls it.
	Name string
	// Description tells the model what the governed action does.
	Description string
	// Event is the governed event the tool applies when called.
	Event string
	// PolicyDigest, when non-empty, makes the tool attested: its durable result records this
	// stable identifier of the governed policy that admitted the action (for example
	// gsm.Registry.PolicyDigest), the resulting state's digest, and the acting identity, so an
	// audit over the journal commits to the policy the action ran under. It is treated as an
	// opaque string: the SDK does not depend on gsm's serialization format.
	PolicyDigest string
	// Attested asks for the attested form explicitly: the result records the state digest and
	// the acting identity as well as the policy digest. A non-empty PolicyDigest makes the tool
	// attested without it; with an empty PolicyDigest, Attested is refused (EventTool panics with
	// ErrConfig), since an attestation needs the policy it attests to. It is what a call of the
	// removed AttestedEventTool, which accepted an empty digest, migrates to.
	Attested bool
	// Safety is the tool's retry classification (see agent.Safety). The zero value is a side
	// effect.
	Safety agent.Safety
	// Options are further tool options (agent.WithApproval, agent.WithTimeout, agent.WithTitle,
	// agent.WithOutputSchema), applied after Safety.
	Options []agent.ToolOption
}

// EventTool gives an agent a GOVERNED action: when the agent's LLM calls it, cfg.Event is
// applied to the shared governed state (convergent + thread-safe). Multiple agents that
// share one governor converge regardless of how their governed calls interleave: the
// tool-boundary bridge from real agent loops to gsm's proven convergence. gsm Guards
// handle preconditions (a guarded-out event is a no-op).
//
// This is the "governor middleware": the point where an agent's tool call becomes a
// verified event on shared state, rather than an unchecked side effect. Works with either
// Governor (in-memory) or PersistentGovernor (crash-recoverable).
//
// Inside a run, the event is applied with ApplyOnce keyed by the tool call and the apply's number
// within it (agent.NextOnceKey), so a call that runs again (a retry-safe tool whose process died
// before its result was recorded) applies its event once and reports the original position, and a
// composite tool that calls EventTool several times in one call applies each of them.
//
// With a non-empty cfg.PolicyDigest the tool is attested: its result also records WHICH governed
// policy admitted the action. Because the result is part of the journaled record, any audit over
// the journal (a signed tree head, or a ProofBundle from ProveToolCall) then commits
// cryptographically to the policy the action ran under, not merely that the action happened. An
// auditor recomputes the digest from the published policy bytes and runs the external verified
// oracle on them (see `bide-audit verify-governance`), tying the cryptographic root (the log) to
// the mathematical root (the proof) over one artifact.
//
// EventTool panics, as agent.Func does, on an invalid option, and with ErrConfig on a config that
// asks for the attested form (Attested) with an empty PolicyDigest.
func EventTool(gov Applier, cfg EventToolConfig) agent.Tool {
	event, policyDigest := cfg.Event, cfg.PolicyDigest
	if cfg.Attested && policyDigest == "" {
		panic(fmt.Errorf("govern: EventTool %q asks for the attested form (state digest and acting identity) with an empty PolicyDigest; set the digest of the policy it attests to: %w", cfg.Name, agent.ErrConfig))
	}
	if policyDigest == "" {
		return agent.MustFunc(cfg.Name, cfg.Description, func(ctx context.Context, _ struct{}) (map[string]any, error) {
			a, err := applyForCall(ctx, gov, event)
			if err != nil {
				return nil, err
			}
			return map[string]any{"event": event, "applied": true, "position": a.Position}, nil
		}, append([]agent.ToolOption{agent.WithSafety(cfg.Safety)}, cfg.Options...)...)
	}
	return agent.MustFunc(cfg.Name, cfg.Description, func(ctx context.Context, _ struct{}) (map[string]any, error) {
		a, err := applyForCall(ctx, gov, event)
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
	}, append([]agent.ToolOption{agent.WithSafety(cfg.Safety)}, cfg.Options...)...)
}
