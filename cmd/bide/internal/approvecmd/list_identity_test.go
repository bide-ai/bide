package approvecmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// putIdentity writes the identity record of the fixture's gate, which marks it identity-bound.
func (f *fixture) putIdentity(t *testing.T, id agent.ApprovalIdentity) {
	t.Helper()
	j, closeJ := f.store()
	defer closeJ()
	if _, err := j.Step(context.Background(), f.run, agent.ApprovalIdentityStep(f.call), func(context.Context) (agent.ApprovalIdentity, error) { return id, nil }); err != nil {
		t.Fatal(err)
	}
}

// -list on a gate with a certificate-bound approver says so, shows the identity block, and does not
// claim a verified tally: the keys it holds cannot check a certificate-bound decision.
func TestList_IdentityBoundGate(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	f.putIdentity(t, agent.ApprovalIdentity{Format: agent.ApprovalIdentityFormat, ToolUseID: f.call, PolicyDigest: strings.Repeat("ab", 32), Rules: "v1",
		Decisions: []agent.IdentityDecision{{Approver: "ops", Identifier: "https://idp.bank.example/person/00000000000000000000000000000001", Tier: agent.TierProvisional,
			Basis: "live count: status issued before the decision (provisional only)", StatusTime: "2026-10-15T09:10:00.000Z", AnchoredAt: "2026-10-15T09:01:00.000Z", TimeSource: "log-signer clock"}}})
	code, out, errs := f.run_("", "-list", "-approver-keys", f.keysJSON)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	for _, want := range []string{"identity-bound", "PROVISIONAL", "https://idp.bank.example/person/00000000000000000000000000000001", "live count: status issued before the decision", "status time (S) 2026-10-15 09:10:00.000"} {
		if !strings.Contains(out, want) {
			t.Errorf("-list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "approvals verified") || strings.Contains(strings.ToLower(out), "verified tally") {
		t.Errorf("-list claims a verified count on an identity-bound gate:\n%s", out)
	}
	_, js, _ := f.run_("", "-list", "-json", "-approver-keys", f.keysJSON)
	var got []struct {
		Verified bool `json:"verified"`
		Identity *struct {
			Bound bool   `json:"bound"`
			Class string `json:"class"`
			Cards []struct {
				Tier       string `json:"tier"`
				Identifier string `json:"identifier"`
				Class      string `json:"class"`
			} `json:"cards"`
		} `json:"identity"`
	}
	if err := json.Unmarshal([]byte(js), &got); err != nil || len(got) != 1 || got[0].Identity == nil || !got[0].Identity.Bound || got[0].Verified {
		t.Fatalf("-list -json = %s (%v)", js, err)
	}
	// Not counted yet: the viewer's class is wait, never final; the card is the provisional one.
	if got[0].Identity.Class == "final" || len(got[0].Identity.Cards) != 1 || got[0].Identity.Cards[0].Class != "prov" {
		t.Fatalf("identity = %+v", got[0].Identity)
	}
}

// A gate of bare keys lists exactly as before.
func TestList_BareGateHasNoIdentity(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	_, out, _ := f.run_("", "-list")
	_, js, _ := f.run_("", "-list", "-json")
	if strings.Contains(out, "identity") || strings.Contains(js, `"identity"`) {
		t.Fatalf("identity on a bare-key gate:\n%s\n%s", out, js)
	}
}
