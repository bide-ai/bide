package postgres

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// v070Journal is run "order-1" as v0.7.0 journaled it: finished, with one side-effect call. v0.7.0
// used the same table, bide_steps, with no journal format header.
var v070Journal = []struct{ name, data string }{
	{"@llm/0", `{"name":"@llm/0","kind":"model","message":{"role":"assistant","parts":[{"args":{},"id":"c1","name":"charge","type":"tool_use"}]},"usage":{},"salt":"mCApESbGjftukMDAtkPYaqiIGljQ42Sv2SH74L6F+HA="}`},
	{"attempt:c1", `{"name":"attempt:c1","kind":"attempt","tool_use_id":"c1","attempted_at":1790746461444,"claim":"5608d3daa98f802ac851ca33910d95c3","salt":"wfNQU6UpZN8xXOf2h1oEx/I7phCBQJp8iRR4S26RLEY="}`},
	{"c1", `{"name":"c1","kind":"tool_result","tool_use_id":"c1","result":"charged","salt":"p7HmBop+8IkzXoZhjF7nPLPyTqM8g+KQ0s14Hl6qJLw="}`},
	{"@llm/1", `{"name":"@llm/1","kind":"model","message":{"role":"assistant","parts":[{"text":"done","type":"text"}]},"usage":{},"salt":"FYkdumZelqPBoeXo4ndduNFE7g3dndWUo4Rzt0xjQJY="}`},
	{"run:complete", `{"name":"run:complete","kind":"value","salt":"la2ayUqmpioz+dAcVITVLHXZyG44NJmYPP7SkctvK58="}`},
}

// A v0.7.0 journal in the same table is refused before anything is read from it or written to
// it: re-invoking the finished run fires nothing and adds nothing to the run. Skips without PG_DSN.
func TestPostgres_V070JournalIsRefusedWithoutAWrite(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The table as v0.7.0 created it.
	if _, err := db.ExecContext(ctx, `CREATE TABLE bide_steps (
		run_id text NOT NULL, seq bigint NOT NULL, name text NOT NULL, data bytea NOT NULL,
		PRIMARY KEY (run_id, name), UNIQUE (run_id, seq))`); err != nil {
		t.Fatal(err)
	}
	for i, r := range v070Journal {
		if _, err := db.ExecContext(ctx, `INSERT INTO bide_steps VALUES ('order-1', $1, $2, $3)`, i, r.name, []byte(r.data)); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(s)
	defer s.Close()
	names := func() (out []string) {
		for e, err := range s.Load(ctx, "order-1", -1) {
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, e.Name)
		}
		return out
	}
	before := names()
	fired := 0
	charge := agent.Func("charge", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { fired++; return "charged", nil })
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "charge", `{}`), agent.TextTurn("done"))
	_, err = agenttest.MustNew(m, j, agent.WithTools(charge)).Run(ctx, "order-1", agent.UserText("charge me"))
	if fired != 0 || !errors.Is(err, agent.ErrJournalVersion) {
		t.Fatalf("Run of a v0.7.0 run = %v, fired %d; want ErrJournalVersion and nothing fired", err, fired)
	}
	if after := names(); !slices.Equal(after, before) {
		t.Errorf("the refused v0.7.0 run was written to: %v, then %v", before, after)
	}
}
