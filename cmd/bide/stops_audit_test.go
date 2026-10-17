//go:build !bide_audit_only

package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/store/sqlite"
)

// bide audit stops reads a real SQLite database: the control run and the stop trail in its event store.
// A deployment's own writes agree; a stop written around the trail is found.
func TestAuditStops_ReadsASQLiteStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "run.db")
	pub, priv, _ := ed25519.GenerateKey(nil)
	pol := agent.StopPolicy{DeploymentID: "prod-91c2", Setters: []string{"ops"}, Lifters: []string{"ops"}, LiftQuorum: 1,
		Verifiers: func(id string) (agent.ApproverVerifier, bool) { return audit.Ed25519Verifier{Pub: pub}, id == "ops" }}
	doc, err := pol.Doc()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(doc)
	policyFile := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policyFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := agent.NewJournal(s)
	signer := audit.Ed25519Signer{Priv: priv}
	ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(pol), agent.WithStopVerifier(audit.StopVerifierFromAudit), agent.WithStopTrail(audit.NewStopTrail(s.EventStore())))
	if err != nil {
		t.Fatal(err)
	}
	e, err := ctl.SetStop(ctx, agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopGlobal}, Reason: "model misbehaving", By: "ops", Signer: signer})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	code, out, errOut := runBide("audit", "stops", "-db", path, "-stop-policy", policyFile, "-unanchored")
	if code != 0 || !strings.Contains(out, "IN FORCE") || !strings.Contains(out, "PASS") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	// A stop written around the trail (the control run holds it, the trail does not).
	s, err = sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	j, _ = agent.NewJournal(s)
	bare, _ := agent.NewStopControl(j, agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(pol), agent.WithStopVerifier(audit.StopVerifierFromAudit))
	if _, err := bare.LiftStop(ctx, e.ID, agent.LiftRequest{By: "ops", Signer: signer}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	code, out, _ = runBide("audit", "stops", "-db", path, "-stop-policy", policyFile, "-unanchored")
	if code != 1 || !strings.Contains(out, "in the control run, not in the trail") {
		t.Fatalf("a lift written around the trail: exit %d\n%s", code, out)
	}
	if code, _, errOut := runBide("audit", "stops", "-db", filepath.Join(dir, "none.db"), "-stop-policy", policyFile, "-unanchored"); code != 4 {
		t.Fatalf("a database that does not exist: exit %d\n%s", code, errOut)
	}
}
