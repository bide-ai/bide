package govern

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/blackwell-systems/bide/agent"
	gsm "github.com/blackwell-systems/gsm"
)

// fakeApplier records applied events and returns a zero state, standing in for a real
// Governor so the attestation test does not need a built machine.
type fakeApplier struct{ applied []string }

func (f *fakeApplier) Apply(_ context.Context, event string) (gsm.State, error) {
	f.applied = append(f.applied, event)
	return gsm.State{}, nil
}
func (f *fakeApplier) State() gsm.State { return gsm.State{} }

// TestAttestedEventTool_EmbedsPolicyDigest confirms the governed tool applies the event and
// records the policy digest in its result, so the journaled record (and any ProofBundle over
// it) commits to which policy admitted the action.
func TestAttestedEventTool_EmbedsPolicyDigest(t *testing.T) {
	fa := &fakeApplier{}
	const digest = "b4c0ffeed00dfeed" // opaque identifier; the SDK does not interpret it

	tool := AttestedEventTool(fa, "ship", "ship the order", "ship", digest, agent.Safety{})
	res, err := tool.Call(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(res, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if m["policy_digest"] != digest {
		t.Fatalf("policy_digest not embedded in result: %v", m)
	}
	if m["event"] != "ship" || m["applied"] != true {
		t.Fatalf("unexpected result payload: %v", m)
	}
	if sd, ok := m["state_digest"].(string); !ok || sd == "" {
		t.Fatalf("state_digest not embedded in result: %v", m)
	}
	if len(fa.applied) != 1 || fa.applied[0] != "ship" {
		t.Fatalf("event not applied exactly once: %v", fa.applied)
	}
}
