package postgreslog_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/govern/postgreslog"
)

// stricterLevels are the default_transaction_isolation settings a deployment may configure above
// Postgres's default of read committed. The log must behave the same under each.
var stricterLevels = []string{"repeatable read", "serializable"}

// isolatedDSN returns dsn with its sessions' default_transaction_isolation set to level (pgx sends
// an unrecognised query parameter as a startup parameter), and checks that a session opened with
// it runs at that level, so a test using it cannot pass by running at read committed.
func isolatedDSN(t *testing.T, dsn, level string) string {
	t.Helper()
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	out := dsn + sep + "default_transaction_isolation=" + url.PathEscape(level)
	db, err := sql.Open("pgx", out)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRowContext(context.Background(), `SHOW default_transaction_isolation`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != level {
		t.Fatalf("session default_transaction_isolation = %q, want %q", got, level)
	}
	return out
}

// Concurrent appends to one entity, from two processes and with every id sent twice, all succeed
// when the deployment's default isolation is above read committed, and leave dense positions with
// one per id. Each append computes the next position as MAX(seq)+1 in its own snapshot, so
// concurrent appends collide on (entity, seq) (23505), and at repeatable read or serializable a
// repeated id fails on the append_id index with a serialization error (40001); the log must run
// the append again rather than report either.
func TestAppend_ConcurrentUnderStricterDefaultIsolation(t *testing.T) {
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres event-log integration test")
	}
	for _, level := range stricterLevels {
		t.Run(strings.ReplaceAll(level, " ", "_"), func(t *testing.T) {
			ctx := context.Background()
			dsn := isolatedDSN(t, base, level)
			var logs [2]*postgreslog.Log
			for i := range logs {
				l, err := postgreslog.Open(ctx, dsn)
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
				logs[i] = l
			}
			entity := uniqueID(t, "iso-")
			const ids = 16
			seqs := make([][2]int64, ids)
			errs := make(chan error, ids*len(logs))
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range ids {
				for n, l := range logs {
					wg.Go(func() {
						<-start
						seq, err := l.Append(ctx, entity, fmt.Sprintf("id%02d", i), fmt.Sprintf("e%02d", i))
						if err != nil {
							errs <- fmt.Errorf("append id%02d via log %d: %w", i, n, err)
						}
						seqs[i][n] = seq
					})
				}
			}
			close(start)
			wg.Wait()
			close(errs)
			failed := 0
			for err := range errs {
				if failed < 5 {
					t.Error(err)
				}
				failed++
			}
			if failed > 0 {
				t.Fatalf("%d of %d concurrent appends failed at %s", failed, ids*len(logs), level)
			}
			events, err := logs[0].Events(ctx, entity, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != ids {
				t.Fatalf("entity holds %d events, want %d", len(events), ids)
			}
			for i, s := range seqs {
				if s[0] != s[1] || events[s[0]] != fmt.Sprintf("e%02d", i) {
					t.Fatalf("id%02d returned positions %v; events %v", i, s, events)
				}
			}
		})
	}
}

// Processes opening the log at once all succeed at a stricter default isolation, on a new schema
// and on a table from before appends carried ids. Skips without PG_DSN.
func TestOpen_ConcurrentOpensUnderStricterDefaultIsolation(t *testing.T) {
	for _, level := range stricterLevels {
		for _, legacy := range []bool{false, true} {
			for trial := range 3 {
				_, _, dsn := migrationSchema(t, legacy)
				dsn = isolatedDSN(t, dsn, level)
				var wg sync.WaitGroup
				errs := make([]error, 8)
				for i := range errs {
					wg.Go(func() {
						l, err := postgreslog.Open(context.Background(), dsn)
						if err != nil {
							errs[i] = err
							return
						}
						defer l.Close()
						_, errs[i] = l.Append(context.Background(), "e", fmt.Sprintf("id%d", i), "new")
					})
				}
				wg.Wait()
				for i, err := range errs {
					if err != nil {
						t.Fatalf("%s legacy=%v trial %d: concurrent Open %d: %v", level, legacy, trial, i, err)
					}
				}
			}
		}
	}
}

// waitForWaiterOn polls pg_blocking_pids until a session waits on a lock held by the session pid,
// so a test can order a commit after a blocked statement.
func waitForWaiterOn(t *testing.T, admin *sql.DB, pid int64) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := admin.QueryRowContext(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE $1::int = ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no session waited on session %d", pid)
		}
		<-tick.C
	}
}

// An append blocked behind another, made deterministic: a winner transaction records id "a" at
// position 0 and stays open; Append, whose snapshot does not see that row, blocks on it; the
// winner commits; and only then does Append proceed. Appending "a" again must return the recorded
// position 0 rather than fail on the append id (40001 at a stricter level), and appending "b" must
// take position 1 rather than fail on position 0 (23505): the log runs the append again with a
// snapshot that sees the winner's row. Skips without PG_DSN.
func TestAppend_BlockedAppendSeesTheWinnersCommit(t *testing.T) {
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres event-log integration test")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, level := range append([]string{"read committed"}, stricterLevels...) {
		for _, tc := range []struct {
			name, id, event string
			want            int64
		}{
			{"same_id", "a", "ea", 0},
			{"next_id", "b", "eb", 1},
		} {
			t.Run(strings.ReplaceAll(level, " ", "_")+"/"+tc.name, func(t *testing.T) {
				l, err := postgreslog.Open(ctx, isolatedDSN(t, base, level))
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
				entity := uniqueID(t, "iso-blocked-")
				winner, err := admin.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
				if err != nil {
					t.Fatal(err)
				}
				defer winner.Rollback()
				var winnerPID int64
				if err := winner.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&winnerPID); err != nil {
					t.Fatal(err)
				}
				if _, err := winner.ExecContext(ctx, `INSERT INTO governed_events (entity, seq, event, append_id) VALUES ($1, 0, 'ea', 'a')`, entity); err != nil {
					t.Fatal(err)
				}
				type result struct {
					seq int64
					err error
				}
				done := make(chan result, 1)
				go func() {
					seq, err := l.Append(ctx, entity, tc.id, tc.event)
					done <- result{seq, err}
				}()
				waitForWaiterOn(t, admin, winnerPID)
				if err := winner.Commit(); err != nil {
					t.Fatal(err)
				}
				got := <-done
				if got.err != nil || got.seq != tc.want {
					t.Fatalf("Append(%q) after the winner committed = (%d, %v), want (%d, nil)", tc.id, got.seq, got.err, tc.want)
				}
				events, err := l.Events(ctx, entity, 0)
				if err != nil {
					t.Fatal(err)
				}
				if want := int(tc.want) + 1; len(events) != want || events[tc.want] != tc.event {
					t.Fatalf("events = %q, want %d with %q at position %d", events, want, tc.event, tc.want)
				}
			})
		}
	}
}
