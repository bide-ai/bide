package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
// "not_terminal", "leased", "parent_live". The Go side (agent.CheckRedaction) makes the same checks
// first, so the function is the second layer and the only one a role with raw SQL meets.
// TestRedactFunctionMatchesGo pins its classification of record names to agent.RedactionProtected.
//
// Like next_seq, it is created if missing and never replaced (redactVersion), runs with a pinned
// search_path, and Open checks its definition when it exists. It is optional: a schema migrated
// before it existed has none, the application's role never needs it, and Redact on such a schema
// says to run the migration as the schema's owner.
const redactVersion = "v1"

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
	IF pg_catalog.left(n, 4) = 'run:' AND pg_catalog.left(n, 9) <> 'run:start' THEN
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
			AND NOT EXISTS (SELECT 1 FROM %[1]s AS m WHERE m.run_id = a.run_id AND m.name = ANY (ends)));
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
	RETURN 'changed';
END
`

// redactConfig is the setting the redaction function runs with, as pg_proc.proconfig stores it.
const redactConfig = "search_path=pg_catalog, pg_temp"

// redactBody returns the redaction function's source for the tables in schema.
func (t tables) redactBody(schema string) string {
	q := quoteIdent(schema) + "."
	return fmt.Sprintf(redactTemplate, q+t.steps, q+t.leases)
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
		return nil
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
	return run("restrict "+s.t.redact, `REVOKE ALL ON FUNCTION `+s.t.qRedact+`(pg_catalog.text, pg_catalog.text, pg_catalog.bytea, pg_catalog.int8) FROM PUBLIC`)
}

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
	case "parent_live":
		return false, fmt.Errorf("run %s: %w", runID, agent.ErrRedactParentLive)
	case "changed":
		return false, fmt.Errorf("run %s, entry %q: %w", runID, name, agent.ErrRedactChanged)
	}
	return false, fmt.Errorf("redact step %q: the redaction function returned %q: %w", name, code, agent.ErrStorage)
}
