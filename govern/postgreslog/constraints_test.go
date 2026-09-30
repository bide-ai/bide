package postgreslog_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern/postgreslog"
)

// Open refuses a governed_events table that lacks a uniqueness the single-statement append
// depends on: (entity, seq), without which two appends that read the same MAX(seq) would both
// commit at one position, and (entity, append_id), without which ON CONFLICT (entity, append_id)
// has no arbiter and an id could be recorded twice. The migration skips a table whose append_id
// index exists by name, so an index of that name that is not unique on exactly those columns
// must be refused too. Skips without PG_DSN.
func TestOpen_RefusesATableWithoutItsUniqueness(t *testing.T) {
	const table = `CREATE TABLE %s.governed_events (entity text NOT NULL, seq bigint NOT NULL, event text NOT NULL, append_id text`
	for _, tc := range []struct {
		name, ddl, want string // want is empty when Open must succeed
	}{
		{"complete", table + `, PRIMARY KEY (entity, seq)); CREATE UNIQUE INDEX governed_events_append_id ON %s.governed_events (entity, append_id);`, ""},
		{"no_seq_uniqueness", table + `); CREATE UNIQUE INDEX governed_events_append_id ON %s.governed_events (entity, append_id);`, "(entity, seq)"},
		{"append_id_index_not_unique", table + `, PRIMARY KEY (entity, seq)); CREATE INDEX governed_events_append_id ON %s.governed_events (entity, append_id);`, "(entity, append_id)"},
		{"append_id_index_on_other_columns", table + `, PRIMARY KEY (entity, seq)); CREATE UNIQUE INDEX governed_events_append_id ON %s.governed_events (append_id);`, "(entity, append_id)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, schema, dsn := migrationSchema(t, false)
			if _, err := db.Exec(strings.ReplaceAll(tc.ddl, "%s", schema)); err != nil {
				t.Fatal(err)
			}
			l, err := postgreslog.Open(ctx, dsn)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Open = %v, want it to accept the table", err)
				}
				l.Close()
				return
			}
			if err == nil {
				l.Close()
				t.Fatalf("Open accepted a table without a unique %s", tc.want)
			}
			if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open = %v, want an ErrConfig naming %s", err, tc.want)
			}
		})
	}
}
