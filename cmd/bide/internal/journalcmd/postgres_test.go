package journalcmd

// The bank path, end to end on Postgres: the application role reads and appends, a separate role holds
// only EXECUTE on the redaction function, and the command erases through that role's own DSN. Skips
// without PG_DSN (a superuser's DSN of a scratch database).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/internal/approvecmd"
	"github.com/bide-ai/bide/internal/auditcli"
	"github.com/bide-ai/bide/store/postgres"
)

const pgPassword = "journalcmd-test"

func pgDSN(t *testing.T, base, user string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(user, pgPassword)
	return u.String()
}

func TestPostgresErasureThroughTheRedactionRole(t *testing.T) {
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	exec := func(stmts ...string) {
		t.Helper()
		for _, q := range stmts {
			if _, err := admin.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	const schema, owner, app, red = "jcschema", "jc_owner", "jc_app", "jc_redactor"
	for _, r := range []string{owner, app, red} {
		admin.ExecContext(ctx, `DROP OWNED BY `+r+` CASCADE`)
		admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+r)
		exec(`CREATE ROLE ` + r + ` LOGIN PASSWORD '` + pgPassword + `'`)
	}
	admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	exec(`CREATE SCHEMA ` + schema + ` AUTHORIZATION ` + owner)
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		for _, r := range []string{owner, app, red} {
			admin.ExecContext(context.Background(), `DROP OWNED BY `+r+` CASCADE`)
			admin.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+r)
		}
	})
	if _, err := postgres.Migrate(ctx, pgDSN(t, base, owner), postgres.WithSchema(schema)); err != nil {
		t.Fatal(err)
	}
	exec(
		`GRANT USAGE ON SCHEMA `+schema+` TO `+app+`, `+red,
		`GRANT SELECT, INSERT ON `+schema+`.bide_steps, `+schema+`.bide_events TO `+app,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON `+schema+`.bide_leases TO `+app,
		`GRANT SELECT ON `+schema+`.bide_schema_version TO `+app+`, `+red,
		`GRANT EXECUTE ON FUNCTION `+schema+`.bide_next_seq_v1(text) TO `+app,
		`GRANT EXECUTE ON FUNCTION `+schema+`.bide_redact_v1(text, text, bytea, bigint) TO `+red,
	)
	appDSN, redDSN := pgDSN(t, base, app), pgDSN(t, base, red)
	appStore, err := postgres.Open(ctx, appDSN, postgres.WithSchema(schema), postgres.WithoutMigration())
	if err != nil {
		t.Fatal(err)
	}
	defer appStore.Close()
	j := agenttest.MustJournal(appStore)
	run := "pgrun-" + strings.ReplaceAll(t.Name(), "/", "-")
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("your account 4111-1111 is fine")), j)
	if _, err := a.Run(ctx, run, agent.UserText("my name is Alice, account 4111-1111")); err != nil {
		t.Fatal(err)
	}

	// The application role cannot erase, whatever it calls.
	recs, err := j.History(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	var start agent.Record
	for _, r := range recs {
		if r.Name == "run:start" {
			start = r
		}
	}
	tomb, err := agent.NewTombstone("run:start", start.Raw(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if done, err := appStore.Redact(ctx, run, "run:start", start.Raw(), tomb); done || err == nil {
		t.Fatalf("the application role erased: %v, %v", done, err)
	}

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "log.key")
	var o, e bytes.Buffer
	if code := approvecmd.KeygenMain([]string{"-out", keyPath, "-color", "never"}, &o, &e); code != 0 {
		t.Fatal(e.String())
	}
	signer, err := approvecmd.LoadSigner(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	th, err := audit.NewTreeHead(ctx, j, run, time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	head, err := audit.SignTreeHead(th, signer)
	if err != nil {
		t.Fatal(err)
	}
	headFile := filepath.Join(dir, "r.sth")
	b, _ := json.Marshal(head)
	os.WriteFile(headFile, b, 0o644)

	oldEnv := getenv
	t.Cleanup(func() { getenv = oldEnv })
	env := map[string]string{"JC_APP": appDSN, "JC_RED": redDSN}
	getenv = func(k string) string { return env[k] }
	cli := func(extra ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(append([]string{"redact", "-color", "never", "-postgres-env", "JC_APP", "-schema", schema, "-run", run, "-payloads"}, extra...), &out, &errb)
		return code, out.String(), errb.String()
	}
	code, out, errb := cli()
	if code != 0 || !strings.Contains(out, "plan digest:") {
		t.Fatalf("dry run: %d %s%s", code, out, errb)
	}
	digest := digestOf(t, out)
	headOut := filepath.Join(dir, "heads.jsonl")
	execArgs := func(extra ...string) []string {
		return append([]string{"-execute", "-plan-digest", digest, "-reason", "customer request", "-ticket", "PRIV-9", "-deployment", "bank-a",
			"-sign-key", keyPath, "-head-out", headOut, "-pubkey", keyPath + ".pub", "-sth", headFile}, extra...)
	}
	// Without the redaction role's DSN the application's role is the one asked to erase, and cannot.
	if code, _, errb := cli(execArgs()...); code != 1 {
		t.Fatalf("erasing as the application role: %d %s", code, errb)
	}
	if strings.Contains(errb, "postgres://") || strings.Contains(errb, pgPassword) {
		t.Errorf("the DSN or its password is in the error: %s", errb)
	}
	if content(t, ctx, j, run) == false {
		t.Fatal("the failed attempt erased something")
	}
	// Through the redaction role's own DSN it works.
	code, out, errb = cli(execArgs("-redact-postgres-env", "JC_RED")...)
	if code != 0 || !strings.Contains(out, "storage, for the operator") || !strings.Contains(out, "VACUUM") {
		t.Fatalf("erasing through the redaction role: %d\n%s%s", code, out, errb)
	}
	if content(t, ctx, j, run) {
		t.Fatal("the content is still in the journal")
	}
	// bide audit redactions passes against the database.
	auditcli.Stores = &auditcli.StoreOpeners{Postgres: func(dsn string) (auditcli.NotifyStores, error) {
		s, err := postgres.OpenReadOnly(ctx, dsn, postgres.WithSchema(schema))
		if err != nil {
			return auditcli.NotifyStores{}, err
		}
		return auditcli.NotifyStores{Journal: s, Events: s.EventStore(), Close: s.Close}, nil
	}}
	t.Cleanup(func() { auditcli.Stores = nil })
	t.Setenv("JC_APP", appDSN)
	hb, _ := os.ReadFile(headOut)
	args := []string{"redactions", "-postgres-env", "JC_APP", "-run", run, "-deployment", "bank-a", "-pubkey", keyPath + ".pub"}
	for i, line := range bytes.Split(bytes.TrimSpace(hb), []byte("\n")) { // every head the command wrote, the failed attempt's too
		f := filepath.Join(dir, "trail"+string(rune('0'+i))+".sth")
		os.WriteFile(f, line, 0o644)
		args = append(args, "-sth", f)
	}
	var ao, ae bytes.Buffer
	if code := auditcli.Main("test", args, &ao, &ae); code != 0 || !strings.Contains(ao.String(), "PASS") {
		t.Fatalf("bide audit redactions on Postgres = %d\n%s%s", code, ao.String(), ae.String())
	}
	// And the role that erased can read nothing.
	redStore, err := postgres.Open(ctx, redDSN, postgres.WithSchema(schema), postgres.WithoutMigration())
	if err == nil {
		defer redStore.Close()
		if _, _, err := redStore.Get(ctx, run, "run:start"); err == nil {
			t.Error("the redaction role read the journal")
		}
	}
}

// content reports whether the run's journal still holds the person's name.
func content(t *testing.T, ctx context.Context, j *agent.Journal, run string) bool {
	t.Helper()
	for e, err := range j.Store().Load(ctx, run, -1) {
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(e.Data, []byte("Alice")) || bytes.Contains(e.Data, []byte("4111")) {
			return true
		}
	}
	return false
}
