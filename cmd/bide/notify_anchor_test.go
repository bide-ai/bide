//go:build !bide_audit_only

package main

import (
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
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/store/postgres"
	"github.com/bide-ai/bide/store/sqlite"
)

// anchoredGate drives run to a gate whose notifier writes its trail through an
// audit.AuditedEventStore over es, and returns the pubkey argument and the files of the heads the
// anchor log received, in order.
func anchoredGate(t *testing.T, s agent.Store, es audit.EventStore, run string, anchor *audit.MemAnchorLog) (pubkey string, heads []string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer := audit.Ed25519Signer{Priv: priv}
	ae, err := audit.NewAuditedEventStore(es, signer, anchor)
	if err != nil {
		t.Fatal(err)
	}
	ae.OnError(func(trail string, err error) { t.Errorf("anchoring %s: %v", trail, err) })
	gateRunOn(t, s, ae, run, true)
	dir := t.TempDir()
	for i, e := range anchor.Entries() {
		if e.RunID != agent.NotifyTrailName(run) {
			continue
		}
		p := filepath.Join(dir, fmt.Sprintf("head-%d.json", i))
		b, err := json.Marshal(e.STH)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		heads = append(heads, p)
	}
	if len(heads) == 0 {
		t.Fatal("the notifier anchored nothing")
	}
	return audit.FormatPublicKey(signer.Alg(), signer.PublicKey()), heads
}

func shArgs(base []string, pubkey string, heads []string) []string {
	args := append(base, "-pubkey", pubkey)
	for _, h := range heads {
		args = append(args, "-sth", h)
	}
	return args
}

// A notifier that anchors its own trail needs no hand-signed head: bide audit notifications reads
// the store and the heads from the anchor log and passes without -unanchored, and then finds a
// record deleted or replaced after it was anchored.
func TestAuditNotifications_AutoAnchoredTrailOnSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.db")
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pubkey, heads := anchoredGate(t, s, s.EventStore(), "told", audit.NewMemAnchorLog())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	args := shArgs([]string{"audit", "notifications", "-db", path}, pubkey, heads)

	code, out, errOut := runBide(args...)
	if code != 0 || !strings.Contains(out, "PASS  acked") || !strings.Contains(out, "that all verify") || strings.Contains(out, "UNANCHORED") || strings.Contains(out, "not attested") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// A record replaced by another valid leaf of the same trail.
	exec(`UPDATE bide_events SET leaf = (SELECT leaf FROM bide_events WHERE run_id = 'notify:told' AND seq = 0) WHERE run_id = 'notify:told' AND seq = 1`)
	if code, out, _ := runBide(args...); code != 1 || !strings.Contains(out, "FAIL  trail notify:told: signed head of size") {
		t.Fatalf("a replaced record: exit %d\n%s", code, out)
	}
	// And the newest record deleted outright.
	exec(`DELETE FROM bide_events WHERE run_id = 'notify:told' AND seq = (SELECT MAX(seq) FROM bide_events WHERE run_id = 'notify:told')`)
	if code, out, _ := runBide(args...); code != 1 || !strings.Contains(out, "records were deleted") {
		t.Fatalf("a deleted record: exit %d\n%s", code, out)
	}
}

// The same against Postgres (PG_DSN).
func TestAuditNotifications_AutoAnchoredTrailOnPostgres(t *testing.T) {
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
	run := "anch-told-" + time.Now().Format("150405.000000000")
	pubkey, heads := anchoredGate(t, s, s.EventStore(), run, audit.NewMemAnchorLog())
	late := time.Now().Add(time.Hour).Format(time.RFC3339)
	args := shArgs([]string{"audit", "notifications", "-postgres-env", "BIDE_AUDIT_TEST_DSN", "-run", run, "-as-of", late}, pubkey, heads)

	code, out, errOut := runBide(args...)
	if code != 0 || !strings.Contains(out, "PASS  acked") || !strings.Contains(out, "that all verify") || strings.Contains(out+errOut, dsn) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	trail := agent.NotifyTrailName(run)
	if _, err := db.ExecContext(ctx, `UPDATE bide_events SET leaf = (SELECT leaf FROM bide_events WHERE run_id = $1 AND seq = 0) WHERE run_id = $1 AND seq = 1`, trail); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runBide(args...); code != 1 || !strings.Contains(out, "FAIL  trail "+trail+": signed head of size") {
		t.Fatalf("a replaced record: exit %d\n%s", code, out)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM bide_events WHERE run_id = $1 AND seq = (SELECT MAX(seq) FROM bide_events WHERE run_id = $1)`, trail); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runBide(args...); code != 1 || !strings.Contains(out, "records were deleted") {
		t.Fatalf("a deleted record: exit %d\n%s", code, out)
	}
}
