package govern

import (
	"context"
	"fmt"
	"strings"
	"sync"

	gsm "github.com/blackwell-systems/gsm"
	agent "github.com/dayna/go-agents"
)

// FederatedApplier is the federated analogue of Applier: it applies a named event to a
// named component registry of a gsm federation, returning the new federated normal form.
type FederatedApplier interface {
	Apply(ctx context.Context, registry, event string) (gsm.FedState, error)
	State() gsm.FedState
}

var _ FederatedApplier = (*FederatedGovernor)(nil)

// fedSep separates the registry and event names inside a single log entry. It's the ASCII
// unit separator — legal registry/event names won't contain it, and Apply rejects any that
// do so the encoding stays unambiguous.
const fedSep = "\x1f"

// FederatedGovernor is a crash-recoverable governor over a gsm federation (multiple
// component registries linked by directed morphisms). Each applied event is durably
// appended to an EventLog as a (registry, event) pair; state is reconstructed by replaying
// the log through the federated machine.
//
// Two guarantees carry over from the federated convergence theorem (paper §8):
//   - Replay is exact and, for events the network proved independent, order-independent —
//     so a shared durable log lets multiple processes converge on the same federated state.
//   - Cross-registry conflicts resolve WITHOUT coordination via the authority argument
//     (§8.3): a source registry deterministically fixes its targets' shared components, so
//     a target-side event that collides with its source is overwritten on the next ρ_Fed.
//     This is the "distributed compensating agents across ownership boundaries" tier: each
//     component registry is an organization / agent subtree, morphisms are the cross-org
//     constraints.
type FederatedGovernor struct {
	m      *gsm.FedMachine
	log    EventLog
	entity string
	mu     sync.Mutex
	state  gsm.FedState
}

// NewFederated constructs a federated governor and reconstructs its state by replaying the
// entity's event log from the initial federated state.
func NewFederated(ctx context.Context, m *gsm.FedMachine, log EventLog, entity string, initial gsm.FedState) (*FederatedGovernor, error) {
	fg := &FederatedGovernor{m: m, log: log, entity: entity, state: initial}
	entries, err := log.Events(ctx, entity)
	if err != nil {
		return nil, err
	}
	for _, enc := range entries {
		registry, event, err := decodeFedEvent(enc)
		if err != nil {
			return nil, err
		}
		st, err := m.ApplyNamed(fg.state, registry, event)
		if err != nil {
			return nil, fmt.Errorf("govern: replaying %q: %w (%w)", enc, err, agent.ErrStorage)
		}
		fg.state = st
	}
	return fg, nil
}

// Apply validates the (registry, event) against the federation, durably records it, then
// advances the federated state. Validation runs before the append (via the pure ApplyNamed,
// which does not mutate), so a rejected event is never written to the log; if the append
// fails, the in-memory state is left untouched.
func (fg *FederatedGovernor) Apply(ctx context.Context, registry, event string) (gsm.FedState, error) {
	if strings.Contains(registry, fedSep) || strings.Contains(event, fedSep) {
		return fg.State(), fmt.Errorf("govern: registry/event name may not contain the separator byte: %w", agent.ErrConfig)
	}
	fg.mu.Lock()
	defer fg.mu.Unlock()

	next, err := fg.m.ApplyNamed(fg.state, registry, event)
	if err != nil {
		return fg.state, err
	}
	if err := fg.log.Append(ctx, fg.entity, encodeFedEvent(registry, event)); err != nil {
		return fg.state, fmt.Errorf("govern: append log: %w (%w)", err, agent.ErrStorage)
	}
	fg.state = next
	return fg.state, nil
}

// State returns the current federated normal-form state.
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

// FederatedEventTool gives an agent a GOVERNED federated action: when the agent's LLM calls
// it, `event` is applied to component `registry` of the shared federation. Multiple agents
// (each owning a different registry) converge regardless of interleaving, with cross-registry
// conflicts resolved by the authority argument — no locking. This is the federated twin of
// EventTool: the point where an agent tool call becomes a verified event on shared,
// cross-organizational governed state.
func FederatedEventTool(gov FederatedApplier, name, description, registry, event string, safety agent.Safety) agent.Tool {
	return agent.Func(name, description, safety,
		func(ctx context.Context, _ struct{}) (map[string]any, error) {
			if _, err := gov.Apply(ctx, registry, event); err != nil {
				return nil, err
			}
			return map[string]any{"registry": registry, "event": event, "applied": true}, nil
		})
}
