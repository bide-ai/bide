package govern

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	agent "github.com/blackwell-systems/bide"
	gsm "github.com/blackwell-systems/gsm"
)

// TestAttestedEventTool_RealPolicyDigest wires a real gsm policy end to end: it builds a
// combinator machine, takes its PolicyDigest, governs a real state transition through
// AttestedEventTool, and confirms the journaled result carries that exact digest. It also
// recomputes the digest independently (the same domain-separated formula goagents-audit
// verify-governance uses) and asserts parity with gsm's own PolicyDigest, so the SDK, the
// verifier CLI, and gsm agree on the policy's identity. When GSM_AST_CHECKER is set, it runs
// the external verified oracle on the policy bytes to close the second trust root in-repo.
func TestAttestedEventTool_RealPolicyDigest(t *testing.T) {
	r := gsm.NewRegistry("cap")
	a := r.Int("a", 0, 5)
	b := r.Int("b", 0, 5)
	r.DeclInvariant("a_cap", gsm.Le(gsm.V(a), gsm.Lit(3)), gsm.Do(gsm.Set(a, gsm.Lit(3))))
	r.DeclEvent("inc_a", gsm.Do(gsm.Set(a, gsm.Add(gsm.V(a), gsm.Lit(1)))))
	r.DeclEvent("inc_b", gsm.Do(gsm.Set(b, gsm.Add(gsm.V(b), gsm.Lit(1)))))

	digest, err := r.PolicyDigest()
	if err != nil {
		t.Fatalf("PolicyDigest: %v", err)
	}
	policy, err := r.PolicyBytes()
	if err != nil {
		t.Fatalf("PolicyBytes: %v", err)
	}

	// Independent recomputation: domain-separated SHA-256 over the published format tag and
	// the policy bytes, exactly what goagents-audit verify-governance computes without importing
	// gsm. Parity here means the anchored digest a verifier recomputes will match gsm's.
	h := sha256.New()
	h.Write([]byte(gsm.PolicyFormatVersion + "\n"))
	h.Write(policy)
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		t.Fatalf("digest formula parity broken:\n gsm      %s\n recomputed %s", digest, got)
	}

	m, rep, err := r.Build()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, rep)
	}
	gov := New(m, m.NewState())

	tool := AttestedEventTool(gov, "inc_a", "increment a (capped at 3)", "inc_a", digest, agent.Safety{})
	res, err := tool.Call(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["policy_digest"] != digest {
		t.Fatalf("journaled result did not carry the real policy digest:\n want %s\n got  %v", digest, out["policy_digest"])
	}
	if gov.State().GetInt(a) != 1 {
		t.Fatalf("governed state did not advance: a=%d", gov.State().GetInt(a))
	}
	// The leaf binds the resulting state too: its digest matches the post-apply state.
	if out["state_digest"] != gov.State().Digest() {
		t.Fatalf("journaled state_digest does not match the resulting state:\n want %s\n got  %v", gov.State().Digest(), out["state_digest"])
	}

	// Optional second trust root: run the external verified oracle on the same policy bytes.
	checker := os.Getenv("GSM_AST_CHECKER")
	if checker == "" {
		t.Skip("set GSM_AST_CHECKER to certify the policy converges via the external oracle")
	}
	path := filepath.Join(t.TempDir(), "cap.machine")
	if err := os.WriteFile(path, policy, 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	oracleOut, err := exec.Command(checker, path).CombinedOutput()
	t.Logf("verified oracle: %s", oracleOut)
	if err != nil {
		t.Fatalf("external oracle rejected the policy gsm built as convergent: %v", err)
	}
}
