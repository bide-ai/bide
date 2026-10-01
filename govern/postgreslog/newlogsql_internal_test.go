package postgreslog

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// newLogSQL allows next_seq only under its exact schema-qualified name, once, in an INSERT into the
// log's own governed_events with no SELECT, and every statement only if it passes the statement
// check.
func TestNewLogSQL(t *testing.T) {
	ns, err := parseName(`"app".governed_events_next_seq_v1`)
	if err != nil {
		t.Fatal(err)
	}
	into, err := parseName(`"app".governed_events`)
	if err != nil {
		t.Fatal(err)
	}
	const ok = `INSERT INTO "app".governed_events (entity, seq, event, append_id) VALUES ($1::pg_catalog.text, "app".governed_events_next_seq_v1($1::pg_catalog.text), $2, $3) ON CONFLICT (entity, append_id) DO NOTHING RETURNING seq`
	if got, err := newLogSQL(ok, ns, into); err != nil || string(got) != ok {
		t.Errorf("newLogSQL(%q) = %q, %v", ok, got, err)
	}
	for _, q := range []string{
		`INSERT INTO "app".other (entity, seq) VALUES ($1, "app".governed_events_next_seq_v1($1))`,
		`INSERT INTO "other".governed_events (entity, seq) VALUES ($1, "app".governed_events_next_seq_v1($1))`,
		`INSERT INTO "app".governed_events (entity, seq) SELECT entity, "app".governed_events_next_seq_v1(entity) FROM "app".governed_events`,
		`INSERT INTO "app".governed_events (entity, seq, event) VALUES ($1, "app".governed_events_next_seq_v1($1), "app".governed_events_next_seq_v1($1))`,
		`INSERT INTO "app".governed_events (entity, seq) VALUES ($1, governed_events_next_seq_v1($1))`,
		`SELECT "app".governed_events_next_seq_v1($1)`,
		`UPDATE "app".governed_events SET seq = 0`,
	} {
		if _, err := newLogSQL(q, ns, into); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("newLogSQL(%q) = %v, want ErrConfig", q, err)
		}
	}
	if _, err := newLogSQL(`SELECT "app".governed_events_next_seq_v1($1)`, nil, nil); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("newLogSQL accepted next_seq in a SELECT with no next_seq allowed: %v", err)
	}
}
