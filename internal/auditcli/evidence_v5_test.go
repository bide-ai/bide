package auditcli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// verify-evidence still accepts a package sealed before format v6 (format v5, a seal over the whole
// package) and says so; a v6 package carries no such note.
func TestCLI_VerifyEvidenceReadsV5(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	if _, err := journaltest.Do(ctx, j, "r", "call:c1", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"ok":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := audit.Ed25519Signer{Priv: priv}
	pkg, err := audit.Evidence(ctx, j, "r", s, 1)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name string, p audit.EvidencePackage) string {
		b, _ := json.Marshal(p)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	v6 := write("v6.json", pkg)
	pkg.Format, pkg.Signature = audit.EvidenceFormatV5, nil
	b, _ := json.Marshal(pkg)
	pkg.Signature, _ = s.Sign(append([]byte("bide.audit.evidence.v5.seal\x00"), b...))
	v5 := write("v5.json", pkg)
	key := keyText(s)
	_ = pub

	code, out, errOut := runCLI("verify-evidence", "-evidence", v5, "-pubkey", key)
	if code != 0 || !strings.Contains(out, "NOTE  format bide.audit.evidence.v5") {
		t.Fatalf("v5: exit %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut = runCLI("verify-evidence", "-evidence", v6, "-pubkey", key)
	if code != 0 || strings.Contains(out, "NOTE") {
		t.Fatalf("v6: exit %d\n%s\n%s", code, out, errOut)
	}
}
