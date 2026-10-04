package govern

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bide-ai/bide/agent"
	gsm "github.com/blackwell-systems/gsm"
)

// FedApplied is the outcome of applying one federated event: the federated normal form after it,
// and the event's position in the shared EventLog. Replaying the log's events at positions
// [0, Position] from the initial federated state reproduces State.
type FedApplied struct {
	State    gsm.FedState
	Position int64
}

// FederatedApplier is the federated analogue of Applier: it applies a named event to a
// named component registry of a gsm federation.
type FederatedApplier interface {
	// Apply applies event to registry as a new action, once per call.
	Apply(ctx context.Context, registry, event string) (FedApplied, error)
	// ApplyOnce applies event to registry at most once per id, like Applier.ApplyOnce: a later
	// call with the same id applies nothing and returns the first call's position and the state
	// replayed through it.
	ApplyOnce(ctx context.Context, id, registry, event string) (FedApplied, error)
	// State returns the governor's view of the federated state as of its last Apply or Sync.
	State() gsm.FedState
}

var _ FederatedApplier = (*FederatedGovernor)(nil)

// fedSep separates the registry and event names inside a single log entry. It's the ASCII
// unit separator — legal registry/event names won't contain it, and Apply rejects any that
// do so the encoding stays unambiguous.
const fedSep = "\x1f"

// FederatedGovernor is a governor over a gsm federation (multiple component registries linked by
// directed morphisms), backed by a shared, durable EventLog. Each applied event is appended to the
// log as a (registry, event) pair, and the governor's state is the log replayed through the
// federated machine: before answering, Apply folds in every event the log holds up to and including
// its own, in log order, including other processes' events. So any number of processes sharing the
// log act on the same federated state, and a recorded state matches an auditor's replay of the log.
//
// Cross-registry conflicts resolve WITHOUT coordination via the authority argument (paper §8.3): a
// source registry deterministically fixes its targets' shared components, so a target-side event
// that collides with its source is overwritten on the next ρ_Fed. This is the "distributed
// compensating agents across ownership boundaries" tier: each component registry is an organization
// or agent subtree, and morphisms are the cross-org constraints.
type FederatedGovernor struct {
	m       *gsm.FedMachine
	log     EventLog
	entity  string
	initial gsm.FedState // the state the log is replayed from
	mu      sync.Mutex
	state   gsm.FedState
	next    int64 // position of the next log entry to fold into state
}

// NewFederated constructs a federated governor over the entity's log and reconstructs its state by
// replaying the log from the initial federated state. A log holding an entry the federation cannot
// apply is an ErrProtocol error here and on every later fold.
func NewFederated(ctx context.Context, m *gsm.FedMachine, log EventLog, entity string, initial gsm.FedState) (*FederatedGovernor, error) {
	fg := &FederatedGovernor{m: m, log: log, entity: entity, initial: initial, state: initial}
	if _, err := fg.Sync(ctx); err != nil {
		return nil, err
	}
	return fg, nil
}

// Apply validates the (registry, event) against the federation, appends it to the shared log, then
// folds the log into the state up to and including its position. Validation runs before the append
// (via the pure ApplyNamed, which does not mutate), so an unknown registry or event is never written
// to the log. If the append succeeds but reading the log fails, the event is still recorded (the
// error says at which position); the next Apply or Sync folds it in. The append carries a fresh
// random id, so a log adapter whose transport retries it still records it once.
func (fg *FederatedGovernor) Apply(ctx context.Context, registry, event string) (FedApplied, error) {
	id, err := newApplyID()
	if err != nil {
		return FedApplied{State: fg.State(), Position: -1}, err
	}
	return fg.ApplyOnce(ctx, id, registry, event)
}

// ApplyOnce is Apply at most once per id, across every process sharing the log (see
// FederatedApplier). A repeated id returns the first append's position and the federated state an
// auditor gets by replaying the log through it.
func (fg *FederatedGovernor) ApplyOnce(ctx context.Context, id, registry, event string) (FedApplied, error) {
	if strings.Contains(registry, fedSep) || strings.Contains(event, fedSep) {
		return FedApplied{State: fg.State(), Position: -1}, fmt.Errorf("govern: registry/event name may not contain the separator byte: %w", agent.ErrConfig)
	}
	fg.mu.Lock()
	defer fg.mu.Unlock()
	if _, err := fg.m.ApplyNamed(fg.state, registry, event); err != nil {
		return FedApplied{State: fg.state, Position: -1}, fmt.Errorf("govern: %w (%w)", err, agent.ErrConfig)
	}
	pos, err := fg.log.Append(ctx, fg.entity, id, encodeFedEvent(registry, event))
	if errors.Is(err, agent.ErrConfig) {
		return FedApplied{State: fg.state, Position: -1}, fmt.Errorf("govern: append log: %w", err)
	}
	if err != nil {
		return FedApplied{State: fg.state, Position: -1}, fmt.Errorf("govern: append log: %w (%w)", err, agent.ErrStorage)
	}
	if err := checkPosition(fg.entity, pos); err != nil {
		return FedApplied{State: fg.state, Position: -1}, err
	}
	if pos < fg.next {
		// The log already held this id, and this governor has folded past it: rebuild the state
		// as of its position rather than report the current one.
		st, err := fg.replayThrough(ctx, pos)
		if err != nil {
			return FedApplied{State: fg.state, Position: pos}, recordedButUnknown(encodeFedEvent(registry, event), pos, err)
		}
		return FedApplied{State: st, Position: pos}, nil
	}
	if err := fg.foldThrough(ctx, pos); err != nil {
		return FedApplied{State: fg.state, Position: pos}, recordedButUnknown(encodeFedEvent(registry, event), pos, err)
	}
	return FedApplied{State: fg.state, Position: pos}, nil
}

// replayThrough replays the log's entries at positions [0, last] from the initial federated state
// and returns the result, without changing the governor's state. The caller holds fg.mu.
func (fg *FederatedGovernor) replayThrough(ctx context.Context, last int64) (gsm.FedState, error) {
	entries, err := fg.log.Events(ctx, fg.entity, 0)
	if err != nil {
		return gsm.FedState{}, fmt.Errorf("govern: read log: %w (%w)", err, agent.ErrStorage)
	}
	if int64(len(entries)) <= last {
		return gsm.FedState{}, fmt.Errorf("govern: log for %q returned %d entries, want at least %d: %w", fg.entity, len(entries), last+1, agent.ErrStorage)
	}
	st := fg.initial
	for i, enc := range entries[:last+1] {
		if st, err = fg.applyEntry(st, enc, int64(i)); err != nil {
			return gsm.FedState{}, err
		}
	}
	return st, nil
}

// applyEntry applies one encoded log entry, found at position pos, to st.
func (fg *FederatedGovernor) applyEntry(st gsm.FedState, enc string, pos int64) (gsm.FedState, error) {
	registry, event, err := decodeFedEvent(enc)
	if err != nil {
		return st, err
	}
	next, err := fg.m.ApplyNamed(st, registry, event)
	if err != nil {
		return st, fmt.Errorf("govern: log for %q holds entry %q at position %d, which the federation cannot apply: %w (%w)", fg.entity, enc, pos, err, agent.ErrProtocol)
	}
	return next, nil
}

// Sync folds every entry the log currently holds into the state and returns it, so the governor
// reflects other processes' events without applying one of its own.
func (fg *FederatedGovernor) Sync(ctx context.Context) (gsm.FedState, error) {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return fg.state, fg.foldThrough(ctx, -1)
}

// foldThrough applies the log's entries from fg.next through position last (or to the end of the
// log when last is -1) to the state. The caller holds fg.mu.
func (fg *FederatedGovernor) foldThrough(ctx context.Context, last int64) error {
	if last >= 0 && last < fg.next {
		return nil
	}
	entries, err := fg.log.Events(ctx, fg.entity, fg.next)
	if err != nil {
		return fmt.Errorf("govern: read log: %w (%w)", err, agent.ErrStorage)
	}
	n := int64(len(entries))
	if last >= 0 {
		want := last - fg.next + 1
		if n < want {
			return fmt.Errorf("govern: log for %q returned %d entries from position %d, want at least %d: %w", fg.entity, n, fg.next, want, agent.ErrStorage)
		}
		n = want
	}
	for _, enc := range entries[:n] {
		st, err := fg.applyEntry(fg.state, enc, fg.next)
		if err != nil {
			return err
		}
		fg.state = st
		fg.next++
	}
	return nil
}

// State returns the governor's view of the federated state as of its last Apply or Sync.
func (fg *FederatedGovernor) State() gsm.FedState {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return fg.state
}

func encodeFedEvent(registry, event string) string { return registry + fedSep + event }

func decodeFedEvent(enc string) (registry, event string, err error) {
	parts := strings.SplitN(enc, fedSep, 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("govern: malformed federated log entry %q: %w", enc, agent.ErrProtocol)
	}
	return parts[0], parts[1], nil
}

// FederatedEventToolConfig configures FederatedEventTool.
type FederatedEventToolConfig struct {
	// Name is the tool's name, as the model calls it.
	Name string
	// Description tells the model what the governed action does.
	Description string
	// Registry is the federation component the event is applied to.
	Registry string
	// Event is the governed event the tool applies when called.
	Event string
	// Safety is the tool's retry classification (see agent.Safety). The zero value is a side
	// effect.
	Safety agent.Safety
	// Options are further tool options (agent.WithApproval, agent.WithTimeout, agent.WithTitle,
	// agent.WithOutputSchema), applied after Safety.
	Options []agent.ToolOption
}

// FederatedEventTool gives an agent a GOVERNED federated action: when the agent's LLM calls
// it, cfg.Event is applied to component cfg.Registry of the shared federation. Multiple agents
// (each owning a different registry) converge regardless of interleaving, with cross-registry
// conflicts resolved by the authority argument, with no locking. This is the federated twin of
// EventTool: the point where an agent tool call becomes a verified event on shared,
// cross-organizational governed state.
//
// Inside a run, the event is applied with ApplyOnce keyed by the tool call, as for EventTool.
// FederatedEventTool panics, as agent.Func does, on an invalid option.
func FederatedEventTool(gov FederatedApplier, cfg FederatedEventToolConfig) agent.Tool {
	registry, event := cfg.Registry, cfg.Event
	return agent.MustFunc(cfg.Name, cfg.Description, func(ctx context.Context, _ struct{}) (map[string]any, error) {
		a, err := applyFedForCall(ctx, gov, registry, event)
		if err != nil {
			return nil, err
		}
		return map[string]any{"registry": registry, "event": event, "applied": true, "position": a.Position}, nil
	}, append([]agent.ToolOption{agent.WithSafety(cfg.Safety)}, cfg.Options...)...)
}

func applyFedForCall(ctx context.Context, gov FederatedApplier, registry, event string) (FedApplied, error) {
	if id := callApplyID(ctx); id != "" {
		return gov.ApplyOnce(ctx, id, registry, event)
	}
	return gov.Apply(ctx, registry, event)
}
