package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// The redaction function. Erasing a record is the one write to an existing journal row, and the role
// that does it must not be able to write anything else, so the checks cannot live in the client
// alone: a role with UPDATE on the steps table could run `UPDATE ... SET data = ...` and rewrite
// any record of any run. So the role that erases (bide_redactor) holds no privilege on the tables.
// It holds EXECUTE on this SECURITY DEFINER function, which the schema's owner creates, and the
// function re-checks everything server-side: the run is over (an end marker), no lease on it is
// live, no run above a sub-agent's run is still being driven, it is not a session's run, the record
// is not one a redaction never replaces, the stored bytes are the bytes the caller read, and the
// tombstone is built here from those bytes (SHA-256 over the journal leaf tag and the stored bytes),
// never taken from the caller.
//
// It returns a code: "replaced", "already" (the entry already holds the redaction of these bytes),
// or the reason it replaced nothing: "session", "mismatch", "protected", "no_entry", "changed",
// "not_terminal", "leased", "parent_live", "held", "isolation". The Go side (agent.CheckRedaction) makes the same checks
// first, so the function is the second layer and the only one a role with raw SQL meets.
// TestRedactFunctionMatchesGo pins its classification of record names to agent.RedactionProtected.
//
// Like next_seq, it is created if missing and never replaced (redactVersion), runs with a pinned
// search_path, and Open checks its definition when it exists. It is optional: a schema migrated
// before it existed has none, the application's role never needs it, and Redact on such a schema
// says to run the migration as the schema's owner.
const redactVersion = "v3"

// redactTemplate is the body of the redaction function, over the steps table %[1]s and the leases
// table %[2]s (qualified with their schema). The function sets search_path to pg_catalog, pg_temp
// (redactConfig), so the plain operators and every unqualified name below resolve in pg_catalog;
// the tables are qualified.
const redactTemplate = `
DECLARE
	segs pg_catalog.text[] := pg_catalog.string_to_array(r, '>');
	nseg pg_catalog.int4 := COALESCE(pg_catalog.array_length(segs, 1), 0);
	ancestors pg_catalog.text[] := '{}';
	ends pg_catalog.text[] := ARRAY['run:complete', 'run:aborted', 'run:cancelled'];
	prot pg_catalog.text[] := ARRAY['@journal', '@saga/', '@subrun/', 'attempt:', 'approval-tally:', 'approval-identity:',
		'approval-status:', 'approval-exec-hold:', 'approval-deadline:', 'turn/', 'start/', 'from/', 'audit:', 'flow:'];
	p pg_catalog.text;
	i pg_catalog.int4;
	doc pg_catalog.jsonb;
	kind pg_catalog.text;
	hex pg_catalog.text;
	tomb pg_catalog.bytea;
	stored pg_catalog.bytea;
	nrows pg_catalog.int8;
BEGIN
	-- Each statement below must see every hold that has committed. At READ COMMITTED a statement takes
	-- its snapshot when it starts, so taking the holds table's SHARE lock first (a hold's INSERT or
	-- DELETE waits for it, and this waits for the ones in flight) makes the UPDATE's snapshot include
	-- every hold committed before the lock was granted. A snapshot taken earlier, by a caller at
	-- REPEATABLE READ or SERIALIZABLE, would not: refuse it.
	IF pg_catalog.current_setting('transaction_isolation') <> 'read committed' THEN
		RETURN 'isolation';
	END IF;
	LOCK TABLE %[3]s IN SHARE MODE;
	FOR i IN 2..nseg LOOP
		IF pg_catalog.left(segs[i], 1) = '@' THEN
			RETURN 'session';
		END IF;
	END LOOP;
	IF ms < 0 OR ms > 253402300799999 THEN
		RETURN 'mismatch';
	END IF;
	IF pg_catalog.substr(cur, 1, 8) <> '\x7b226e616d65223a'::pg_catalog.bytea THEN
		RETURN 'protected';
	END IF;
	BEGIN
		doc := pg_catalog.convert_from(cur, 'UTF8')::pg_catalog.jsonb;
	EXCEPTION WHEN OTHERS THEN
		RETURN 'protected';
	END;
	IF doc->>'name' IS DISTINCT FROM n THEN
		RETURN 'mismatch';
	END IF;
	kind := doc->>'kind';
	IF kind IN ('header', 'attempt', 'not_started', 'saga_fail') THEN
		RETURN 'protected';
	END IF;
	IF pg_catalog.left(n, 4) = 'run:' AND pg_catalog.left(n, 9) <> 'run:start' AND n <> ALL (ends) THEN
		RETURN 'protected';
	END IF;
	FOREACH p IN ARRAY prot LOOP
		IF pg_catalog.left(n, pg_catalog.length(p)) = p THEN
			RETURN 'protected';
		END IF;
	END LOOP;
	FOR i IN 1..nseg - 1 LOOP
		ancestors := ancestors || pg_catalog.array_to_string(segs[1:i], '>');
	END LOOP;
	hex := pg_catalog.encode(pg_catalog.sha256('\x00'::pg_catalog.bytea || pg_catalog.convert_to('bide.audit.journal-leaf.v1', 'UTF8')
		|| '\x00'::pg_catalog.bytea || cur), 'hex');
	tomb := pg_catalog.convert_to('{"redacted":{"leaf_hash":"' || hex || '","at_ms":' || ms::pg_catalog.text
		|| ',"name":' || pg_catalog.to_json(n)::pg_catalog.text || '}}', 'UTF8');
	UPDATE %[1]s AS s SET data = tomb
		WHERE s.run_id = r AND s.name = n AND s.data = cur
		AND EXISTS (SELECT 1 FROM %[1]s AS e WHERE e.run_id = r AND e.name = ANY (ends))
		AND NOT EXISTS (SELECT 1 FROM %[2]s AS l WHERE l.run_id = r AND l.expiry >= pg_catalog.now())
		AND NOT EXISTS (SELECT 1 FROM %[1]s AS a WHERE a.run_id = ANY (ancestors)
			AND NOT EXISTS (SELECT 1 FROM %[1]s AS m WHERE m.run_id = a.run_id AND m.name = ANY (ends)))
		AND NOT EXISTS (SELECT 1 FROM %[3]s AS h WHERE h.run_id = r OR (h.tree AND h.run_id = ANY (ancestors)));
	GET DIAGNOSTICS nrows = ROW_COUNT;
	IF nrows = 1 THEN
		RETURN 'replaced';
	END IF;
	SELECT s.data INTO stored FROM %[1]s AS s WHERE s.run_id = r AND s.name = n;
	IF NOT FOUND THEN
		RETURN 'no_entry';
	END IF;
	IF stored <> cur THEN
		IF position(('"leaf_hash":"' || hex || '"')::pg_catalog.bytea IN stored) > 0
			AND pg_catalog.substr(stored, 1, 13) = '{"redacted":{'::pg_catalog.bytea THEN
			RETURN 'already';
		END IF;
		RETURN 'changed';
	END IF;
	IF NOT EXISTS (SELECT 1 FROM %[1]s AS e WHERE e.run_id = r AND e.name = ANY (ends)) THEN
		RETURN 'not_terminal';
	END IF;
	IF EXISTS (SELECT 1 FROM %[2]s AS l WHERE l.run_id = r AND l.expiry >= pg_catalog.now()) THEN
		RETURN 'leased';
	END IF;
	IF EXISTS (SELECT 1 FROM %[1]s AS a WHERE a.run_id = ANY (ancestors)
		AND NOT EXISTS (SELECT 1 FROM %[1]s AS m WHERE m.run_id = a.run_id AND m.name = ANY (ends))) THEN
		RETURN 'parent_live';
	END IF;
	IF EXISTS (SELECT 1 FROM %[3]s AS h WHERE h.run_id = r OR (h.tree AND h.run_id = ANY (ancestors))) THEN
		RETURN 'held';
	END IF;
	RETURN 'changed';
END
`

// redactConfig is the setting the redaction function runs with, as pg_proc.proconfig stores it.
const redactConfig = "search_path=pg_catalog, pg_temp"

// redactBody returns the redaction function's source for the tables in schema.
func (t tables) redactBody(schema string) string {
	q := quoteIdent(schema) + "."
	return fmt.Sprintf(redactTemplate, q+t.steps, q+t.leases, q+t.holds)
}

// redactPresent reports whether a function named $1 taking (text, text, bytea, bigint) exists in
// schema $2.
const redactPresent = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_proc AS p
	WHERE p.proname OPERATOR(pg_catalog.=) $1 AND p.proargtypes OPERATOR(pg_catalog.=) '25 25 17 20'::pg_catalog.oidvector
		AND p.pronamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2))`

// expectedRedactFunction returns, for the function $1(text, text, bytea, bigint) in schema $2,
// whether it is a VOLATILE SECURITY DEFINER plpgsql function returning text whose only setting is
// $3 and whose source is $4; and whether its owner is the owner of table $5. Each is NULL when
// there is no such function.
const expectedRedactFunction = `SELECT
		l.lanname OPERATOR(pg_catalog.=) 'plpgsql' AND p.provolatile OPERATOR(pg_catalog.=) 'v' AND p.prosecdef AND NOT p.proretset
			AND p.proconfig OPERATOR(pg_catalog.=) $3::pg_catalog.text[]
			AND p.prorettype OPERATOR(pg_catalog.=) 'pg_catalog.text'::pg_catalog.regtype::pg_catalog.oid
			AND p.prosrc OPERATOR(pg_catalog.=) $4,
		p.proowner OPERATOR(pg_catalog.=) (SELECT c.relowner FROM pg_catalog.pg_class AS c
			WHERE c.relname OPERATOR(pg_catalog.=) $5 AND c.relnamespace OPERATOR(pg_catalog.=) p.pronamespace)
	FROM pg_catalog.pg_proc AS p JOIN pg_catalog.pg_language AS l ON l.oid OPERATOR(pg_catalog.=) p.prolang
	WHERE p.proname OPERATOR(pg_catalog.=) $1 AND p.proargtypes OPERATOR(pg_catalog.=) '25 25 17 20'::pg_catalog.oidvector
		AND p.pronamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2)`

// migrateRedact creates the redaction function if it is missing (never replacing one), owned by the
// role that runs the migration, and withdraws EXECUTE from PUBLIC: only the owner and the roles it
// grants EXECUTE to (bide_redactor) can erase.
func (s *Store) migrateRedact(ctx context.Context, tx *sql.Tx, res *MigrationResult, run func(failure, stmt string) error) error {
	var have bool
	if err := tx.QueryRowContext(ctx, redactPresent, s.t.redact, s.schema).Scan(&have); err != nil {
		return fmt.Errorf("postgres: look up %s: %w (%w)", s.t.redact, err, agent.ErrStorage)
	}
	if have {
		return s.revokeOldRedact(ctx, tx, run)
	}
	res.Created = append(res.Created, "function "+quoteIdent(s.schema)+"."+s.t.redact+"(text, text, bytea, bigint)")
	body := s.t.redactBody(s.schema)
	if strings.Contains(body, "$bide$") {
		return fmt.Errorf("postgres: schema name %q holds $bide$: %w", s.schema, agent.ErrConfig)
	}
	sig := s.t.qRedact + `(r pg_catalog.text, n pg_catalog.text, cur pg_catalog.bytea, ms pg_catalog.int8)`
	if err := run("create "+s.t.redact, `CREATE FUNCTION `+sig+
		` RETURNS pg_catalog.text LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $bide$`+body+`$bide$`); err != nil {
		return err
	}
	if err := run("restrict "+s.t.redact, `REVOKE ALL ON FUNCTION `+s.t.qRedact+`(pg_catalog.text, pg_catalog.text, pg_catalog.bytea, pg_catalog.int8) FROM PUBLIC`); err != nil {
		return err
	}
	return s.revokeOldRedact(ctx, tx, run)
}

// revokeOldRedact withdraws EXECUTE on every older version of the redaction function (bide_redact_v1,
// v2, ... below redactVersion) from PUBLIC and from every role it was granted to explicitly, so only
// the current function can be called. An older version does not check what the current one does (v1
// reads no legal holds; v2 refuses an end marker the current one erases), and a role that can still
// call one can do what the current checks forbid or leave a record the current one would have erased:
// the migration never replaces or drops a function, but it can and does revoke. It runs on every
// migration (revoking again is harmless), and does nothing for a schema that never had one.
func (s *Store) revokeOldRedact(ctx context.Context, tx *sql.Tx, run func(failure, stmt string) error) error {
	prefix := strings.TrimSuffix(s.t.redact, redactVersion)
	cur, err := strconv.Atoi(strings.TrimPrefix(redactVersion, "v"))
	if err != nil {
		return fmt.Errorf("postgres: redaction function version %q: %w", redactVersion, agent.ErrConfig)
	}
	rows, err := tx.QueryContext(ctx, redactVersions, s.schema, prefix+"v%")
	if err != nil {
		return fmt.Errorf("postgres: look up the older %s versions: %w (%w)", prefix, err, agent.ErrStorage)
	}
	var older []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("postgres: look up the older %s versions: %w (%w)", prefix, err, agent.ErrStorage)
		}
		if rest, ok := strings.CutPrefix(name, prefix+"v"); ok {
			if n, err := strconv.Atoi(rest); err == nil && n < cur && strconv.Itoa(n) == rest {
				older = append(older, name)
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("postgres: look up the older %s versions: %w (%w)", prefix, err, agent.ErrStorage)
	}
	rows.Close()
	for _, old := range older {
		if err := s.revokeRedactVersion(ctx, tx, run, old); err != nil {
			return err
		}
	}
	return nil
}

// redactVersions lists the names, matching the LIKE pattern $2, of the functions taking (text, text,
// bytea, bigint) in schema $1.
const redactVersions = `SELECT p.proname::pg_catalog.text FROM pg_catalog.pg_proc AS p
	WHERE p.proname OPERATOR(pg_catalog.~~) $2 AND p.proargtypes OPERATOR(pg_catalog.=) '25 25 17 20'::pg_catalog.oidvector
		AND p.pronamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $1)
	ORDER BY 1`

// revokeRedactVersion withdraws EXECUTE on the function old from PUBLIC and from every role granted it.
func (s *Store) revokeRedactVersion(ctx context.Context, tx *sql.Tx, run func(failure, stmt string) error, old string) error {
	qold := quoteIdent(s.schema) + "." + quoteIdent(old) + `(pg_catalog.text, pg_catalog.text, pg_catalog.bytea, pg_catalog.int8)`
	if err := run("revoke "+old+" from PUBLIC", `REVOKE ALL ON FUNCTION `+qold+` FROM PUBLIC`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, grantedRedact, old, s.schema)
	if err != nil {
		return fmt.Errorf("postgres: look up the grants on %s: %w (%w)", old, err, agent.ErrStorage)
	}
	var grantees []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			rows.Close()
			return fmt.Errorf("postgres: look up the grants on %s: %w (%w)", old, err, agent.ErrStorage)
		}
		grantees = append(grantees, g)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("postgres: look up the grants on %s: %w (%w)", old, err, agent.ErrStorage)
	}
	rows.Close()
	for _, g := range grantees {
		if err := run("revoke "+old+" from "+g, `REVOKE ALL ON FUNCTION `+qold+` FROM `+g); err != nil {
			return err
		}
	}
	return nil
}

// grantedRedact lists, as SQL, the roles other than the owner that hold an explicit privilege on the
// function $1(text, text, bytea, bigint) in schema $2.
const grantedRedact = `SELECT pg_catalog.quote_ident(pg_catalog.pg_get_userbyid(a.grantee))
	FROM pg_catalog.pg_proc AS p CROSS JOIN LATERAL pg_catalog.aclexplode(p.proacl) AS a
	WHERE p.proname OPERATOR(pg_catalog.=) $1 AND p.proargtypes OPERATOR(pg_catalog.=) '25 25 17 20'::pg_catalog.oidvector
		AND p.pronamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2)
		AND a.grantee OPERATOR(pg_catalog.<>) 0 AND a.grantee OPERATOR(pg_catalog.<>) p.proowner`

// checkRedactFunction checks the redaction function's definition when the schema has one: a
// SECURITY DEFINER function is the one place the store runs another role's code, so a function of
// that name that is not the one this version creates, or that another role owns (and could replace
// after the check), is refused. A schema without it is fine.
func (s *Store) checkRedactFunction(ctx context.Context) error {
	var have bool
	if err := s.db.QueryRowContext(ctx, redactPresent, s.t.redact, s.schema).Scan(&have); err != nil {
		return fmt.Errorf("postgres: look up %s: %w (%w)", s.t.redact, err, agent.ErrStorage)
	}
	if !have {
		return nil
	}
	var same, owner sql.NullBool
	if err := s.db.QueryRowContext(ctx, expectedRedactFunction, s.t.redact, s.schema, []string{redactConfig}, s.t.redactBody(s.schema), s.t.steps).Scan(&same, &owner); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("postgres: check %s: %w (%w)", s.t.redact, err, agent.ErrStorage)
	}
	if !same.Bool {
		return fmt.Errorf("postgres: function %s.%s(text, text, bytea, bigint) is not the one this version creates (VOLATILE SECURITY DEFINER plpgsql returning text, with SET %s and no other setting, and the expected body); the migration never replaces a function: %w",
			quoteIdent(s.schema), s.t.redact, redactConfig, agent.ErrConfig)
	}
	if !owner.Bool {
		return fmt.Errorf("postgres: function %s.%s(text, text, bytea, bigint) has another owner than table %s: its owner could replace it, and it runs with the table owner's privilege; drop it, or have the tables' owner create it: %w",
			quoteIdent(s.schema), s.t.redact, s.t.steps, agent.ErrConfig)
	}
	return nil
}

// Redact implements agent.Redactor by calling the redaction function (see redactVersion), which
// replaces the entry only if it holds current, the run is over, no lease on it is live, no run above
// it is still being driven, and it is not a session's run, and builds the tombstone itself. The role
// that calls it needs EXECUTE on the function and nothing on the tables. The entry keeps its seq,
// so it does not contend with the run's inserts for a position.
func (s *Store) Redact(ctx context.Context, runID, name string, current, tombstone []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := agent.CheckRedaction(runID, name, current, tombstone); err != nil {
		return false, err
	}
	at, ok := agent.TombstoneMillis(tombstone)
	if !ok {
		return false, fmt.Errorf("run %s, entry %q: the tombstone has no time: %w", runID, name, agent.ErrRedactMismatch)
	}
	code, err := retry(ctx, func() (string, error) {
		var code string
		err := s.db.QueryRowContext(ctx, string(s.t.redactCall), runID, name, current, at).Scan(&code)
		return code, err
	})
	if err != nil {
		if sqlState(err) == "42883" || sqlState(err) == "3F000" { // undefined_function, invalid_schema_name
			return false, fmt.Errorf("redact step %q: the schema has no function %s: run the migration (bide migrate) as the schema's owner to create it, and grant EXECUTE on it to the redaction role: %w", name, s.t.redact, agent.ErrConfig)
		}
		return false, fmt.Errorf("redact step %q: %w (%w)", name, err, agent.ErrStorage)
	}
	switch code {
	case "replaced":
		return true, nil
	case "already":
		return false, nil
	case "session":
		return false, fmt.Errorf("run %s: %w", runID, agent.ErrRedactSession)
	case "mismatch":
		return false, fmt.Errorf("run %s, entry %q: %w", runID, name, agent.ErrRedactMismatch)
	case "protected":
		return false, fmt.Errorf("run %s, entry %q: %w", runID, name, agent.ErrRedactProtected)
	case "no_entry":
		return false, fmt.Errorf("run %s, entry %q: %w", runID, name, agent.ErrRedactNoEntry)
	case "not_terminal":
		return false, fmt.Errorf("run %s: %w", runID, agent.ErrRedactNotTerminal)
	case "leased":
		return false, fmt.Errorf("run %s: %w", runID, agent.ErrRedactLeased)
	case "isolation":
		return false, fmt.Errorf("redact step %q: the redaction function must run at READ COMMITTED (its transaction already took a snapshot, which would not see a hold placed meanwhile): set the erasing role's default_transaction_isolation to 'read committed' (ALTER ROLE ... SET default_transaction_isolation = 'read committed'): %w", name, agent.ErrConfig)
	case "held":
		return false, fmt.Errorf("run %s: %w", runID, agent.ErrRedactHeld)
	case "parent_live":
		return false, fmt.Errorf("run %s: %w", runID, agent.ErrRedactParentLive)
	case "changed":
		return false, fmt.Errorf("run %s, entry %q: %w", runID, name, agent.ErrRedactChanged)
	}
	return false, fmt.Errorf("redact step %q: the redaction function returned %q: %w", name, code, agent.ErrStorage)
}

// PlaceHold implements agent.Holder with one statement that inserts a row per run the hold names, only
// if no hold has the ID. Two placements of one ID at the same instant over disjoint runs could both
// land (the statement checks the ID, it does not lock it); the key on (hold_id, run_id) refuses an
// overlap. The placed time is this process's clock.
func (s *Store) PlaceHold(ctx context.Context, h agent.Hold) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := agent.CheckHold(h); err != nil {
		return err
	}
	n, err := s.write(ctx, s.t.placeHold, h.ID, h.Runs, h.Tree, h.Matter, time.Now().UnixMilli())
	if sqlState(err) == uniqueViolation {
		return fmt.Errorf("hold %q: %w", h.ID, agent.ErrHoldExists)
	}
	if err != nil {
		return fmt.Errorf("place hold %q: %w (%w)", h.ID, err, agent.ErrStorage)
	}
	if n == 0 {
		return fmt.Errorf("hold %q: %w", h.ID, agent.ErrHoldExists)
	}
	return nil
}

// LiftHold implements agent.Holder.
func (s *Store) LiftHold(ctx context.Context, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	n, err := s.write(ctx, s.t.liftHold, id)
	if err != nil {
		return false, fmt.Errorf("lift hold %q: %w (%w)", id, err, agent.ErrStorage)
	}
	return n > 0, nil
}

// Holds implements agent.Holder. A schema migrated before holds has no table and no hold.
func (s *Store) Holds(ctx context.Context) ([]agent.Hold, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var present sql.NullBool
	if err := retryScan(ctx, func() error {
		return s.db.QueryRowContext(ctx, relationIsTable, s.t.holds, s.schema).Scan(&present)
	}); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read the legal holds: %w (%w)", err, agent.ErrStorage)
	}
	if !present.Bool {
		return nil, nil
	}
	rows, err := retry(ctx, func() ([]agent.Hold, error) {
		r, err := s.db.QueryContext(ctx, string(s.t.listHolds))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		var out []agent.Hold
		for r.Next() {
			var id, run, matter string
			var tree bool
			var at int64
			if err := r.Scan(&id, &run, &tree, &matter, &at); err != nil {
				return nil, err
			}
			if len(out) == 0 || out[len(out)-1].ID != id {
				out = append(out, agent.Hold{ID: id, Matter: matter, Tree: tree, PlacedAtMs: at})
			}
			out[len(out)-1].Runs = append(out[len(out)-1].Runs, run)
		}
		return out, r.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("read the legal holds: %w (%w)", err, agent.ErrStorage)
	}
	return rows, nil
}
