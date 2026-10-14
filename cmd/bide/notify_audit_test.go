//go:build !bide_audit_only

package main

import (
	"context"
	"errors"
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

type okSink struct{}

func (okSink) Notify(context.Context, agent.Notification) error { return nil }

// gateRun drives run to an approval gate in s, with a notifier writing its trail to s's event store
// (notified) or none (a gate no one was told of).
func gateRun(t *testing.T, s *sqlite.Store, run string, notified bool) {
	gateRunOn(t, s, s.EventStore(), run, notified)
}

func gateRunOn(t *testing.T, s agent.Store, es audit.EventStore, run string, notified bool) {
	t.Helper()
	ctx := context.Background()
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	opts := []agent.Option{agent.WithTools(tool)}
	if notified {
		opts = append(opts, agent.WithNotifier(okSink{}, audit.NewNotifyTrail(es), agent.WithNotifyHolder("p1")))
	}
	a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), j, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, run, agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatalf("Run = %v", err)
	}
	if err := a.FlushNotifications(ctx); err != nil {
		t.Fatal(err)
	}
}

// bide audit notifications reads a real SQLite journal and its event store, read-only, and finds a
// gate no notifier saw.
func TestAuditNotifications_ReadsASQLiteStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.db")
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	gateRun(t, s, "told", true)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runBide("audit", "notifications", "-db", path, "-unanchored")
	if code != 0 || !strings.Contains(out, "PASS  acked") || !strings.Contains(out, "trail notify:told") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}

	s, err = sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	gateRun(t, s, "untold", false)
	s.Close()
	late := time.Now().Add(time.Hour).Format(time.RFC3339)
	code, out, _ = runBide("audit", "notifications", "-db", path, "-unanchored", "-as-of", late)
	if code != 1 || !strings.Contains(out, "FAIL  never_notified") || !strings.Contains(out, "approval:untold:") {
		t.Fatalf("an un-notified gate: exit %d\n%s", code, out)
	}
	code, out, _ = runBide("audit", "notifications", "-db", path, "-unanchored", "-as-of", late, "-run", "told")
	if code != 0 || strings.Contains(out, "untold") {
		t.Fatalf("-run told: exit %d\n%s", code, out)
	}
	if code, _, errOut := runBide("audit", "notifications", "-db", filepath.Join(t.TempDir(), "none.db"), "-unanchored"); code != 4 {
		t.Fatalf("a database that does not exist: exit %d\n%s", code, errOut)
	}
}

// The same against Postgres, through the DSN in an environment variable. It runs against the CI
// Postgres (PG_DSN), one unique run so other tests' runs in the database are not read.
func TestAuditNotifications_ReadsAPostgresStore(t *testing.T) {
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
	suffix := time.Now().Format("150405.000000000")
	told, untold := "audit-told-"+suffix, "audit-untold-"+suffix
	gateRunOn(t, s, s.EventStore(), told, true)
	gateRunOn(t, s, s.EventStore(), untold, false)
	late := time.Now().Add(time.Hour).Format(time.RFC3339)
	code, out, errOut := runBide("audit", "notifications", "-postgres-env", "BIDE_AUDIT_TEST_DSN", "-run", told, "-unanchored", "-as-of", late)
	if code != 0 || !strings.Contains(out, "PASS  acked") || strings.Contains(out+errOut, dsn) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	code, out, _ = runBide("audit", "notifications", "-postgres-env", "BIDE_AUDIT_TEST_DSN", "-run", untold, "-unanchored", "-as-of", late)
	if code != 1 || !strings.Contains(out, "FAIL  never_notified") {
		t.Fatalf("an un-notified gate: exit %d\n%s", code, out)
	}
}
