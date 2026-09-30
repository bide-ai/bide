package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// stricterLevels are the default_transaction_isolation settings a deployment may configure above
// Postgres's default of read committed. The store must behave the same under each.
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

// Concurrent steps of one run are all recorded when the deployment's default isolation is above
// read committed. Each insert computes the next position as MAX(seq)+1 in its own snapshot, so
// concurrent inserts collide on (run_id, seq) (23505), or at repeatable read or serializable fail
// with a serialization error (40001), and the store must run the insert again rather than report
// the collision. A lost tool-result write halts the run later. Skips without PG_DSN.
func TestPostgres_ConcurrentStepsUnderStricterDefaultIsolation(t *testing.T) {
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	for _, level := range stricterLevels {
		t.Run(strings.ReplaceAll(level, " ", "_"), func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(ctx, isolatedDSN(t, base, level))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			runID := uniqueID(t, "pg-iso-")
			const steps = 32
			errs := make([]error, steps)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range steps {
				wg.Go(func() {
					<-start
					_, errs[i] = s.Do(ctx, runID, fmt.Sprintf("step-%02d", i), func(context.Context) (agent.Record, error) {
						return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(fmt.Sprint(i))}, nil
					})
				})
			}
			close(start)
			wg.Wait()
			failed := 0
			for i, err := range errs {
				if err != nil {
					failed++
					t.Errorf("step %d: %v", i, err)
				}
			}
			if failed > 0 {
				t.Fatalf("%d of %d concurrent steps failed at %s", failed, steps, level)
			}
			hist, err := s.History(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if len(hist) != steps+1 { // the journal header, then the steps
				t.Fatalf("History has %d records, want the header and %d", len(hist), steps)
			}
		})
	}
}

// Two nodes racing to record the same step converge on one record, without an error, at a
// stricter default isolation. The loser's insert meets the winner's row, committed while it waited
// for the winner; at repeatable read or serializable that row is outside the loser's snapshot, and
// ON CONFLICT DO NOTHING then fails with a serialization error (40001) instead of doing nothing,
// which the store must answer by running the insert again. Skips without PG_DSN.
func TestPostgres_RacingNodesUnderStricterDefaultIsolation(t *testing.T) {
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	for _, level := range stricterLevels {
		t.Run(strings.ReplaceAll(level, " ", "_"), func(t *testing.T) {
			ctx := context.Background()
			dsn := isolatedDSN(t, base, level)
			var nodes [2]*Store
			for i := range nodes {
				s, err := Open(ctx, dsn)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				nodes[i] = s
			}
			const runs = 32
			prefix := uniqueID(t, "pg-iso-race-")
			errs := make(chan error, runs*len(nodes))
			start := make(chan struct{})
			var wg sync.WaitGroup
			for r := range runs {
				for n, s := range nodes {
					wg.Go(func() {
						<-start
						_, err := s.Do(ctx, fmt.Sprintf("%s-%d", prefix, r), "step", func(context.Context) (agent.Record, error) {
							return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(fmt.Sprint(n))}, nil
						})
						if err != nil {
							errs <- fmt.Errorf("run %d node %d: %w", r, n, err)
						}
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
				t.Fatalf("%d racing steps failed at %s", failed, level)
			}
		})
	}
}

// Nodes opening the store at once over an empty schema all succeed at a stricter default
// isolation too. Skips without PG_DSN.
func TestPostgres_ConcurrentOpensUnderStricterDefaultIsolation(t *testing.T) {
	for _, level := range stricterLevels {
		t.Run(strings.ReplaceAll(level, " ", "_"), func(t *testing.T) {
			for range 3 {
				dsn, _, _ := freshSchema(t)
				dsn = isolatedDSN(t, dsn, level)
				const nodes = 8
				errs := make(chan error, nodes)
				start := make(chan struct{})
				for range nodes {
					go func() {
						<-start
						s, err := Open(context.Background(), dsn)
						if err == nil {
							s.Close()
						}
						errs <- err
					}()
				}
				close(start)
				for range nodes {
					if err := <-errs; err != nil {
						t.Fatalf("a concurrent Open failed at %s: %v", level, err)
					}
				}
			}
		})
	}
}

// Lease calls racing on one run never fail at a stricter default isolation. A single-statement
// upsert or update that meets a row another transaction changed after its snapshot proceeds on the
// latest row at read committed, but fails with a serialization error (40001) at repeatable read or
// serializable; the store must run it again rather than report it, which a recoverer would read
// as a storage failure rather than a lost race. Skips without PG_DSN.
func TestPostgres_ConcurrentLeasesUnderStricterDefaultIsolation(t *testing.T) {
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	for _, level := range stricterLevels {
		t.Run(strings.ReplaceAll(level, " ", "_"), func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(ctx, isolatedDSN(t, base, level))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			runID := uniqueID(t, "pg-iso-lease-")
			const workers, rounds = 16, 25
			errs := make(chan error, workers*rounds*3)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range workers {
				wg.Go(func() {
					<-start
					// Half the workers share a holder, so their acquires and renewals update the
					// same live row; the others contend for it.
					holder := "shared"
					if i%2 == 1 {
						holder = fmt.Sprintf("h%d", i)
					}
					for range rounds {
						if _, err := s.AcquireLease(ctx, runID, holder, time.Minute); err != nil {
							errs <- fmt.Errorf("acquire: %w", err)
						}
						if _, err := s.RenewLease(ctx, runID, holder, time.Minute); err != nil {
							errs <- fmt.Errorf("renew: %w", err)
						}
					}
					if err := s.ReleaseLease(ctx, runID, holder); err != nil {
						errs <- fmt.Errorf("release: %w", err)
					}
				})
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
				t.Fatalf("%d lease calls failed at %s", failed, level)
			}
		})
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

// The loser's path, made deterministic: a winner transaction records seq 0 and stays open; the
// store's insert, whose snapshot does not see that row, computes seq 0 too and blocks on the
// winner's row; the winner commits; and only then does the insert proceed. Recording the same step
// must then report that another node recorded it (0 rows, no error) rather than fail with 40001,
// and recording another step must take seq 1 rather than fail on seq 0 (23505): the store runs the
// insert again with a snapshot that sees the winner's row. Skips without PG_DSN.
func TestPostgres_BlockedInsertSeesTheWinnersCommit(t *testing.T) {
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, level := range append([]string{"read committed"}, stricterLevels...) {
		for _, tc := range []struct {
			name    string
			step    string
			wantN   int64
			wantSeq int64
		}{
			{"same_step", "step", 0, 0},
			{"next_step", "other", 1, 1},
		} {
			t.Run(strings.ReplaceAll(level, " ", "_")+"/"+tc.name, func(t *testing.T) {
				s, err := Open(ctx, isolatedDSN(t, base, level))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				runID := uniqueID(t, "pg-iso-blocked-")
				winner, err := admin.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
				if err != nil {
					t.Fatal(err)
				}
				defer winner.Rollback()
				var winnerPID int64
				if err := winner.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&winnerPID); err != nil {
					t.Fatal(err)
				}
				if _, err := winner.ExecContext(ctx, `INSERT INTO bide_steps (run_id, seq, name, data) VALUES ($1, 0, 'step', '\x00')`, runID); err != nil {
					t.Fatal(err)
				}
				type result struct {
					n   int64
					err error
				}
				done := make(chan result, 1)
				go func() {
					_, inserted, err := s.insert(ctx, runID, tc.step, []byte("loser"))
					n := int64(0)
					if inserted {
						n = 1
					}
					done <- result{n, err}
				}()
				waitForWaiterOn(t, admin, winnerPID)
				if err := winner.Commit(); err != nil {
					t.Fatal(err)
				}
				got := <-done
				if got.err != nil || got.n != tc.wantN {
					t.Fatalf("insert of %q after the winner committed = (%d, %v), want (%d, nil)", tc.step, got.n, got.err, tc.wantN)
				}
				var seq int64
				var data []byte
				if err := admin.QueryRowContext(ctx, `SELECT seq, data FROM bide_steps WHERE run_id = $1 AND name = $2`, runID, tc.step).Scan(&seq, &data); err != nil {
					t.Fatal(err)
				}
				wantData := "loser"
				if tc.wantN == 0 {
					wantData = "\x00"
				}
				if seq != tc.wantSeq || string(data) != wantData {
					t.Fatalf("step %q recorded at seq %d with %q, want seq %d with %q", tc.step, seq, data, tc.wantSeq, wantData)
				}
			})
		}
	}
}
