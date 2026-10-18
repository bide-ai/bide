//go:build !bide_audit_only

package main

import (
	"context"
	"encoding/json"
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

// stopNotify sweeps the control run of s into its event store (a notifier told of every stop), as a
// deployment's notifier process does.
func stopNotify(t *testing.T, s *sqlite.Store, at time.Time) {
	t.Helper()
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopBasic))
	if err != nil {
		t.Fatal(err)
	}
	err = agent.NotifyPending(context.Background(), j, okSink{}, audit.NewNotifyTrail(s.EventStore()), agent.WithNotifyStops(ctl), agent.WithNotifyHolder("p1"))
	if err != nil {
		t.Fatal(err)
	}
}

func setStopIn(t *testing.T, s *sqlite.Store, tool string) agent.StopEntry {
	t.Helper()
	j, _ := agent.NewJournal(s)
	ctl, _ := agent.NewStopControl(j, agent.WithStopMode(agent.StopBasic))
	e, err := ctl.SetStop(context.Background(), agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopTool, Name: tool}, Reason: "incident", By: "dayna", Host: "h"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func liftStopIn(t *testing.T, s *sqlite.Store, id string) {
	t.Helper()
	j, _ := agent.NewJournal(s)
	ctl, _ := agent.NewStopControl(j, agent.WithStopMode(agent.StopBasic))
	if _, err := ctl.LiftStop(context.Background(), id, agent.LiftRequest{By: "dayna"}); err != nil {
		t.Fatal(err)
	}
}

// bide audit notifications checks the kill switch's notifications from the control run: a stop the notifier
// was told of passes; a stop no notifier saw, and a lift no notifier saw, fail.
func TestAuditNotifications_StopsFromTheControlRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.db")
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := setStopIn(t, s, "refund")
	stopNotify(t, s, time.Now())
	s.Close()
	late := time.Now().Add(time.Hour).Format(time.RFC3339)
	code, out, errOut := runBide("audit", "notifications", "-db", path, "-unanchored", "-as-of", late)
	if code != 0 || !strings.Contains(out, "stop_set") || !strings.Contains(out, "PASS") {
		t.Fatalf("a notified stop: exit %d\n%s\n%s", code, out, errOut)
	}

	// The stop is lifted and nobody is told: the lift (and the all-clear of the stop) are owed.
	s, _ = sqlite.Open(path)
	liftStopIn(t, s, e.ID)
	s.Close()
	code, out, _ = runBide("audit", "notifications", "-db", path, "-unanchored", "-as-of", late)
	if code != 1 || !strings.Contains(out, "never_notified") || !strings.Contains(out, "stop_lifted") {
		t.Fatalf("a lift no notifier saw: exit %d\n%s", code, out)
	}
	s, _ = sqlite.Open(path)
	stopNotify(t, s, time.Now())
	s.Close()
	if code, out, _ := runBide("audit", "notifications", "-db", path, "-unanchored", "-as-of", late); code != 0 {
		t.Fatalf("after the notifier caught up: exit %d\n%s", code, out)
	}

	// A stop no notifier saw.
	s, _ = sqlite.Open(path)
	setStopIn(t, s, "payout")
	s.Close()
	if code, out, _ := runBide("audit", "notifications", "-db", path, "-unanchored", "-as-of", late); code != 1 || !strings.Contains(out, "never_notified") || !strings.Contains(out, "stop_set") {
		t.Fatalf("a stop no notifier saw: exit %d\n%s", code, out)
	}
}

// auditedStopNotify sweeps the control run of an audited deployment into its event store, judged under the
// policy, as the deployment's notifier does.
func auditedStopNotify(t *testing.T, e *stopEnv) {
	t.Helper()
	s, err := sqlite.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j, _ := agent.NewJournal(s)
	b, _ := os.ReadFile(e.policy)
	var doc agent.StopPolicyDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	pol, err := audit.StopPolicyFromDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(pol), agent.WithStopVerifier(audit.StopVerifierFromAudit))
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.NotifyPending(context.Background(), j, okSink{}, audit.NewNotifyTrail(s.EventStore()), agent.WithNotifyStops(ctl), agent.WithNotifyHolder("p1")); err != nil {
		t.Fatal(err)
	}
}

// An audited deployment's stops are judged under its policy: without it the audit refuses (it cannot read the
// signed records), with it an unnotified stop fails and a notified one passes.
func TestAuditNotifications_AuditedStops(t *testing.T) {
	e := newStopEnv(t, 1, "ops")
	if code, _, errOut := e.stop("ops", "-tool", "refund", "-reason", "incident"); code != 0 {
		t.Fatalf("set: %d\n%s", code, errOut)
	}
	late := time.Now().Add(time.Hour).Format(time.RFC3339)
	code, _, errOut := runBide("audit", "notifications", "-db", e.db, "-unanchored", "-as-of", late)
	if code != 4 || !strings.Contains(errOut, "give -stop-policy") {
		t.Fatalf("no policy: exit %d\n%s", code, errOut)
	}
	code, out, _ := runBide("audit", "notifications", "-db", e.db, "-unanchored", "-as-of", late, "-stop-policy", e.policy)
	if code != 1 || !strings.Contains(out, "never_notified") || !strings.Contains(out, "stop_set") {
		t.Fatalf("an audited stop no notifier saw: exit %d\n%s", code, out)
	}
	auditedStopNotify(t, e)
	if code, out, errOut := runBide("audit", "notifications", "-db", e.db, "-unanchored", "-as-of", late, "-stop-policy", e.policy); code != 0 {
		t.Fatalf("after the notifier: exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestAuditNotifications_StopFlagErrors(t *testing.T) {
	e := newStopEnv(t, 0)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"policy with files", []string{"-trail", "x.json", "-unanchored", "-stop-policy", "p.json"}, "need -db"},
		{"root alone", []string{"-db", e.db, "-unanchored", "-stop-root", strings.Repeat("a", 64)}, "-stop-root needs -stop-policy"},
		{"short window", []string{"-db", e.db, "-unanchored", "-stops-within", "1s"}, "at least one minute"},
	} {
		code, _, errOut := runBide(append([]string{"audit", "notifications"}, tc.args...)...)
		if code != 2 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: exit %d\n%s", tc.name, code, errOut)
		}
	}
}
