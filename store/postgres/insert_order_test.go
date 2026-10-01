package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// waitBlocked polls until n backends run the insert statement while waiting on a lock.
func waitBlocked(t *testing.T, admin *sql.DB, n int) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.Now().Add(30 * time.Second)
	for ; time.Now().Before(deadline); <-tick.C {
		var c int
		if err := admin.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE 'INSERT INTO %bide_steps%'`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c >= n {
			return
		}
	}
	t.Fatal("insert never blocked")
}

func loadAll(t *testing.T, s *Store, run string) []agent.Entry {
	var out []agent.Entry
	for e, err := range s.Load(context.Background(), run, -1) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// A held, uncommitted insert at the next position blocks a concurrent Insert; the Insert takes the
// position after it once it commits, or the same position if it rolls back; a reader never sees the
// later entry first. (From the #113 review.) Skips without PG_DSN.
func TestPostgres_InsertOrderAgainstHeldInsert(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	for _, commit := range []bool{true, false} {
		t.Run(fmt.Sprint("commit=", commit), func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			admin, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			run := fmt.Sprintf("order-%d", time.Now().UnixNano())
			if _, _, err := s.Insert(ctx, run, "h", []byte("h")); err != nil {
				t.Fatal(err)
			}
			tx, err := admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			var held int64
			if err := tx.QueryRowContext(ctx, string(s.t.insert), run, "a", []byte("a")).Scan(&held); err != nil {
				t.Fatal(err)
			}
			done := make(chan agent.Entry)
			go func() {
				e, _, err := s.Insert(ctx, run, "b", []byte("b"))
				if err != nil {
					t.Error(err)
				}
				done <- e
			}()
			waitBlocked(t, admin, 1)
			if got := loadAll(t, s, run); len(got) != 1 {
				t.Fatalf("reader sees %d entries while a is uncommitted and b blocked", len(got))
			}
			if commit {
				tx.Commit()
			} else {
				tx.Rollback()
			}
			b := <-done
			got := loadAll(t, s, run)
			if commit {
				if b.Seq != held+1 || len(got) != 3 || got[1].Name != "a" || got[2].Name != "b" {
					t.Fatalf("held=%d b=%d load=%v", held, b.Seq, got)
				}
			} else if b.Seq != held || len(got) != 2 {
				t.Fatalf("held=%d b=%d load=%v", held, b.Seq, got)
			}
		})
	}
}
