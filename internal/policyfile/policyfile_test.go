package policyfile

import (
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

func write(t *testing.T, anchor string) string {
	t.Helper()
	root := testca.NewRoot(t, "root", testca.Opts{})
	inter := root.NewIntermediate(t, "inter", testca.Opts{})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "root.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	spki := audit.SPKIHashOf(inter.Cert)
	b, _ := json.Marshal(map[string]any{
		"anchors": []string{anchor}, "intermediates": []string{hex.EncodeToString(spki[:])},
		"identity": map[string]string{"alice": testca.Person(1)}, "id_kind": "person-uri",
		"eku": testca.EKU.String(), "policy_oid": testca.PolicyOID.String(), "canonical": map[string]string{"host": "idp.bank.example"},
	})
	p := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadLoadsAnchorsFromThePolicyDirectory(t *testing.T) {
	ci, err := Read(write(t, "root.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ci.Identity("alice"); !ok {
		t.Fatal("identity map not loaded")
	}
}

func TestReadFailuresAreConfigErrors(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "absent.json")); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("missing policy: %v", err)
	}
	if _, err := Read(write(t, "absent.pem")); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("missing anchor: %v", err)
	}
}
