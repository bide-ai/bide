package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// column is one column the store's statements depend on, as the catalog reports it.
type column struct {
	name, typ string // typ as pg_catalog.format_type prints it
}

// tableSpec is a table as the migration creates it: its columns (every one NOT NULL), the column
// sets of its unique indexes, and its CHECK constraints (as normalizeCheck prints them).
type tableSpec struct {
	columns []column
	uniques [][]string
	checks  []string
}

var (
	stepsSpec = tableSpec{
		columns: []column{{"run_id", "text"}, {"seq", "bigint"}, {"name", "text"}, {"data", "bytea"}},
		uniques: [][]string{{"name", "run_id"}, {"run_id", "seq"}}}
	leasesSpec = tableSpec{
		columns: []column{{"run_id", "text"}, {"holder", "text"}, {"expiry", "timestamp with time zone"}},
		uniques: [][]string{{"run_id"}}}
	versionSpec = tableSpec{
		columns: []column{{"id", "integer"}, {"version", "integer"}},
		uniques: [][]string{{"id"}}, checks: []string{"CHECKid=1"}}
	eventsSpec = tableSpec{
		columns: []column{{"run_id", "text"}, {"seq", "bigint"}, {"leaf", "bytea"}},
		uniques: [][]string{{"run_id", "seq"}}, checks: []string{"CHECKseq>=0"}}
)

// tableColumns returns, for the live columns of table $1 in schema $2 in order: the name, the type,
// whether it is NOT NULL, whether it has a default, whether it is generated or an identity column, and whether its collation is nondeterministic (a case-insensitive
// collation would make equal run IDs of different bytes the same key).
const tableColumns = `SELECT a.attname::pg_catalog.text, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull,
		a.atthasdef, a.attgenerated OPERATOR(pg_catalog.<>) '' OR a.attidentity OPERATOR(pg_catalog.<>) '',
		EXISTS (SELECT 1 FROM pg_catalog.pg_collation AS co WHERE co.oid OPERATOR(pg_catalog.=) a.attcollation AND NOT co.collisdeterministic)
	FROM pg_catalog.pg_attribute AS a
	WHERE a.attrelid OPERATOR(pg_catalog.=) (SELECT c.oid FROM pg_catalog.pg_class AS c
			WHERE c.relname OPERATOR(pg_catalog.=) $1
				AND c.relnamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2))
		AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped
	ORDER BY a.attnum`

// tableHooks returns, for table $1 in schema $2: whether it has a trigger of its own (a constraint
// trigger the server keeps for a foreign key is internal and does not count), whether it has a
// rule, and whether row-level security is enabled on it, and whether it inherits from or is inherited by another table (a
// partition counts). Each can change or hide what a statement
// writes or reads without the statement saying so.
const tableHooks = `SELECT
		EXISTS (SELECT 1 FROM pg_catalog.pg_trigger AS t WHERE t.tgrelid OPERATOR(pg_catalog.=) c.oid AND NOT t.tgisinternal),
		EXISTS (SELECT 1 FROM pg_catalog.pg_rewrite AS r WHERE r.ev_class OPERATOR(pg_catalog.=) c.oid),
		c.relrowsecurity,
		EXISTS (SELECT 1 FROM pg_catalog.pg_inherits AS h WHERE h.inhrelid OPERATOR(pg_catalog.=) c.oid OR h.inhparent OPERATOR(pg_catalog.=) c.oid)
	FROM pg_catalog.pg_class AS c
	WHERE c.relname OPERATOR(pg_catalog.=) $1
		AND c.relnamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2)`

// tableConstraints returns the constraints of table $1 in schema $2 besides primary keys, unique
// constraints and NOT NULL ones: their type and definition.
const tableConstraints = `SELECT k.contype::pg_catalog.text, pg_catalog.pg_get_constraintdef(k.oid)
	FROM pg_catalog.pg_constraint AS k
	WHERE k.conrelid OPERATOR(pg_catalog.=) (SELECT c.oid FROM pg_catalog.pg_class AS c
			WHERE c.relname OPERATOR(pg_catalog.=) $1
				AND c.relnamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2))
		AND k.contype OPERATOR(pg_catalog.<>) 'p' AND k.contype OPERATOR(pg_catalog.<>) 'u' AND k.contype OPERATOR(pg_catalog.<>) 'n'`

// tableUniques returns one row per key column of each unique index on table $1 in schema $2: the
// index, the column's name (NULL for an expression), and whether the index is partial or on an
// expression.
const tableUniques = `SELECT i.indexrelid::pg_catalog.int8, a.attname::pg_catalog.text, i.indpred IS NOT NULL OR i.indexprs IS NOT NULL
	FROM pg_catalog.pg_index AS i
		CROSS JOIN pg_catalog.unnest(i.indkey::pg_catalog.int2[]) AS k
		LEFT JOIN pg_catalog.pg_attribute AS a ON a.attrelid OPERATOR(pg_catalog.=) i.indrelid AND a.attnum OPERATOR(pg_catalog.=) k
	WHERE i.indisunique AND i.indrelid OPERATOR(pg_catalog.=) (SELECT c.oid FROM pg_catalog.pg_class AS c
			WHERE c.relname OPERATOR(pg_catalog.=) $1
				AND c.relnamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2))
	ORDER BY i.indexrelid`

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

// checkTable checks that table is as spec says: the columns by name, type and NOT NULL, in any
// order, none generated or an identity and none with a nondeterministic collation; every other
// column nullable or defaulted (so the store's inserts still work); no unique index but the
// expected ones; no CHECK, EXCLUDE or foreign-key constraint but the expected ones; and no trigger,
// rule, row-level security, partition or inheritance on it. The migration never alters a table, so
// it cannot repair any of this.
func (s *Store) checkTable(ctx context.Context, table string, spec tableSpec) error {
	want := spec.columns
	rows, err := s.db.QueryContext(ctx, tableColumns, table, s.schema)
	if err != nil {
		return fmt.Errorf("postgres: check the columns of %s: %w (%w)", table, err, agent.ErrStorage)
	}
	defer rows.Close()
	have := map[string]column{}
	for rows.Next() {
		var c column
		var notNull, defaulted, generated, nondeterministic bool
		if err := rows.Scan(&c.name, &c.typ, &notNull, &defaulted, &generated, &nondeterministic); err != nil {
			return fmt.Errorf("postgres: check the columns of %s: %w (%w)", table, err, agent.ErrStorage)
		}
		if i := indexOfColumn(want, c.name); i >= 0 {
			have[c.name] = c
			switch {
			case c.typ != want[i].typ:
				return badColumn(s.schema, table, c.name, "has type "+c.typ+", not "+want[i].typ)
			case !notNull:
				return badColumn(s.schema, table, c.name, "is nullable, not NOT NULL")
			case generated:
				return badColumn(s.schema, table, c.name, "is a generated or identity column, which would override what the store writes")
			case nondeterministic:
				return badColumn(s.schema, table, c.name, "has a nondeterministic collation, under which different bytes can be the same key")
			}
		} else if notNull && !defaulted && !generated {
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
	if err := s.checkConstraints(ctx, table, spec); err != nil {
		return err
	}
	var trigger, rule, rls, inherits bool
	if err := s.db.QueryRowContext(ctx, tableHooks, table, s.schema).Scan(&trigger, &rule, &rls, &inherits); err != nil {
		return fmt.Errorf("postgres: check %s for triggers and rules: %w (%w)", table, err, agent.ErrStorage)
	}
	for _, h := range []struct {
		what  string
		found bool
	}{{"a trigger", trigger}, {"a rule", rule}, {"row-level security", rls}, {"a partition or inheritance", inherits}} {
		if h.found {
			return fmt.Errorf("postgres: table %s.%s has %s, which could change or hide what the store's statements write or read; the store never alters a table and refuses it (it must be an ordinary table; audit with pgaudit or a separate table instead): %w",
				quoteIdent(s.schema), table, h.what, agent.ErrConfig)
		}
	}
	return nil
}

// checkConstraints refuses a CHECK, EXCLUDE, foreign-key or constraint-trigger constraint beyond
// spec.checks, and a unique index (a constraint's or a plain one) whose columns are not one of
// spec.uniques or that is partial or on an expression: any of them could reject a row the
// store's statements write.
func (s *Store) checkConstraints(ctx context.Context, table string, spec tableSpec) error {
	rows, err := s.db.QueryContext(ctx, tableConstraints, table, s.schema)
	if err != nil {
		return fmt.Errorf("postgres: check the constraints of %s: %w (%w)", table, err, agent.ErrStorage)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, def string
		if err := rows.Scan(&kind, &def); err != nil {
			return fmt.Errorf("postgres: check the constraints of %s: %w (%w)", table, err, agent.ErrStorage)
		}
		if kind != "c" || !slices.Contains(spec.checks, normalizeCheck(def)) {
			return fmt.Errorf("postgres: table %s.%s has the constraint %s, which the migration does not create and which could reject what the store writes; the store never alters a table: %w",
				quoteIdent(s.schema), table, def, agent.ErrConfig)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgres: check the constraints of %s: %w (%w)", table, err, agent.ErrStorage)
	}
	urows, err := s.db.QueryContext(ctx, tableUniques, table, s.schema)
	if err != nil {
		return fmt.Errorf("postgres: check the unique indexes of %s: %w (%w)", table, err, agent.ErrStorage)
	}
	defer urows.Close()
	cols := map[int64][]string{}
	odd := map[int64]bool{} // partial, or with an expression among its keys
	for urows.Next() {
		var id int64
		var name sql.NullString
		var partial bool
		if err := urows.Scan(&id, &name, &partial); err != nil {
			return fmt.Errorf("postgres: check the unique indexes of %s: %w (%w)", table, err, agent.ErrStorage)
		}
		cols[id] = append(cols[id], name.String)
		odd[id] = odd[id] || partial || !name.Valid
	}
	if err := urows.Err(); err != nil {
		return fmt.Errorf("postgres: check the unique indexes of %s: %w (%w)", table, err, agent.ErrStorage)
	}
	for id, c := range cols {
		slices.Sort(c)
		if odd[id] || !slices.ContainsFunc(spec.uniques, func(u []string) bool { return slices.Equal(u, c) }) {
			return fmt.Errorf("postgres: table %s.%s has a unique index on (%s) that the migration does not create (or that is partial or on an expression), which could reject what the store writes; the store never alters a table: %w",
				quoteIdent(s.schema), table, strings.Join(c, ", "), agent.ErrConfig)
		}
	}
	return nil
}

// normalizeCheck is a CHECK constraint's definition without spaces and parentheses, so that
// "CHECK ((seq >= 0))" is "CHECKseq>=0".
func normalizeCheck(def string) string {
	def = operatorForm.ReplaceAllString(def, "$1") // the server prints OPERATOR(pg_catalog.=) when the search path could shadow it
	return strings.NewReplacer(" ", "", "(", "", ")", "").Replace(def)
}

var operatorForm = regexp.MustCompile(`OPERATOR\(pg_catalog\.([^)]+)\)`)

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
