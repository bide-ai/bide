package migratecmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/store/postgres"
)

func run(t *testing.T, env map[string]string, args ...string) (code int, out, errOut string) {
	t.Helper()
	old := getenv
	getenv = func(k string) string { return env[k] }
	t.Cleanup(func() { getenv = old })
	var o, e bytes.Buffer
	code = Main(append(args, "-color", "never"), &o, &e)
	return code, o.String(), e.String()
}

func TestUsageErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
		want string
	}{
		"no variable named": {nil, nil, "-postgres-env is required"},
		"variable unset":    {nil, []string{"-postgres-env", "X"}, "X is not set"},
		"a dsn as argument": {map[string]string{"X": "postgres://u:p@h/db"}, []string{"-postgres-env", "X", "postgres://u:secret@h/db"}, "never an argument"},
	} {
		code, out, errOut := run(t, tc.env, tc.args...)
		if code != 2 || !strings.Contains(errOut, tc.want) || out != "" {
			t.Errorf("%s: code %d, out %q, err %q", name, code, out, errOut)
		}
		if strings.Contains(errOut, "secret") && name == "a dsn as argument" && strings.Count(errOut, "secret") > 1 {
			t.Errorf("%s: the argument is echoed more than once: %q", name, errOut)
		}
	}
}

func TestReport(t *testing.T) {
	old := migrate
	t.Cleanup(func() { migrate = old })
	env := map[string]string{"D": "postgres://u:hunter2@h/db"}

	migrate = func(_ context.Context, dsn string, opts ...postgres.Option) (postgres.MigrationResult, error) {
		return postgres.MigrationResult{Schema: "app", Before: 0, After: 1, Created: []string{`table "app".bide_steps`}, Statements: []string{"CREATE X"}}, nil
	}
	code, out, _ := run(t, env, "-postgres-env", "D", "-schema", "app")
	if code != 0 || !strings.Contains(out, "version before none, after 1") || !strings.Contains(out, `created table "app".bide_steps`) || strings.Contains(out, "CREATE X") {
		t.Errorf("first migration: %d %q", code, out)
	}

	migrate = func(context.Context, string, ...postgres.Option) (postgres.MigrationResult, error) {
		return postgres.MigrationResult{Schema: "app", Before: 1, After: 1}, nil
	}
	code, out, _ = run(t, env, "-postgres-env", "D")
	if code != 0 || !strings.Contains(out, "version before 1, after 1") || !strings.Contains(out, "nothing to create") {
		t.Errorf("current schema: %d %q", code, out)
	}

	migrate = func(context.Context, string, ...postgres.Option) (postgres.MigrationResult, error) {
		return postgres.MigrationResult{Schema: "app", After: 1, DryRun: true, Created: []string{"index i"}, Statements: []string{"\n CREATE INDEX i\n"}}, nil
	}
	code, out, _ = run(t, env, "-postgres-env", "D", "-dry-run")
	if code != 0 || !strings.Contains(out, "rolled back") || !strings.Contains(out, "CREATE INDEX i;") || !strings.Contains(out, "would create index i") {
		t.Errorf("dry run: %d %q", code, out)
	}
}

// A failure exits 1 and shows neither the DSN nor its password, even when the driver echoes them.
func TestFailureRedactsTheDSN(t *testing.T) {
	old := migrate
	t.Cleanup(func() { migrate = old })
	const dsn = "postgres://u:hunter2@h/db"
	migrate = func(_ context.Context, d string, _ ...postgres.Option) (postgres.MigrationResult, error) {
		return postgres.MigrationResult{}, fmt.Errorf("cannot connect to %s: password hunter2 refused", d)
	}
	code, out, errOut := run(t, map[string]string{"D": dsn}, "-postgres-env", "D")
	if code != 1 || out != "" || strings.Contains(errOut, "hunter2") || strings.Contains(errOut, dsn) || !strings.Contains(errOut, "cannot connect") {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
}

// Against a real database: dry run, migrate, migrate again. The tables stay (a throwaway CI database).
func TestAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	prefix := fmt.Sprintf("mc%d_", time.Now().UnixNano())
	env := map[string]string{"D": dsn}
	code, out, errOut := run(t, env, "-postgres-env", "D", "-prefix", prefix, "-dry-run")
	if code != 0 || !strings.Contains(out, "would create table") || !strings.Contains(out, "CREATE TABLE IF NOT EXISTS") {
		t.Fatalf("dry run: %d %q %q", code, out, errOut)
	}
	if s, err := postgres.Open(context.Background(), dsn, postgres.WithTablePrefix(prefix), postgres.WithoutMigration()); err == nil {
		s.Close()
		t.Fatal("the dry run left the schema behind")
	} else if !errors.Is(err, postgres.ErrSchemaMissing) {
		t.Fatal(err)
	}
	code, out, errOut = run(t, env, "-postgres-env", "D", "-prefix", prefix)
	if code != 0 || !strings.Contains(out, "version before none, after 1") {
		t.Fatalf("migrate: %d %q %q", code, out, errOut)
	}
	code, out, errOut = run(t, env, "-postgres-env", "D", "-prefix", prefix)
	if code != 0 || !strings.Contains(out, "version before 1, after 1") || !strings.Contains(out, "nothing to create") {
		t.Fatalf("migrate again: %d %q %q", code, out, errOut)
	}
}
