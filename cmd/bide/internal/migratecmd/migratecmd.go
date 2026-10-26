// Package migratecmd is bide migrate: it creates or checks a Postgres journal's schema as the
// schema's owner, so the running service can open the store without DDL rights
// (postgres.WithoutMigration):
//
//	bide migrate -postgres-env BIDE_OWNER_DSN -schema bide
//	bide migrate -postgres-env BIDE_OWNER_DSN -schema bide -dry-run
//
// The DSN is read from an environment variable and is never an argument, and never printed.
package migratecmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"

	"github.com/bide-ai/bide/internal/approveline"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/store/postgres"
)

// getenv and migrate are replaced by tests.
var (
	getenv  = os.Getenv
	migrate = postgres.Migrate
)

// Main runs bide migrate with args (after "migrate") and returns the exit status: 0 when the schema
// is current (or, with -dry-run, the migration would succeed), 1 when the migration failed, 2 for a
// usage error.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: bide migrate -postgres-env <var> -schema <name> [-prefix <p>] [-dry-run]

Creates or checks a Postgres journal's schema, as the role in the DSN, so that the running service
can open the store with postgres.WithoutMigration under a role that has no DDL rights. Run it
with the schema's owner at deploy time, and again after upgrading bide.

It creates the tables (bide_steps, bide_leases, bide_events, bide_schema_version, bide_holds),
the bide_next_seq_v1 and bide_redact_v3 functions and the lease indexes that are missing, and
revokes EXECUTE on every older bide_redact function from PUBLIC and from the roles it was granted
to, and does nothing else: it never alters or drops a table or replaces a function, it is
idempotent, and it refuses a schema newer than this bide's. It reports the schema version before and after ("none" before the first
migration), and what it created. With -dry-run it runs the migration in a transaction and rolls
it back, printing the SQL: that shows what would be created and that the role may create it, and
leaves nothing behind.

The DSN is read from the environment variable -postgres-env names and is never an argument or
output. -schema is required (the schema must exist; the migration does not create it): without one the tables would
land in whichever schema the DSN's search path puts first.

Exit status is 0 when the schema is current, 1 when the migration failed, and 2 for a usage
error.

`)
		fs.PrintDefaults()
	}
	pgEnv := fs.String("postgres-env", "", "the environment variable holding the owner role's Postgres DSN (required)")
	schema := fs.String("schema", "", "the schema the tables are in; it must exist (required)")
	prefix := fs.String("prefix", "", `the table name prefix (empty: "bide_")`)
	dry := fs.Bool("dry-run", false, "run the migration in a transaction, print its SQL, and roll back")
	color := term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	st := term.For(stderr, *color)
	usage := func(msg string) int {
		fmt.Fprintln(stderr, st.ErrorPrefix()+"bide migrate:", msg)
		fs.Usage()
		return 2
	}
	switch {
	case fs.NArg() > 0:
		// not echoed: a stray argument may be a DSN with a password
		return usage("unexpected argument (the DSN is never an argument: set -postgres-env)")
	case *pgEnv == "":
		return usage("-postgres-env is required: the name of the environment variable holding the DSN")
	case !approveline.ValidEnvName(*pgEnv):
		return usage(approveline.NotAnEnvName)
	case *schema == "":
		return usage("-schema is required: without one the tables land in the first schema on the DSN's search path")
	}
	dsn := getenv(*pgEnv)
	if dsn == "" {
		return usage("the environment variable " + *pgEnv + " is not set")
	}
	opts := []postgres.Option{postgres.WithSchema(*schema)}
	if *prefix != "" {
		opts = append(opts, postgres.WithTablePrefix(*prefix))
	}
	if *dry {
		opts = append(opts, postgres.WithDryRun())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := migrate(ctx, dsn, opts...)
	out := term.For(stdout, *color)
	if err != nil {
		fmt.Fprintln(stderr, st.ErrorPrefix()+"bide migrate: "+redact(err.Error(), dsn))
		return 1
	}
	report(stdout, out, res)
	return 0
}

// report prints what the migration did.
func report(w io.Writer, st term.Style, res postgres.MigrationResult) {
	if res.DryRun {
		fmt.Fprintln(w, "-- dry run: the SQL below ran in a transaction that was rolled back")
		for _, s := range res.Statements {
			fmt.Fprintln(w, strings.TrimSpace(s)+";")
		}
	}
	before := "none"
	if res.Before > 0 {
		before = fmt.Sprint(res.Before)
	}
	verb := "created"
	if res.DryRun {
		verb = "would create"
	}
	fmt.Fprintf(w, "schema %s: version before %s, after %d\n", res.Schema, before, res.After)
	if len(res.Created) == 0 {
		fmt.Fprintf(w, "%sthe schema is current: nothing to create\n", st.Pass())
		return
	}
	for _, c := range res.Created {
		fmt.Fprintf(w, "%s%s %s\n", st.Pass(), verb, c)
	}
}

// redact removes the DSN and its password from msg: a driver may echo what it was given. It
// handles a URL DSN (the password, encoded or not, and a password query parameter) and a
// keyword/value DSN (password=secret, password='a b').
func redact(msg, dsn string) string {
	secrets := []string{dsn}
	if u, err := url.Parse(dsn); err == nil && strings.Contains(dsn, "://") {
		if u.User != nil {
			if pw, ok := u.User.Password(); ok {
				secrets = append(secrets, pw)
				if _, enc, ok := strings.Cut(u.User.String(), ":"); ok { // as the DSN wrote it
					secrets = append(secrets, enc)
				}
			}
		}
		secrets = append(secrets, u.Query().Get("password"))
	} else {
		for _, m := range keywordPassword.FindAllStringSubmatch(dsn, -1) {
			secrets = append(secrets, unquoteKeyword(m[1]))
		}
	}
	for _, sec := range secrets {
		if sec != "" {
			msg = strings.ReplaceAll(msg, sec, "<redacted>")
		}
	}
	return msg
}

// keywordPassword matches password=value in a keyword/value DSN, the value bare or single-quoted.
var keywordPassword = regexp.MustCompile(`(?i)(?:^|\s)password\s*=\s*('(?:\\.|[^'\\])*'|\S+)`)

// unquoteKeyword is the value of a keyword/value DSN field: a quoted value without its quotes and
// backslash escapes.
func unquoteKeyword(v string) string {
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		v = v[1 : len(v)-1]
		var b strings.Builder
		for i := 0; i < len(v); i++ {
			if v[i] == '\\' && i+1 < len(v) {
				i++
			}
			b.WriteByte(v[i])
		}
		return b.String()
	}
	return v
}
