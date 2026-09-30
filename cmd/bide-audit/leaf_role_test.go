package main

import (
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// Each bundle a verb reads plays one role: a policy leaf, a convergence leaf, a governed action, a
// governed commit. A record proves only what it is, so the verbs check that each bundle is a record
// of its role, not merely one whose result has the right shape. Otherwise any tool whose output an
// attacker controls (a web fetch, say) could journal a "policy leaf" and an "action" that link to a
// policy nobody anchored.

func TestVerifyGovernedAction_PolicyBundleMustBeAPolicyLeaf(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	policy, digest := testPolicy()
	p, _ := json.Marshal(policy)
	action := `{"event":"approve","applied":true,"policy_digest":"` + digest + `"}`
	policyLeaf := `{"digest":"` + digest + `","policy":` + string(p) + `}`
	for name, recs := range map[string][2]agent.Record{
		"policy content in a step not named for it":  {toolLeaf("act", action), valueLeaf("junk", policyLeaf)},
		"policy content in a tool result":            {toolLeaf("act", action), toolLeaf("audit:policy:"+digest, policyLeaf)},
		"policy leaf named for another digest":       {toolLeaf("act", action), valueLeaf("audit:policy:"+digest+"x", policyLeaf)},
		"action payload in a step, not a tool call":  {valueLeaf("act", action), valueLeaf("audit:policy:"+digest, policyLeaf)},
		"action payload in the policy leaf's record": {valueLeaf("audit:policy:x", action), valueLeaf("audit:policy:"+digest, policyLeaf)},
	} {
		paths, pub := leafFiles(t, dir, recs[0], recs[1])
		if code, out := exitCode(t, bin, "verify-governed-action", "-action", paths[0], "-policy-bundle", paths[1], "-pubkey", pub); code == 0 {
			t.Errorf("%s: verify-governed-action exited 0\n%s", name, out)
		}
	}
}

func TestVerifyConvergence_BundlesMustBeTheirLeaves(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	agree := fakeChecker(t, dir, "agree", 0, "compensation_free=false")
	policy, digest := testPolicy()
	p, _ := json.Marshal(policy)
	convLeaf := `{"digest":"` + digest + `","certificate":` + testCert(t, digest) + `}`
	policyLeaf := `{"digest":"` + digest + `","policy":` + string(p) + `}`
	for name, recs := range map[string][2]agent.Record{
		"certificate in a step not named for it": {valueLeaf("junk", convLeaf), valueLeaf("audit:policy:"+digest, policyLeaf)},
		"certificate in a tool result":           {toolLeaf("audit:convergence:"+digest, convLeaf), valueLeaf("audit:policy:"+digest, policyLeaf)},
		"certificate named as a policy leaf":     {valueLeaf("audit:policy:"+digest+"c", convLeaf), valueLeaf("audit:policy:"+digest, policyLeaf)},
		"policy in a step not named for it":      {valueLeaf("audit:convergence:"+digest, convLeaf), valueLeaf("junk", policyLeaf)},
		"policy in a tool result":                {valueLeaf("audit:convergence:"+digest, convLeaf), toolLeaf("audit:policy:"+digest, policyLeaf)},
	} {
		paths, pub := leafFiles(t, dir, recs[0], recs[1])
		for _, checker := range [][]string{nil, {"-checker", agree}} {
			args := append([]string{"verify-convergence", "-cert-bundle", paths[0], "-policy-bundle", paths[1], "-pubkey", pub}, checker...)
			if code, out := exitCode(t, bin, args...); code == 0 {
				t.Errorf("%s (%v): verify-convergence exited 0\n%s", name, checker, out)
			}
		}
	}
}

func TestVerifyQuorum_CommitMustBeAToolResult(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	_, digest := testPolicy()
	tally := `{"decision":"approve","votes_for":2,"total":2,"agreed":true,"votes":[{"voter":"a","decision":"approve"},{"voter":"b","decision":"approve"}]}`
	commit := `{"event":"approve","applied":true,"policy_digest":"` + digest + `"}`
	paths, pub := leafFiles(t, dir, valueLeaf("quorum/q/vote/a", `{"voter":"a","decision":"approve"}`),
		valueLeaf("quorum/q/vote/b", `{"voter":"b","decision":"approve"}`), valueLeaf("quorum/q/tally", tally), valueLeaf("commit", commit))
	if code, out := exitCode(t, bin, "verify-quorum", "-name", "q", "-tally", paths[2], "-vote", paths[0], "-vote", paths[1],
		"-commit", paths[3], "-pubkey", pub, "-k", "2"); code == 0 {
		t.Errorf("a step record as the governed commit: verify-quorum exited 0\n%s", out)
	}
}
