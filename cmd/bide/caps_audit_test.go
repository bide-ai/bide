//go:build !bide_audit_only

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/store/postgres"
	"github.com/bide-ai/bide/store/sqlite"
)

// cappedRuns journals two refunds, each under its own run, into store through an AuditedStore, under
// the cap namespace ns and runs named prefix1, prefix2. It returns the log public key and a file of
// the heads the store anchored (one per line).
func cappedRuns(t *testing.T, store agent.Store, ns, prefix string) (pubkey, headsFile string) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	signer := audit.Ed25519Signer{Priv: priv}
	anchor := audit.NewMemAnchorLog()
	as, err := audit.NewAuditedStore(store, signer, anchor)
	if err != nil {
		t.Fatal(err)
	}
	j, err := agent.NewJournal(as)
	if err != nil {
		t.Fatal(err)
	}
	type in struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	tool, err := agent.Func("refund", "refund", func(context.Context, in) (string, error) { return "ok", nil },
		agent.WithCaps(`refund_total: refund.amount[minor, currency=currency] sum 100 USD per day -> refuse`))
	if err != nil {
		t.Fatal(err)
	}
	for i, amt := range []int{30, 50} {
		a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", fmt.Sprintf(`{"amount":%d,"currency":"USD"}`, amt)), agenttest.TextTurn("done")),
			j, agent.WithTools(tool), agent.WithCapNamespace(ns))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Run(context.Background(), fmt.Sprintf("%s%d", prefix, i+1), agent.UserText("go")); err != nil {
			t.Fatal(err)
		}
	}
	if err := as.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	for _, e := range anchor.Entries() {
		line, _ := json.Marshal(e.STH)
		b.Write(append(line, '\n'))
	}
	headsFile = filepath.Join(t.TempDir(), "heads.jsonl")
	if err := os.WriteFile(headsFile, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return audit.FormatPublicKey(signer.Alg(), pub), headsFile
}

// TestAuditCaps_SQLite: the verb reads a SQLite journal read-only, passes a ledger that is what the
// store signed, and finds a slot deleted from the database afterwards.
func TestAuditCaps_SQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.db")
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pubkey, heads := cappedRuns(t, s, "bank", "r")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	args := []string{"audit", "caps", "-db", path, "-sth", heads, "-pubkey", pubkey}
	code, out, errOut := runBide(args...)
	if code != 0 || !strings.Contains(out, "2 slot(s), 2 live, window total 80 USD") || !strings.Contains(out, "is unchanged since its newest signed head") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM bide_steps WHERE run_id LIKE '@cap/bank/%' AND name = 'slot:1'`); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runBide(args...)
	if code != 1 || !strings.Contains(out, "FAIL  DELETED") || !strings.Contains(out, "FAIL  DIVERGENT_HEAD") {
		t.Fatalf("a deleted slot: exit %d\n%s", code, out)
	}
	if code, out, _ = runBide("audit", "caps", "-db", path, "-unanchored"); code != 1 || !strings.Contains(out, "FAIL  DELETED") {
		t.Fatalf("a deleted slot, unanchored: exit %d\n%s", code, out)
	}
}

// TestAuditCaps_Postgres is the same against Postgres (PG_DSN). The shared test database holds other
// tests' ledgers, so the audit is limited to this test's cap namespace.
func TestAuditCaps_Postgres(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	t.Setenv("BIDE_AUDIT_TEST_DSN", dsn)
	ctx := context.Background()
	s, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stamp := time.Now().Format("150405000000000")
	ns := "t" + stamp
	pubkey, heads := cappedRuns(t, s, ns, "capsrun-"+stamp+"-")
	args := []string{"audit", "caps", "-postgres-env", "BIDE_AUDIT_TEST_DSN", "-namespace", ns, "-sth", heads, "-pubkey", pubkey}
	code, out, errOut := runBide(args...)
	if code != 0 || !strings.Contains(out, "2 slot(s), 2 live, window total 80 USD") || strings.Contains(out+errOut, dsn) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `DELETE FROM bide_steps WHERE run_id LIKE $1 AND name = 'slot:1'`, "@cap/"+ns+"/%"); err != nil {
		t.Fatal(err)
	}
	if code, out, _ = runBide(args...); code != 1 || !strings.Contains(out, "FAIL  DELETED") {
		t.Fatalf("a deleted slot: exit %d\n%s", code, out)
	}
}

// TestAuditCaps_Sample: the sample's README lists the command, and the sample's ledger passes it; a slot
// deleted from the sample's database is found.
func TestAuditCaps_Sample(t *testing.T) {
	dir := sampleDir(t)
	path := filepath.Join(dir, "sample.db")
	code, out, errOut := runBide("audit", "caps", "-db", path, "-unanchored")
	if code != 0 || !strings.Contains(out, "2 slot(s), 2 live, window total 24000 USD") || !strings.HasPrefix(out, "UNANCHORED:") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "README.txt")); err != nil || !strings.Contains(string(b), "bide audit caps -db sample.db -unanchored") {
		t.Fatalf("the sample README does not list the command: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM bide_steps WHERE run_id LIKE '@cap/%' AND name = 'slot:1'`); err != nil {
		t.Fatal(err)
	}
	if code, out, _ = runBide("audit", "caps", "-db", path, "-unanchored"); code != 1 || !strings.Contains(out, "FAIL  DELETED") {
		t.Fatalf("a deleted slot: exit %d\n%s", code, out)
	}
}
