package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/fakekms"
	"github.com/bide-ai/bide/store/sqlite"
)

// bide-hsm stop signs the kill switch's records with a key in a KMS: the same command as bide stop, with -key
// naming the key by reference. The signature is checked by the fold that reads the control run.
func TestStopSignsWithAKMSKey(t *testing.T) {
	kms := fakekms.New()
	kms.AddEd25519(kmsEd)
	ref := "awskms:" + kmsEd
	entry := enrol(t, kms, "ops", ref)
	v, err := audit.ParsePublicKey(entry)
	if err != nil {
		t.Fatal(err)
	}
	alg, pub := v.Alg(), v.(interface{ PublicKey() []byte }).PublicKey()
	dir := t.TempDir()
	db := filepath.Join(dir, "j.db")
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	doc := agent.StopPolicyDoc{DeploymentID: "prod-91c2", Approvers: map[string]agent.StopApprover{"ops": {Alg: alg, Key: hex.EncodeToString(pub)}}, Setters: []string{"ops"}, Lifters: []string{"ops"}, LiftQuorum: 1}
	b, _ := json.Marshal(doc)
	policy := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policy, b, 0o600); err != nil {
		t.Fatal(err)
	}
	r := hsm(t, kms, "", "stop", "-db", db, "-tool", "refund", "-reason", "provider incident", "-stop-policy", policy, "-as", "ops", "-key", ref)
	if r.code != 0 || !strings.Contains(r.out, "in force") {
		t.Fatalf("stop: %d\n%s", r.code, r.all())
	}
	state := func() agent.StopState {
		s, err := sqlite.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		j, _ := agent.NewJournal(s)
		pol, err := audit.StopPolicyFromDoc(doc)
		if err != nil {
			t.Fatal(err)
		}
		ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(pol), agent.WithStopVerifier(audit.StopVerifierFromAudit))
		if err != nil {
			t.Fatal(err)
		}
		st, err := ctl.State(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	st := state()
	if len(st.Active()) != 1 || !st.Active()[0].Signed || st.Active()[0].By != "ops" || st.RejectedTotal != 0 {
		t.Fatalf("the KMS-signed stop is not in force: %+v", st)
	}
	r = hsm(t, kms, "", "stop", "-db", db, "-lift", st.Active()[0].ID, "-reason", "recovered", "-stop-policy", policy, "-as", "ops", "-key", ref, "-yes")
	if r.code != 0 || len(state().Active()) != 0 {
		t.Fatalf("lift: %d\n%s", r.code, r.all())
	}
	// The messages name this command, and a stray key reference is not echoed back.
	r = hsm(t, kms, "", "stop", "-db", db, "-all", "-reason", "r", "awskms:arn:aws:kms:us-east-1:1:key/secret-looking")
	if r.code != 2 || strings.Contains(r.all(), "secret-looking") || !strings.Contains(r.all(), "bide-hsm stop") {
		t.Fatalf("stray argument: %d\n%s", r.code, r.all())
	}
	r = hsm(t, kms, "", "stop", "-db", db)
	if r.code != 2 || !strings.Contains(r.errOut, "bide-hsm stop: no scope given") {
		t.Fatalf("bare: %d\n%s", r.code, r.all())
	}
}
