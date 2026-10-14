package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// column is one column the store's statements depend on, as the catalog reports it.
type column struct {
	name, typ string // typ as pg_catalog.format_type prints it
}

// The columns of each table, from the migration. Every one is NOT NULL.
var (
	stepsColumns   = []column{{"run_id", "text"}, {"seq", "bigint"}, {"name", "text"}, {"data", "bytea"}}
	leasesColumns  = []column{{"run_id", "text"}, {"holder", "text"}, {"expiry", "timestamp with time zone"}}
	versionColumns = []column{{"id", "integer"}, {"version", "integer"}}
	eventsColumns  = []column{{"run_id", "text"}, {"seq", "bigint"}, {"leaf", "bytea"}}
)

// tableColumns returns, for the live columns of table $1 in schema $2 in order: the name, the type,
// whether it is NOT NULL, whether it has a default or is generated (so an insert that does not
// name it still works), and whether its collation is nondeterministic (a case-insensitive
// collation would make equal run IDs of different bytes the same key).
const tableColumns = `SELECT a.attname::pg_catalog.text, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull,
		a.atthasdef OR a.attgenerated OPERATOR(pg_catalog.<>) '' OR a.attidentity OPERATOR(pg_catalog.<>) '',
		EXISTS (SELECT 1 FROM pg_catalog.pg_collation AS co WHERE co.oid OPERATOR(pg_catalog.=) a.attcollation AND NOT co.collisdeterministic)
	FROM pg_catalog.pg_attribute AS a
	WHERE a.attrelid OPERATOR(pg_catalog.=) (SELECT c.oid FROM pg_catalog.pg_class AS c
			WHERE c.relname OPERATOR(pg_catalog.=) $1
				AND c.relnamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2))
		AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped
	ORDER BY a.attnum`

// tableHooks returns, for table $1 in schema $2: whether it has a trigger of its own (a constraint
// trigger the server keeps for a foreign key is internal and does not count), whether it has a
// rule, and whether row-level security is enabled on it. Each can change or hide what a statement
// writes or reads without the statement saying so.
const tableHooks = `SELECT
		EXISTS (SELECT 1 FROM pg_catalog.pg_trigger AS t WHERE t.tgrelid OPERATOR(pg_catalog.=) c.oid AND NOT t.tgisinternal),
		EXISTS (SELECT 1 FROM pg_catalog.pg_rewrite AS r WHERE r.ev_class OPERATOR(pg_catalog.=) c.oid),
		c.relrowsecurity
	FROM pg_catalog.pg_class AS c
	WHERE c.relname OPERATOR(pg_catalog.=) $1
		AND c.relnamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2)`

// indexDefinition returns, for the index $1 in schema $2 on the table $3 in the same schema, no
// row when it is not an index on that table; else its access method, whether it is valid, unique,
// partial or on an expression, how many key columns it has, the name of its first key column
// (NULL for an expression) and the name of that key's collation.
const indexDefinition = `SELECT am.amname::pg_catalog.text, i.indisvalid, i.indisunique,
		i.indpred IS NOT NULL OR i.indexprs IS NOT NULL, i.indnkeyatts::pg_catalog.int8,
		(SELECT a.attname::pg_catalog.text FROM pg_catalog.pg_attribute AS a WHERE a.attrelid OPERATOR(pg_catalog.=) i.indrelid AND a.attnum OPERATOR(pg_catalog.=) i.indkey[0]),
		(SELECT co.collname::pg_catalog.text FROM pg_catalog.pg_collation AS co WHERE co.oid OPERATOR(pg_catalog.=) i.indcollation[0])
	FROM pg_catalog.pg_class AS x
		JOIN pg_catalog.pg_index AS i ON i.indexrelid OPERATOR(pg_catalog.=) x.oid
		JOIN pg_catalog.pg_am AS am ON am.oid OPERATOR(pg_catalog.=) x.relam
	WHERE x.relname OPERATOR(pg_catalog.=) $1 AND x.relkind OPERATOR(pg_catalog.=) 'i'
		AND x.relnamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2)
		AND i.indrelid OPERATOR(pg_catalog.=) (SELECT c.oid FROM pg_catalog.pg_class AS c
			WHERE c.relname OPERATOR(pg_catalog.=) $3 AND c.relnamespace OPERATOR(pg_catalog.=) x.relnamespace)`

// checkTable checks that table has the columns want (by name, type and NOT NULL, in any order,
// with a deterministic collation), that every other column is nullable or has a default (so the
// store's inserts still work), and that no trigger, rule or row-level security policy sits on it.
// The migration never alters a table, so it cannot repair any of this.
func (s *Store) checkTable(ctx context.Context, table string, want []column) error {
	rows, err := s.db.QueryContext(ctx, tableColumns, table, s.schema)
	if err != nil {
		return fmt.Errorf("postgres: check the columns of %s: %w (%w)", table, err, agent.ErrStorage)
	}
	defer rows.Close()
	have := map[string]column{}
	for rows.Next() {
		var c column
		var notNull, defaulted, nondeterministic bool
		if err := rows.Scan(&c.name, &c.typ, &notNull, &defaulted, &nondeterministic); err != nil {
			return fmt.Errorf("postgres: check the columns of %s: %w (%w)", table, err, agent.ErrStorage)
		}
		if i := indexOfColumn(want, c.name); i >= 0 {
			have[c.name] = c
			switch {
			case c.typ != want[i].typ:
				return badColumn(s.schema, table, c.name, "has type "+c.typ+", not "+want[i].typ)
			case !notNull:
				return badColumn(s.schema, table, c.name, "is nullable, not NOT NULL")
			case nondeterministic:
				return badColumn(s.schema, table, c.name, "has a nondeterministic collation, under which different bytes can be the same key")
			}
		} else if notNull && !defaulted {
			return badColumn(s.schema, table, c.name, "is an extra NOT NULL column with no default, which the store's inserts do not set")
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgres: check the columns of %s: %w (%w)", table, err, agent.ErrStorage)
	}
	for _, w := range want {
		if _, ok := have[w.name]; !ok {
			return badColumn(s.schema, table, w.name, "does not exist")
		}
	}
	var trigger, rule, rls bool
	if err := s.db.QueryRowContext(ctx, tableHooks, table, s.schema).Scan(&trigger, &rule, &rls); err != nil {
		return fmt.Errorf("postgres: check %s for triggers and rules: %w (%w)", table, err, agent.ErrStorage)
	}
	for what, found := range map[string]bool{"a trigger": trigger, "a rule": rule, "row-level security": rls} {
		if found {
			return fmt.Errorf("postgres: table %s.%s has %s, which could change or hide what the store's statements write or read; the store never alters a table and refuses it (audit with pgaudit or a separate table instead): %w",
				quoteIdent(s.schema), table, what, agent.ErrConfig)
		}
	}
	return nil
}

func indexOfColumn(cols []column, name string) int {
	for i, c := range cols {
		if c.name == name {
			return i
		}
	}
	return -1
}

func badColumn(schema, table, col, why string) error {
	return fmt.Errorf("postgres: column %s of %s.%s %s; the store's statements depend on the definition the migration creates, and the store never alters a table: %w",
		col, quoteIdent(schema), table, why, agent.ErrConfig)
}

// checkIndexes checks that each lease index is a valid, plain btree index on the leases table with
// the key and collation the migration creates; a relation of that name that is something else (a
// table, an index on another table) is refused. An index that is missing is not checked here (see
// verify).
func (s *Store) checkIndexes(ctx context.Context) error {
	for _, ix := range leaseIndexes(s.t) {
		var am string
		var valid, unique, partial bool
		var keys int64
		var key, coll sql.NullString
		err := s.db.QueryRowContext(ctx, indexDefinition, ix.name, s.schema, s.t.leases).Scan(&am, &valid, &unique, &partial, &keys, &key, &coll)
		if err == sql.ErrNoRows {
			return fmt.Errorf("postgres: %s.%s is not an index on %s; the migration skips an index whose name is taken and the store never alters the schema: %w",
				quoteIdent(s.schema), ix.name, s.t.leases, agent.ErrConfig)
		}
		if err != nil {
			return fmt.Errorf("postgres: check index %s: %w (%w)", ix.name, err, agent.ErrStorage)
		}
		if am != "btree" || !valid || unique || partial || keys != 1 || key.String != ix.key || coll.String != ix.collation {
			return fmt.Errorf("postgres: index %s.%s is not the one the migration creates (a valid, non-unique, non-partial btree on %s%s); the store's lapsed listing depends on it: %w",
				quoteIdent(s.schema), ix.name, ix.key, collationNote(ix.collation), agent.ErrConfig)
		}
	}
	return nil
}

func collationNote(c string) string {
	if c == "" {
		return ""
	}
	return ` under collation "` + c + `"`
}
