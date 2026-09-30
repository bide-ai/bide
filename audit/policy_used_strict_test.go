package audit

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// The used-policy set reads a governed action's policy digest by the same rule bide-audit
// verify-governed-action does: the exact name "policy_digest" in a result that decodes strictly.
// With a looser reading, one action would be said to run under one policy by the CLI and under
// another by the used set, a run certificate and an absence proof.
func TestPoliciesUsed_ReadsTheDigestAsTheCLIDoes(t *testing.T) {
	for in, want := range map[string][]string{
		`{"policy_digest":"A","actor":"a","extra":{"x":[1]}}`: {"A"},
		`{"policy_digest":"A","POLICY_DIGEST":"B"}`:           {"A"},
		`{"POLICY_DIGEST":"B","policy_digest":"A"}`:           {"A"},
		`{"Policy_Digest":"A"}`:                               {},
		`{"policy_digest":"A","policy_digest":"B"}`:           {},
		`{"policy_digest":"A","x":{"k":1,"k":2}}`:             {},
		`{"policy_digest":"\udc00"}`:                          {},
		`{"policy_digest":7}`:                                 {},
	} {
		rec := agent.Record{Kind: agent.StepToolResult, ToolUseID: "t", Result: json.RawMessage(in)}
		if got := PoliciesUsed([]agent.Record{rec}); !reflect.DeepEqual(got, want) {
			t.Errorf("PoliciesUsed(%s) = %q, want %q", in, got, want)
		}
	}
}
