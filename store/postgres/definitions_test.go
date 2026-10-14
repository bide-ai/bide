package postgres

// The checks of the tables' definitions: a schema whose columns, indexes, triggers, rules or
// row-level security differ from what the migration creates is refused by every way of opening the
// store, because the migration never alters a table and the statements depend on the definition.
// Skips without PG_DSN.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func TestDefinitionsAreChecked(t *testing.T) {
	const schema = "dfschema"
	cases := []struct {
		name  string
		setup []string // run as the admin, with S for the schema
		want  string   // "" when the schema is accepted; else a substring of the refusal
	}{
		{"untouched", nil, ""},
		{"run_id in the default collation, as v0.7.0 made it", []string{`ALTER TABLE S.bide_steps ALTER COLUMN run_id TYPE text COLLATE "default"`}, ""},
		{"extra nullable column", []string{`ALTER TABLE S.bide_steps ADD COLUMN note text`}, ""},
		{"extra NOT NULL column with a default", []string{`ALTER TABLE S.bide_leases ADD COLUMN gen int NOT NULL DEFAULT 0`}, ""},
		{"run_id with a nondeterministic collation", []string{
			`CREATE COLLATION S.ci (provider = icu, locale = 'und-u-ks-level2', deterministic = false)`,
			`ALTER TABLE S.bide_steps ALTER COLUMN run_id TYPE text COLLATE S.ci`}, "nondeterministic"},
		{"data as text", []string{`ALTER TABLE S.bide_steps ALTER COLUMN data TYPE text USING encode(data, 'hex')`}, "has type text, not bytea"},
		{"seq as int4", []string{`ALTER TABLE S.bide_steps ALTER COLUMN seq TYPE int4`}, "has type integer, not bigint"},
		{"holder dropped", []string{`ALTER TABLE S.bide_leases DROP COLUMN holder`}, "column holder"},
		{"leaf dropped", []string{`ALTER TABLE S.bide_events DROP COLUMN leaf`}, "column leaf"},
		{"expiry nullable", []string{`ALTER TABLE S.bide_leases ALTER COLUMN expiry DROP NOT NULL`}, "nullable"},
		{"version as bigint", []string{`ALTER TABLE S.bide_schema_version ALTER COLUMN version TYPE bigint`}, "has type bigint"},
		{"extra NOT NULL column with no default", []string{`ALTER TABLE S.bide_events ADD COLUMN must text`, `UPDATE S.bide_events SET must = ''`, `ALTER TABLE S.bide_events ALTER COLUMN must SET NOT NULL`}, "extra NOT NULL"},
		{"BEFORE INSERT trigger", []string{
			`CREATE FUNCTION S.evil() RETURNS trigger LANGUAGE plpgsql AS $x$ BEGIN NEW.data := '\x00'; RETURN NEW; END $x$`,
			`CREATE TRIGGER evil BEFORE INSERT ON S.bide_steps FOR EACH ROW EXECUTE FUNCTION S.evil()`}, "a trigger"},
		{"AFTER trigger on the events", []string{
			`CREATE FUNCTION S.noop() RETURNS trigger LANGUAGE plpgsql AS $x$ BEGIN RETURN NULL; END $x$`,
			`CREATE TRIGGER noop AFTER INSERT ON S.bide_events FOR EACH ROW EXECUTE FUNCTION S.noop()`}, "a trigger"},
		{"rule", []string{`CREATE RULE hide AS ON INSERT TO S.bide_leases DO INSTEAD NOTHING`}, "a rule"},
		{"row-level security", []string{`ALTER TABLE S.bide_steps ENABLE ROW LEVEL SECURITY`}, "row-level security"},
		{"lease index is a table", []string{`DROP INDEX S.bide_leases_run_c`, `CREATE TABLE S.bide_leases_run_c (x int)`}, "is not an index"},
		{"lease index on the wrong column", []string{`DROP INDEX S.bide_leases_expiry`, `CREATE INDEX bide_leases_expiry ON S.bide_leases (holder)`}, "is not the one the migration creates"},
		{"lease index in the wrong collation", []string{`DROP INDEX S.bide_leases_run_c`, `CREATE INDEX bide_leases_run_c ON S.bide_leases (run_id)`}, "is not the one the migration creates"},
		{"lease index partial", []string{`DROP INDEX S.bide_leases_expiry`, `CREATE INDEX bide_leases_expiry ON S.bide_leases (expiry) WHERE holder <> ''`}, "is not the one the migration creates"},
		{"lease index unique", []string{`DROP INDEX S.bide_leases_expiry`, `CREATE UNIQUE INDEX bide_leases_expiry ON S.bide_leases (expiry)`}, "unique index on (expiry)"},
		{"seq identity always", []string{`ALTER TABLE S.bide_steps ALTER COLUMN seq ADD GENERATED ALWAYS AS IDENTITY`}, "identity"},
		{"data generated", []string{`ALTER TABLE S.bide_steps DROP COLUMN data`, `ALTER TABLE S.bide_steps ADD COLUMN data bytea GENERATED ALWAYS AS ('\x00'::bytea) STORED NOT NULL`}, "generated"},
		{"check constraint on data", []string{`ALTER TABLE S.bide_steps ADD CONSTRAINT c CHECK (length(data) < 3)`}, "constraint CHECK"},
		{"exclusion constraint", []string{`ALTER TABLE S.bide_leases ADD CONSTRAINT x EXCLUDE (holder WITH =)`}, "constraint EXCLUDE"},
		{"foreign key", []string{`ALTER TABLE S.bide_events ADD CONSTRAINT f FOREIGN KEY (run_id, seq) REFERENCES S.bide_events (run_id, seq)`}, "constraint FOREIGN KEY"},
		{"extra unique index", []string{`CREATE UNIQUE INDEX u ON S.bide_steps (data)`}, "unique index on (data)"},
		{"unique index on an expression", []string{`CREATE UNIQUE INDEX u ON S.bide_leases (lower(holder))`}, "unique index"},
		{"partial unique index", []string{`CREATE UNIQUE INDEX u ON S.bide_events (leaf) WHERE seq > 5`}, "unique index on (leaf)"},
		{"partitioned steps with a trigger on the partition", []string{
			`DROP TABLE S.bide_steps`,
			`CREATE TABLE S.bide_steps (run_id text COLLATE "C" NOT NULL, seq int8 NOT NULL, name text NOT NULL, data bytea NOT NULL, UNIQUE (run_id, seq), UNIQUE (run_id, name)) PARTITION BY HASH (run_id)`,
			`CREATE TABLE S.bide_steps_p0 PARTITION OF S.bide_steps FOR VALUES WITH (MODULUS 1, REMAINDER 0)`,
			`CREATE FUNCTION S.evil() RETURNS trigger LANGUAGE plpgsql AS $x$ BEGIN NEW.data := '\x00'; RETURN NEW; END $x$`,
			`CREATE TRIGGER evil BEFORE INSERT ON S.bide_steps_p0 FOR EACH ROW EXECUTE FUNCTION S.evil()`}, "ordinary table"},
		{"partitioned events", []string{
			`DROP TABLE S.bide_events`,
			`CREATE TABLE S.bide_events (run_id text COLLATE "C" NOT NULL, seq int8 NOT NULL CHECK (seq >= 0), leaf bytea NOT NULL, PRIMARY KEY (run_id, seq)) PARTITION BY HASH (run_id)`,
			`CREATE TABLE S.bide_events_p0 PARTITION OF S.bide_events FOR VALUES WITH (MODULUS 1, REMAINDER 0)`}, "ordinary table"},
		{"partitioned steps with no partitions", []string{
			`DROP TABLE S.bide_steps`,
			`CREATE TABLE S.bide_steps (run_id text COLLATE "C" NOT NULL, seq int8 NOT NULL, name text NOT NULL, data bytea NOT NULL, UNIQUE (run_id, seq), UNIQUE (run_id, name)) PARTITION BY HASH (run_id)`}, "ordinary table"},
		{"inheritance child of steps", []string{`CREATE TABLE S.bide_steps_child () INHERITS (S.bide_steps)`}, "partition or inheritance"},
		{"leases inherits another table", []string{`CREATE TABLE S.parent (run_id text NOT NULL)`, `ALTER TABLE S.bide_leases INHERIT S.parent`}, "partition or inheritance"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admin, base := rv120Admin(t)
			ctx := context.Background()
			rv120Schema(t, admin, schema)
			if _, err := Migrate(ctx, base, WithSchema(schema)); err != nil {
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
			for name, open := range map[string]func() (*Store, error){
				"WithoutMigration": func() (*Store, error) { return Open(ctx, base, WithSchema(schema), WithoutMigration()) },
				"Open":             func() (*Store, error) { return Open(ctx, base, WithSchema(schema)) },
			} {
				s, err := open()
				if s != nil {
					s.Close()
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
			if tc.want != "" { // Migrate checks the schema after migrating, too
				if _, err := Migrate(ctx, base, WithSchema(schema)); err == nil {
					t.Error("Migrate accepted the schema")
				}
			}
		})
	}
}
