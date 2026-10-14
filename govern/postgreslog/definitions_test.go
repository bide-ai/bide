package postgreslog_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern/postgreslog"
)

// A schema whose columns, triggers, rules or row-level security differ from what the migration
// creates is refused by every way of opening the log, because the migration never alters a table.
func TestDefinitionsAreChecked(t *testing.T) {
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres event-log integration test")
	}
	cases := []struct {
		name  string
		setup []string // run as the admin, with S for the schema
		want  string   // "" when the schema is accepted
	}{
		{"untouched", nil, ""},
		{"extra nullable column", []string{`ALTER TABLE S.governed_events ADD COLUMN note text`}, ""},
		{"append_id NOT NULL", []string{`ALTER TABLE S.governed_events ALTER COLUMN append_id SET NOT NULL`}, ""},
		{"no version table, as before it existed", []string{`DROP TABLE S.governed_events_schema_version`}, ""},
		{"nondeterministic collation", []string{
			`CREATE COLLATION S.ci (provider = icu, locale = 'und-u-ks-level2', deterministic = false)`,
			`ALTER TABLE S.governed_events ALTER COLUMN entity TYPE text COLLATE S.ci`}, "nondeterministic"},
		{"event as bytea", []string{`ALTER TABLE S.governed_events ALTER COLUMN event TYPE bytea USING convert_to(event, 'UTF8')`}, "has type bytea"},
		{"seq as int4", []string{`ALTER TABLE S.governed_events ALTER COLUMN seq TYPE int4`}, "has type integer"},
		{"append_id as int", []string{`ALTER TABLE S.governed_events DROP COLUMN append_id`, `ALTER TABLE S.governed_events ADD COLUMN append_id int`, `CREATE UNIQUE INDEX IF NOT EXISTS governed_events_append_id ON S.governed_events (entity, append_id)`}, "has type integer"},
		{"event nullable", []string{`ALTER TABLE S.governed_events ALTER COLUMN event DROP NOT NULL`}, "nullable"},
		{"extra NOT NULL column with no default", []string{`ALTER TABLE S.governed_events ADD COLUMN must text`, `UPDATE S.governed_events SET must = ''`, `ALTER TABLE S.governed_events ALTER COLUMN must SET NOT NULL`}, "extra NOT NULL"},
		{"BEFORE INSERT trigger", []string{
			`CREATE FUNCTION S.evil() RETURNS trigger LANGUAGE plpgsql AS $x$ BEGIN NEW.event := 'x'; RETURN NEW; END $x$`,
			`CREATE TRIGGER evil BEFORE INSERT ON S.governed_events FOR EACH ROW EXECUTE FUNCTION S.evil()`}, "a trigger"},
		{"rule", []string{`CREATE RULE hide AS ON INSERT TO S.governed_events DO INSTEAD NOTHING`}, "a rule"},
		{"row-level security", []string{`ALTER TABLE S.governed_events ENABLE ROW LEVEL SECURITY`}, "row-level security"},
		{"version as bigint", []string{`ALTER TABLE S.governed_events_schema_version ALTER COLUMN version TYPE bigint`}, "has type bigint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			admin, err := sql.Open("pgx", base)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			schema := fmt.Sprintf("df_%d", time.Now().UnixNano())
			if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`) })
			if err := postgreslog.Migrate(ctx, base, postgreslog.WithSchema(schema)); err != nil {
				t.Fatal(err)
			}
			for _, q := range tc.setup {
				if _, err := admin.ExecContext(ctx, strings.ReplaceAll(q, "S.", schema+".")); err != nil {
					if strings.Contains(q, "COLLATION") && strings.Contains(err.Error(), "ICU") {
						t.Skipf("no ICU: %v", err)
					}
					t.Fatalf("%s: %v", q, err)
				}
			}
			for name, o := range map[string][]postgreslog.Option{
				"WithoutMigration": {postgreslog.WithSchema(schema), postgreslog.WithoutMigration()},
				"Open":             {postgreslog.WithSchema(schema)},
			} {
				l, err := postgreslog.Open(ctx, base, o...)
				if l != nil {
					l.Close()
				}
				switch {
				case tc.want == "" && err != nil:
					t.Errorf("%s refused an acceptable schema: %v", name, err)
				case tc.want != "" && err == nil:
					t.Errorf("%s accepted the schema", name)
				case tc.want != "" && (!strings.Contains(err.Error(), tc.want) || !errors.Is(err, agent.ErrConfig)):
					t.Errorf("%s: %v, want an ErrConfig error containing %q", name, err, tc.want)
				}
			}
		})
	}
}
