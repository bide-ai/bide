package postgres

// The next_seq design against commit-time lock holds, a rolling upgrade from versions that took
// the run lock in a statement of their own, resends of one name, and table prefixes. Every wait
// is on a pg_locks condition, never a fixed sleep. (From the reviews of #113.)


import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// rvWaitLock polls pg_locks until a session waits for the advisory lock with the given 64-bit key.
func rvWaitLock(t *testing.T, admin *sql.DB, keySQL string, args ...any) {
	t.Helper()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var n int
		if err := admin.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND objsubid = 1
			AND ((classid::bigint << 32) | objid::bigint) = (`+keySQL+`)`, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no waiter on advisory key %s %v", keySQL, args)
		}
		<-tick.C
	}
}

// rvHolders returns how many sessions hold the run's advisory lock granted.
func rvHolders(t *testing.T, admin *sql.DB, run string) int {
	var n int
	if err := admin.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND objsubid = 1
		AND ((classid::bigint << 32) | objid::bigint) = hashtextextended($1, 0)`, run).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An autocommit Insert keeps the run's lock until its commit completes, not only until its
// statement ends: a deferred constraint trigger holds the insert of "a" at commit time (after the
// INSERT statement has finished). A second Insert must wait on the run lock, a reader must not see
// "a" yet, and once "a" commits the second Insert takes the next position.
func TestPostgres_NextSeqHoldsTheRunLockThroughCommit(t *testing.T) {
	for _, level := range []string{"", "repeatable read", "serializable"} {
		t.Run(strings.ReplaceAll("level_"+level, " ", "_"), func(t *testing.T) {
			ctx := context.Background()
			dsn, schema, admin := freshSchema(t)
			if level != "" {
				dsn = isolatedDSN(t, dsn, level)
			}
			s, err := Open(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			const gateKey = 7113113
			if _, err := admin.ExecContext(ctx, fmt.Sprintf(`
				CREATE FUNCTION %[1]s.zz_gate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(%[2]d); RETURN NULL; END $$;
				CREATE CONSTRAINT TRIGGER zz_gate AFTER INSERT ON %[1]s.bide_steps DEFERRABLE INITIALLY DEFERRED
					FOR EACH ROW WHEN (NEW.name = 'a') EXECUTE FUNCTION %[1]s.zz_gate();`, schema, gateKey)); err != nil {
				t.Fatal(err)
			}
			run := uniqueID(t, "rv-commit-")
			if _, _, err := s.Insert(ctx, run, "h", []byte("h")); err != nil {
				t.Fatal(err)
			}
			gate, err := admin.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Close()
			if _, err := gate.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, gateKey); err != nil {
				t.Fatal(err)
			}
			type res struct {
				e   agent.Entry
				ok  bool
				err error
			}
			aDone, bDone := make(chan res, 1), make(chan res, 1)
			go func() { e, ok, err := s.Insert(ctx, run, "a", []byte("a")); aDone <- res{e, ok, err} }()
			rvWaitLock(t, admin, `$1::bigint`, int64(gateKey)) // "a" finished its statement, is committing
			if n := rvHolders(t, admin, run); n != 1 {
				t.Fatalf("while a commits, %d sessions hold the run lock, want 1", n)
			}
			go func() { e, ok, err := s.Insert(ctx, run, "b", []byte("b")); bDone <- res{e, ok, err} }()
			rvWaitLock(t, admin, `hashtextextended($1, 0)`, run)
			if got := loadAll(t, s, run); len(got) != 1 {
				t.Fatalf("reader sees %d entries while a commits", len(got))
			}
			if _, err := gate.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, gateKey); err != nil {
				t.Fatal(err)
			}
			a, b := <-aDone, <-bDone
			if a.err != nil || b.err != nil || a.e.Seq != 1 || b.e.Seq != 2 {
				t.Fatalf("a=%+v b=%+v, want seq 1 then 2", a, b)
			}
			got := loadAll(t, s, run)
			if len(got) != 3 || got[1].Name != "a" || got[2].Name != "b" {
				t.Fatalf("load = %v", got)
			}
		})
	}
}

// oldInsert is the pre-#113 insert (v0.8.0 and the parent of #113), run in its own read committed
// transaction: the run's lock, then the insert with a MAX subquery.
const oldLock = `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`
const oldInsert = `INSERT INTO bide_steps (run_id, seq, name, data)
	VALUES ($1, (SELECT COALESCE(MAX(seq), -1) + 1 FROM bide_steps WHERE run_id = $1), $2, $3)
	ON CONFLICT (run_id, name) DO NOTHING RETURNING seq`

// Rolling upgrade: an old node's insert transaction and a new node's single statement queue on
// the same lock in both directions, and neither picks a position the other holds, whatever the new
// node's default isolation.
func TestPostgres_NextSeqQueuesWithEarlierVersions(t *testing.T) {
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	for _, level := range []string{"", "repeatable read", "serializable"} {
		for _, oldFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/oldFirst=%v", strings.ReplaceAll("level_"+level, " ", "_"), oldFirst), func(t *testing.T) {
				ctx := context.Background()
				dsn, schema, admin := freshSchema(t)
				newDSN := dsn
				if level != "" {
					newDSN = isolatedDSN(t, dsn, level)
				}
				s, err := Open(ctx, newDSN)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				old, err := sql.Open("pgx", dsn)
				if err != nil {
					t.Fatal(err)
				}
				defer old.Close()
				run := uniqueID(t, "rv-roll-")
				if _, _, err := s.Insert(ctx, run, "h", []byte("h")); err != nil {
					t.Fatal(err)
				}
				rc := &sql.TxOptions{Isolation: sql.LevelReadCommitted}
				if oldFirst {
					tx, err := old.BeginTx(ctx, rc)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					if _, err := tx.ExecContext(ctx, oldLock, run); err != nil {
						t.Fatal(err)
					}
					var oseq int64
					if err := tx.QueryRowContext(ctx, oldInsert, run, "old", []byte("o")).Scan(&oseq); err != nil {
						t.Fatal(err)
					}
					done := make(chan agent.Entry, 1)
					go func() {
						e, _, err := s.Insert(ctx, run, "new", []byte("n"))
						if err != nil {
							t.Error(err)
						}
						done <- e
					}()
					rvWaitLock(t, admin, `hashtextextended($1, 0)`, run)
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
					if e := <-done; oseq != 1 || e.Seq != 2 {
						t.Fatalf("old=%d new=%d, want 1 then 2", oseq, e.Seq)
					}
				} else {
					held, err := admin.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer held.Rollback()
					rvSetPath(t, held, schema)
					var nseq int64
					if err := held.QueryRowContext(ctx, string(s.t.insert), run, "new", []byte("n")).Scan(&nseq); err != nil {
						t.Fatal(err)
					}
					done := make(chan int64, 1)
					go func() {
						tx, err := old.BeginTx(ctx, rc)
						if err != nil {
							t.Error(err)
							done <- -1
							return
						}
						defer tx.Rollback()
						if _, err := tx.ExecContext(ctx, oldLock, run); err != nil {
							t.Error(err)
						}
						var seq int64
						if err := tx.QueryRowContext(ctx, oldInsert, run, "old", []byte("o")).Scan(&seq); err != nil {
							t.Error(err)
						}
						if err := tx.Commit(); err != nil {
							t.Error(err)
						}
						done <- seq
					}()
					rvWaitLock(t, admin, `hashtextextended($1, 0)`, run)
					if err := held.Commit(); err != nil {
						t.Fatal(err)
					}
					if oseq := <-done; nseq != 1 || oseq != 2 {
						t.Fatalf("new=%d old=%d, want 1 then 2", nseq, oseq)
					}
				}
				got := loadAll(t, s, run)
				if len(got) != 3 {
					t.Fatalf("load = %v", got)
				}
			})
		}
	}
}

// A3: a resend of a name whose first insert is still in flight (held uncommitted) waits and then
// returns the committed entry, or inserts it if the first rolled back; a resend after a lost reply
// returns the stored entry.
func TestPostgres_NextSeqResendDedupsByName(t *testing.T) {
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	for _, level := range []string{"", "repeatable read", "serializable"} {
		for _, commit := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/commit=%v", strings.ReplaceAll("level_"+level, " ", "_"), commit), func(t *testing.T) {
				ctx := context.Background()
				dsn, schema, admin := freshSchema(t)
				if level != "" {
					dsn = isolatedDSN(t, dsn, level)
				}
				s, err := Open(ctx, dsn)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				run := uniqueID(t, "rv-dup-")
				if _, _, err := s.Insert(ctx, run, "h", []byte("h")); err != nil {
					t.Fatal(err)
				}
				tx, err := admin.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				rvSetPath(t, tx, schema)
				var held int64
				if err := tx.QueryRowContext(ctx, string(s.t.insert), run, "x", []byte("first")).Scan(&held); err != nil {
					t.Fatal(err)
				}
				type res struct {
					e   agent.Entry
					ok  bool
					err error
				}
				done := make(chan res, 1)
				go func() { e, ok, err := s.Insert(ctx, run, "x", []byte("second")); done <- res{e, ok, err} }()
				rvWaitLock(t, admin, `hashtextextended($1, 0)`, run)
				if commit {
					tx.Commit()
				} else {
					tx.Rollback()
				}
				r := <-done
				if r.err != nil {
					t.Fatal(r.err)
				}
				if commit && (r.ok || r.e.Seq != held || string(r.e.Data) != "first") {
					t.Fatalf("resend after commit = %+v, want the first entry at %d, not inserted", r, held)
				}
				if !commit && (!r.ok || r.e.Seq != held || string(r.e.Data) != "second") {
					t.Fatalf("resend after rollback = %+v, want inserted at %d", r, held)
				}
				// Lost reply: the same call again returns the stored entry.
				e, ok, err := s.Insert(ctx, run, "x", []byte("second"))
				if err != nil || ok || e.Seq != held {
					t.Fatalf("second resend = %+v %v %v", e, ok, err)
				}
				if got := loadAll(t, s, run); len(got) != 2 {
					t.Fatalf("load = %v", got)
				}
			})
		}
	}
}

// Two stores with different prefixes in one schema share the run's lock key: an insert held in
// store a_ on run r makes store b_'s insert into its own, unrelated run r wait. Contention, not a
// correctness failure; the test documents it.
func TestPostgres_NextSeqPrefixesShareLockKeys(t *testing.T) {
	ctx := context.Background()
	dsn, schema, admin := freshSchema(t)
	a, err := Open(ctx, dsn, WithTablePrefix("a_"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, dsn, WithTablePrefix("b_"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	tx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rvSetPath(t, tx, schema)
	var seq int64
	if err := tx.QueryRowContext(ctx, string(a.t.insert), "r", "x", []byte("x")).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := b.Insert(ctx, "r", "y", []byte("y")); done <- err }()
	rvWaitLock(t, admin, `hashtextextended($1, 0)`, "r") // b_ waits on a_'s run lock
	tx.Commit()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// rvSetPath points tx at the test's schema, as the store's own sessions are.
func rvSetPath(t *testing.T, tx *sql.Tx, schema string) {
	t.Helper()
	if _, err := tx.Exec(`SET LOCAL search_path = ` + schema); err != nil {
		t.Fatal(err)
	}
}
