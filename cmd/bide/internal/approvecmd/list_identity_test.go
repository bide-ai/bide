package approvecmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/store/sqlite"
)

// markBound writes a phase note that marks the fixture's gate identity-bound, with ops certificate-bound,
// and paused for status. (The gate writes it itself once the identity feature is wired into a gate; the
// engine refuses these names to Journal.Step, so the test writes them to the store.)
func (f *fixture) markBound(t *testing.T) {
	t.Helper()
	s, err := sqlite.Open(f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b, _ := json.Marshal(agent.ApprovalWait{ToolUseID: f.call, ToolName: "refund", Need: 2, Approvers: approvers, IdentityBound: true, IdentityApprovers: []string{"ops"},
		IdentityPolicyDigest: strings.Repeat("ab", 32), Pause: agent.PauseAwaitingStatus, PauseDetail: "no CRL newer than the decision"})
	name := agent.ApprovalPauseStep(f.call, 1)
	data, err := agent.JournalEntry(name, agent.Record{Kind: agent.StepValue, Result: b})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(context.Background(), f.run, name, data); err != nil {
		t.Fatal(err)
	}
}

// -list on a gate with a certificate-bound approver says so, shows the identity block, and does not
// claim a verified tally: the keys it holds cannot check a certificate-bound decision.
func TestList_IdentityBoundGate(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	f.markBound(t)
	code, out, errs := f.run_("", "-list", "-approver-keys", f.keysJSON)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	for _, want := range []string{"identity-bound", "paused: identity status unknown", "no CRL newer than the decision", "policy " + strings.Repeat("ab", 32), "certificate-bound decisions count only when the gate checks them"} {
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
	if got[0].Identity.Class != "wait" {
		t.Fatalf("identity = %+v", got[0].Identity)
	}
	// No bare-key command is offered for the certificate-bound approver.
	if strings.Contains(out, "-as ops ") {
		t.Fatalf("a bare-key command for a certificate-bound approver:\n%s", out)
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
