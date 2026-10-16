package runview

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestApproveCommandForTheJournal: the command names the journal as the viewer was given it: a SQLite
// file by its absolute path, a Postgres journal by the same -postgres-env variable, and a raw -postgres
// DSN by a placeholder, never the DSN.
func TestApproveCommandForTheJournal(t *testing.T) {
	const dsn = "postgres://app:hunter2@db.internal:5432/bide"
	abs, _ := filepath.Abs("run.db")
	if strings.ContainsAny(abs, "\\ ") { // a Windows path: approveline quotes it as one POSIX word
		abs = "'" + abs + "'"
	}
	for name, c := range map[string]struct {
		f    flags
		want string
	}{
		"sqlite":  {flags{db: "run.db"}, "bide approve -db " + abs + " -run r1 -call c1"},
		"env":     {flags{postgresEnv: "ORDERS_DSN"}, "bide approve -postgres-env ORDERS_DSN -run r1 -call c1"},
		"raw dsn": {flags{postgres: dsn}, "bide approve -postgres-env '<VAR>' -run r1 -call c1"},
	} {
		got, err := approveCommand(c.f)("r1", "c1", false)
		if err != nil || !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: %q, %v; want it to start %q", name, got, err, c.want)
		}
		if strings.Contains(got, "hunter2") || strings.Contains(got, "postgres://") {
			t.Errorf("%s: the command carries the DSN: %q", name, got)
		}
	}
}

// TestPostgresEnvFlag: -postgres-env reads the DSN from the variable (an unset one is refused), and is
// exclusive with -postgres and the other inputs.
func TestPostgresEnvFlag(t *testing.T) {
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(args, &out, &errb, Stores{})
		return code, errb.String()
	}
	if code, msg := run("-postgres", "x", "-postgres-env", "V"); code != 2 || !strings.Contains(msg, "not both") {
		t.Errorf("both: %d %q", code, msg)
	}
	if code, msg := run("-db", "x", "-postgres-env", "V"); code != 2 || !strings.Contains(msg, "exactly one") {
		t.Errorf("with -db: %d %q", code, msg)
	}
	t.Setenv("BIDE_VIEW_TEST_UNSET", "")
	if code, msg := run("-postgres-env", "BIDE_VIEW_TEST_UNSET"); code != 1 || !strings.Contains(msg, "empty or not set") {
		t.Errorf("unset: %d %q", code, msg)
	}
}

// TestPostgresEnvIsNotADSN: a DSN given where the variable's name belongs is a usage error that does
// not echo it.
func TestPostgresEnvIsNotADSN(t *testing.T) {
	for _, v := range []string{"postgres://u:S3cr3tPw@127.0.0.1:1/db", "a b", "1x"} {
		var out, errb bytes.Buffer
		code := Main([]string{"-postgres-env", v}, &out, &errb, Stores{})
		if code != 2 || !strings.Contains(errb.String(), "-postgres-env takes the name of an environment variable, not a DSN") || strings.Contains(errb.String(), "S3cr3tPw") || (strings.Contains(v, "://") && strings.Contains(errb.String(), v)) {
			t.Errorf("%q: exit %d, stderr %q", v, code, errb.String())
		}
	}
}

// TestApproveCertCommandForTheJournal: the -cert command names the journal as approveCommand does (a
// SQLite file by its absolute path, a Postgres journal by the variable, a raw DSN by a placeholder), and
// carries the placeholders for the key, the chain and the policy file, never a DSN.
func TestApproveCertCommandForTheJournal(t *testing.T) {
	const dsn = "postgres://app:hunter2@db.internal:5432/bide"
	abs, _ := filepath.Abs("run.db")
	if strings.ContainsAny(abs, "\\ ") {
		abs = "'" + abs + "'"
	}
	for name, c := range map[string]struct {
		f    flags
		want string
	}{
		"sqlite":  {flags{db: "run.db"}, "bide approve -db " + abs + " -run r1 -call c1 -as '<approver>' "},
		"env":     {flags{postgresEnv: "ORDERS_DSN"}, "bide approve -postgres-env ORDERS_DSN -run r1 -call c1 -as '<approver>' "},
		"raw dsn": {flags{postgres: dsn}, "bide approve -postgres-env '<VAR>' -run r1 -call c1 -as '<approver>' "},
	} {
		got, err := approveCertCommand(c.f)("r1", "c1")
		if err != nil || !strings.HasPrefix(got, c.want) || !strings.HasSuffix(got, " -key '<key-file>' -cert '<chain.pem>' -identity-policy '<identity-policy.json>'") {
			t.Errorf("%s: %q, %v; want it to start %q", name, got, err, c.want)
		}
		if strings.Contains(got, "hunter2") || strings.Contains(got, "postgres://") {
			t.Errorf("%s: the command carries the DSN: %q", name, got)
		}
	}
}
