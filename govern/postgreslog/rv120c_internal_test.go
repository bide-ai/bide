package postgreslog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern"
	"github.com/bide-ai/bide/govern/eventlogtest"
)

var rv120cNames = []string{
	`rv120c we"ird`,
	`rv120c.dotted`,
	`Rv120cMiXed`,
	`rv120c_схема_🚀`,
	"rv120c_" + strings.Repeat("a", 56),
	"rv120c_" + strings.Repeat("b", 57),
	"rv120c_" + strings.Repeat("c", 80),
	"rv120c_" + strings.Repeat("é", 30),
	`rv120c -- /* $1 */ 'x' ; drop`,
}

func rv120cWork(t *testing.T, ctx context.Context, l *Log, i int) {
	t.Helper()
	ent := fmt.Sprintf("rv120c-%s-%d", t.Name(), i)
	if _, err := l.Append(ctx, ent, "a", "e1"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if seq, err := l.Append(ctx, ent, "a", "e1"); err != nil || seq != 0 {
		t.Fatalf("Append again = %d, %v", seq, err)
	}
	if _, err := l.Append(ctx, ent, "b", "e2"); err != nil {
		t.Fatalf("Append b: %v", err)
	}
	if ev, err := l.Events(ctx, ent, 0); err != nil || len(ev) != 2 {
		t.Fatalf("Events = %v, %v", ev, err)
	}
}

func TestRV120c_LogPinnedNames(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	for _, name := range rv120cNames {
		t.Run(name, func(t *testing.T) {
			q := quoteIdent(name)
			admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+q+` CASCADE`)
			rv120Exec(t, admin, `CREATE SCHEMA `+q)
			t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+q+` CASCADE`) })
			for i := 0; i < 2; i++ {
				l, err := Open(ctx, rv120DSN(t, base, "", "public"), WithSchema(name))
				if err != nil {
					t.Fatalf("Open #%d WithSchema(%q) (%d bytes): %v", i+1, name, len(name), err)
				}
				rv120cWork(t, ctx, l, i)
				l.Close()
			}
			var n int
			if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM `+q+`.governed_events`).Scan(&n); err != nil || n != 4 {
				t.Fatalf("rows = %d, %v; want 4", n, err)
			}
		})
	}
}

func TestRV120c_LogPinnedIgnoresDecoyPath(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	rv120Schema(t, admin, "rv120clapp")
	rv120Schema(t, admin, "rv120cldecoy")
	rv120Exec(t, admin,
		`CREATE TABLE rv120cldecoy.ran (op text)`,
		`CREATE VIEW rv120cldecoy.governed_events AS SELECT 1 AS x`,
	)
	i := 0
	for _, op := range []string{"=", "<>", "<", ">", "<=", ">=", "+", "-", "*"} {
		for _, ty := range [][2]string{{"text", "text"}, {"text[]", "text[]"}, {"oid", "regtype"}, {"oid", "oid"},
			{"name", "name"}, {"name", "text"}, {"name", "name[]"}, {"int8", "int8"}, {"int8", "int4"}, {"int4", "int4"},
			{"int2", "int2"}, {"char", "char"}, {"oidvector", "oidvector"}, {"regtype", "oid"}} {
			fn := fmt.Sprintf("rv120cldecoy.f%d", i)
			i++
			rv120Exec(t, admin, `CREATE FUNCTION `+fn+`(a `+ty[0]+`, b `+ty[1]+`) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN INSERT INTO rv120cldecoy.ran VALUES ('x'); RAISE EXCEPTION 'decoy operator ran'; END $$`)
			admin.ExecContext(ctx, `CREATE OPERATOR rv120cldecoy.`+op+` (LEFTARG = `+ty[0]+`, RIGHTARG = `+ty[1]+`, FUNCTION = `+fn+`)`)
		}
	}
	for _, path := range []string{"rv120cldecoy,public", `"$user",rv120cldecoy,pg_catalog`, `""`} {
		t.Run(path, func(t *testing.T) {
			for i := 0; i < 2; i++ {
				l, err := Open(ctx, rv120DSN(t, base, "", path), WithSchema("rv120clapp"))
				if err != nil {
					t.Fatalf("Open #%d with path %s: %v", i+1, path, err)
				}
				rv120cWork(t, ctx, l, i)
				l.Close()
			}
		})
	}
	var ran int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM rv120cldecoy.ran`).Scan(&ran); err != nil || ran != 0 {
		t.Fatalf("decoy operators ran %d times (%v)", ran, err)
	}
}

func TestRV120c_LogPinnedMissingSchema(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS rv120clmissing CASCADE`)
	l, err := Open(ctx, rv120DSN(t, base, "", "public"), WithSchema("rv120clmissing"))
	if err == nil {
		l.Close()
		t.Fatal("Open with a missing pinned schema succeeded")
	}
	t.Logf("Open = %v (ErrConfig %v)", err, errors.Is(err, agent.ErrConfig))
	if !errors.Is(err, agent.ErrConfig) {
		t.Errorf("a missing pinned schema is a configuration error; got a non-ErrConfig error: %v", err)
	}
}

func TestRV120c_LogConformancePinned(t *testing.T) {
	admin, base := rv120Admin(t)
	rv120Schema(t, admin, "rv120clpin")
	rv120Schema(t, admin, "rv120clpindecoy")
	eventlogtest.Run(t, func(t *testing.T) govern.EventLog {
		l, err := Open(context.Background(), rv120DSN(t, base, "", "rv120clpindecoy,public"), WithSchema("rv120clpin"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return l
	})
}
