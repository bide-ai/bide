package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/storetest"
)

// rv120cNames are pinned schema names that must round-trip: quotes, dots, mixed case, unicode,
// and names at and past Postgres's 63-byte identifier limit.
var rv120cNames = []string{
	`rv120c we"ird`,
	`rv120c.dotted`,
	`Rv120cMiXed`,
	`rv120c_схема_🚀`,
	"rv120c_" + strings.Repeat("a", 56), // 63 bytes, the limit
	"rv120c_" + strings.Repeat("b", 57), // 64 bytes, truncated by Postgres
	"rv120c_" + strings.Repeat("c", 80), // 87 bytes
	"rv120c_" + strings.Repeat("é", 30), // 67 bytes, multibyte truncation
	`rv120c -- /* $1 */ 'x' ; drop`,     // tokenizer bait inside a quoted name
}

func rv120cWork(t *testing.T, ctx context.Context, s *Store, tag string) {
	t.Helper()
	run := uniqueID(t, "rv120c-"+tag+"-")
	if _, _, err := s.Insert(ctx, run, "a", []byte("1")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if e, ok, err := s.Insert(ctx, run, "b", []byte("2")); err != nil || !ok || e.Seq != 1 {
		t.Fatalf("Insert b = %+v %v %v", e, ok, err)
	}
	if _, ok, err := s.Get(ctx, run, "b"); err != nil || !ok {
		t.Fatalf("Get = %v %v", ok, err)
	}
	n := 0
	for _, err := range s.Load(ctx, run, -1) {
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("Load read %d entries", n)
	}
	for _, f := range []agent.RunFilter{{Prefix: "rv120c-", ExcludeHolding: []string{"zz"}}, {Prefix: "rv120c-", ExcludeHolding: []string{"zz"}, LeaseLapsed: true}} {
		for _, err := range s.Runs(ctx, f) {
			if err != nil {
				t.Fatalf("Runs(%+v): %v", f, err)
			}
		}
	}
	if ok, err := s.AcquireLease(ctx, run, "h", time.Minute); err != nil || !ok {
		t.Fatalf("AcquireLease = %v %v", ok, err)
	}
	if ok, err := s.RenewLease(ctx, run, "h", time.Minute); err != nil || !ok {
		t.Fatalf("RenewLease = %v %v", ok, err)
	}
	if err := s.ReleaseLease(ctx, run, "h"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
}

// Every awkward pinned name opens, works, reopens, and puts the tables in exactly that schema.
func TestRV120c_PinnedNames(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	for _, name := range rv120cNames {
		t.Run(name, func(t *testing.T) {
			q := quoteIdent(name)
			admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+q+` CASCADE`)
			rv120Exec(t, admin, `CREATE SCHEMA `+q)
			t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+q+` CASCADE`) })
			for i := 0; i < 2; i++ {
				s, err := Open(ctx, rv120DSN(t, base, "", "public"), WithSchema(name))
				if err != nil {
					t.Fatalf("Open #%d WithSchema(%q) (%d bytes): %v", i+1, name, len(name), err)
				}
				rv120cWork(t, ctx, s, "n")
				s.Close()
			}
			var n int
			if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM `+q+`.bide_steps`).Scan(&n); err != nil || n != 4 {
				t.Fatalf("rows in %s.bide_steps = %d, %v; want 4", q, n, err)
			}
		})
	}
}

// Pinned mode never consults the search path: with a decoy schema first on the path that holds a
// view named bide_steps (which discovery refuses), a trap table set, and trap operators for every
// argument type the statements compare, a pinned Open, every call, and a reopen all succeed in
// the pinned schema and run nothing of the decoy's. The same holds with an empty search path.
func TestRV120c_PinnedIgnoresDecoyPath(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	rv120Schema(t, admin, "rv120capp")
	rv120Schema(t, admin, "rv120cdecoy")
	rv120Exec(t, admin,
		`CREATE TABLE rv120cdecoy.ran (op text)`,
		`CREATE VIEW rv120cdecoy.bide_steps AS SELECT 1 AS x`,
		`CREATE TABLE rv120cdecoy.bide_leases (run_id text, holder text, expiry timestamptz)`,
		`CREATE TABLE rv120cdecoy.bide_schema_version (id int, version int)`,
		`INSERT INTO rv120cdecoy.bide_schema_version VALUES (1, 999)`,
	)
	type opSig struct{ op, l, r string }
	sigs := []opSig{}
	for _, op := range []string{"=", "<>", "<", ">", "<=", ">=", "+", "-", "*"} {
		for _, ty := range [][2]string{{"text", "text"}, {"text[]", "text[]"}, {"oid", "regtype"}, {"oid", "oid"},
			{"name", "name"}, {"name", "text"}, {"name", "name[]"}, {"int8", "int8"}, {"int8", "int4"}, {"int4", "int4"}, {"int2", "int2"},
			{"char", "char"}, {"char", "unknown"}, {"oidvector", "oidvector"}, {"timestamptz", "timestamptz"},
			{"timestamptz", "interval"}, {"float8", "interval"}, {"int2", "int8"}, {"regtype", "oid"}, {"text", "unknown"}} {
			if ty[1] == "unknown" {
				continue
			}
			sigs = append(sigs, opSig{op, ty[0], ty[1]})
		}
	}
	for i, s := range sigs {
		fn := "rv120cdecoy.f" + strings.Repeat("x", 0) + itoa(i)
		rv120Exec(t, admin,
			`CREATE FUNCTION `+fn+`(a `+s.l+`, b `+s.r+`) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN INSERT INTO rv120cdecoy.ran VALUES ('`+s.op+` `+s.l+` `+s.r+`'); RAISE EXCEPTION 'decoy operator ran'; END $$`)
		if _, err := admin.ExecContext(ctx, `CREATE OPERATOR rv120cdecoy.`+s.op+` (LEFTARG = `+s.l+`, RIGHTARG = `+s.r+`, FUNCTION = `+fn+`)`); err != nil {
			t.Logf("skip operator %s(%s,%s): %v", s.op, s.l, s.r, err)
		}
	}
	for _, path := range []string{"rv120cdecoy,public", "rv120cdecoy", `"$user",rv120cdecoy,pg_catalog`, `""`} {
		t.Run(path, func(t *testing.T) {
			for i := 0; i < 2; i++ {
				s, err := Open(ctx, rv120DSN(t, base, "", path), WithSchema("rv120capp"))
				if err != nil {
					t.Fatalf("Open #%d with path %s: %v", i+1, path, err)
				}
				if s.schema != "rv120capp" {
					t.Fatalf("schema = %q", s.schema)
				}
				rv120cWork(t, ctx, s, "decoy")
				s.Close()
			}
		})
	}
	var ran int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM rv120cdecoy.ran`).Scan(&ran); err != nil || ran != 0 {
		t.Fatalf("decoy operators ran %d times (%v)", ran, err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// A pinned schema that does not exist: the documentation says it must exist. Open must refuse it,
// create nothing, and say why.
func TestRV120c_PinnedMissingSchema(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS rv120cmissing CASCADE`)
	s, err := Open(ctx, rv120DSN(t, base, "", "public"), WithSchema("rv120cmissing"))
	if err == nil {
		s.Close()
		t.Fatal("Open with a missing pinned schema succeeded")
	}
	t.Logf("Open = %v (ErrConfig %v, ErrStorage %v)", err, errors.Is(err, agent.ErrConfig), errors.Is(err, agent.ErrStorage))
	var n int
	admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = 'rv120cmissing'`).Scan(&n)
	if n != 0 {
		t.Fatal("Open created the schema")
	}
	if !errors.Is(err, agent.ErrConfig) {
		t.Errorf("a missing pinned schema is a configuration error; got a non-ErrConfig error: %v", err)
	}
}

// The full store suites with a pinned store, through a decoy path.
func TestRV120c_StorePinned(t *testing.T) {
	admin, base := rv120Admin(t)
	rv120Schema(t, admin, "rv120cpin")
	rv120Schema(t, admin, "rv120cpindecoy")
	open := func(t *testing.T) *Store {
		s, err := Open(context.Background(), rv120DSN(t, base, "", "rv120cpindecoy,public"), WithSchema("rv120cpin"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	storetest.Run(t, func(t *testing.T) agent.Store { return open(t) })
}

// Reserved schema names: pg_temp names the session's temporary schema even quoted, so a store
// there would lose its journal with each pooled connection. Open must refuse it.
func TestRV120c_PinnedReservedNames(t *testing.T) {
	_, base := rv120Admin(t)
	ctx := context.Background()
	for _, name := range []string{"pg_temp", "pg_catalog", "pg_toast"} {
		s, err := Open(ctx, rv120DSN(t, base, "", "public"), WithSchema(name))
		if err == nil {
			s.Close()
			t.Errorf("Open WithSchema(%q) succeeded", name)
			continue
		}
		t.Logf("WithSchema(%q): %v", name, err)
	}
}
