package approvecmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/sample"
	"github.com/bide-ai/bide/store/sqlite"
)

// TestSampleJournal: bide approve works on the sample journal, which holds a plan flow node's gate
// (a node key for a call id, its input for arguments) and agent runs waiting at gates, one with
// signed approvals already.
func TestSampleJournal(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "sample.db")
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	res, err := sample.Write(context.Background(), s, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// The sample's approver keys are derived from its public seed; write them as key files.
	keyFile := func(id string) string {
		h := sha256.Sum256([]byte("bide sample v1/key/" + id))
		p := filepath.Join(dir, id+".key")
		if err := os.WriteFile(p, []byte(hex.EncodeToString(h[:])+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	keys := filepath.Join(dir, "keys.json")
	b, _ := json.Marshal(res.ApproverKeys)
	if err := os.WriteFile(keys, b, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(stdin string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(append([]string{"-db", db, "-color", "never"}, args...), strings.NewReader(stdin), &out, &errb, stores)
		return code, out.String(), errb.String()
	}

	code, out, errs := run("", "-list", "-json")
	if code != 0 {
		t.Fatal(errs)
	}
	var waits []waiting
	if err := json.Unmarshal([]byte(out), &waits); err != nil {
		t.Fatal(err)
	}
	var flow *waiting
	for i := range waits {
		if waits[i].Run == "triage-3004" {
			flow = &waits[i]
		}
	}
	if flow == nil {
		t.Fatalf("triage-3004 does not wait: %s", out)
	}
	if !strings.HasPrefix(flow.Call, "node:") || flow.Need != 2 || strings.Join(flow.ApprovedBy, ",") != "finance" {
		t.Fatalf("%+v", flow)
	}
	// Finance already approved this flow node; rejecting it now is refused, with the recorded time (approving again would be "already recorded").
	code, _, errs = run("", "-run", flow.Run, "-call", flow.Call, "-as", "finance", "-key", keyFile("finance"), "-yes", "-reject", "-approver-keys", keys)
	if code != 1 || !strings.Contains(errs, "finance already decided") || !strings.Contains(errs, "approved at 2026-") {
		t.Fatalf("%d %s", code, errs)
	}
	// Ops approves the node's gate; the node's input is what the approver reads.
	code, out, errs = run("", "-run", flow.Run, "-call", flow.Call, "-as", "ops", "-key", keyFile("ops"), "-yes", "-approver-keys", keys)
	if code != 0 {
		t.Fatalf("%d %s\n%s", code, errs, out)
	}
	contains(t, "flow approval", out, "o-3004", "verified tally: 2 of 2 needed approved by finance, ops", "the gate has its approvals")
	// The gate has its approvals but the run was not re-run: it still waits, and the list says what
	// to do instead of offering a command; the agent run at another gate still offers its commands.
	_, out, _ = run("", "-list", "-approver-keys", keys)
	i := strings.Index(out, "run triage-3004")
	if i < 0 || !strings.Contains(out[i:], "approvals verified: re-run the run") || strings.Contains(out[i:], "-run triage-3004") {
		t.Fatalf("-list after: %s", out)
	}
	if !strings.Contains(out, "-run refund-1043 -call c2 -as finance") {
		t.Fatalf("-list after lost the other gate's commands: %s", out)
	}
}
