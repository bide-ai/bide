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
// read committed. Each insert takes the run's advisory lock and then computes the next position as
// MAX(seq)+1; at repeatable read or serializable the snapshot would be taken by the lock statement,
// before the lock is granted, so the insert would miss rows committed while it waited and collide
// on (run_id, seq). A lost tool-result write halts the run later. Skips without PG_DSN.
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
			if len(hist) != steps {
				t.Fatalf("History has %d records, want %d", len(hist), steps)
			}
		})
	}
}

// Two nodes racing to record the same step converge on one record, without an error, at a
// stricter default isolation. The loser's insert meets the winner's row, committed while it waited
// for the run's lock; at repeatable read or serializable that row is outside the loser's snapshot,
// and ON CONFLICT DO NOTHING then fails with a serialization error (40001) instead of doing
// nothing. Skips without PG_DSN.
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
// serializable, which a recoverer would read as a storage failure rather than a lost race. Skips
// without PG_DSN.
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
