package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/store/postgres"
)

// TestPostgres_Perf measures Insert and lease throughput and latency, with 1, 4, 16 and 64
// concurrent writers: inserts into one run, inserts into a run per writer, and lease renewals. It
// prints one PERF line per case, with the database's rollback count (each failed attempt of a
// statement is a rollback). The case to watch is inserts into one run with 16 and 64 writers,
// where inserts race for positions and retry with a backoff. It runs only with REVIEW_PERF=1, on a
// quiet machine (REVIEW_ONE=1 limits it to the one-run inserts):
//
//	PG_DSN=... REVIEW_PERF=1 go test -run TestPostgres_Perf -v ./store/postgres
func TestPostgres_Perf(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" || os.Getenv("REVIEW_PERF") != "1" {
		// Not t.Skip: the CI job that runs this package against Postgres fails on any skip, which
		// would otherwise mean the database was not reached.
		t.Log("set PG_DSN and REVIEW_PERF=1 to measure")
		return
	}
	ctx := context.Background()
	s, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	admin, _ := sql.Open("pgx", dsn)
	defer admin.Close()
	rollbacks := func() int64 {
		var n int64
		admin.QueryRow(`SELECT xact_rollback FROM pg_stat_database WHERE datname = current_database()`).Scan(&n)
		return n
	}
	tag := time.Now().UnixNano()
	run := func(label string, workers, perWorker int, op func(w, i int) error) {
		lat := make([][]time.Duration, workers)
		admin.Exec(`SELECT pg_stat_clear_snapshot()`)
		rb0 := rollbacks()
		var wg sync.WaitGroup
		start := time.Now()
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range perWorker {
					t0 := time.Now()
					if err := op(w, i); err != nil {
						t.Error(err)
						return
					}
					lat[w] = append(lat[w], time.Since(t0))
				}
			}()
		}
		wg.Wait()
		el := time.Since(start)
		all := slices.Concat(lat...)
		slices.Sort(all)
		n := len(all)
		admin.Exec(`SELECT pg_stat_clear_snapshot()`)
		rb := rollbacks() - rb0
		fmt.Printf("PERF %-28s ops=%d ops/s=%.0f p50=%v p99=%v max=%v rollbacks=%d\n", label, n, float64(n)/el.Seconds(),
			all[n/2].Round(time.Microsecond), all[n*99/100].Round(time.Microsecond), all[n-1].Round(time.Microsecond), rb)
	}
	gs := []int{1, 4, 16, 64}
	if os.Getenv("REVIEW_ONE") != "" {
		gs = []int{1, 16, 64}
	}
	for _, g := range gs {
		per := 1000 / g
		if per < 30 {
			per = 30
		}
		runID := fmt.Sprintf("perf-one-%d-%d", tag, g)
		run(fmt.Sprintf("insert one-run g=%d", g), g, per, func(w, i int) error {
			_, _, err := s.Insert(ctx, runID, fmt.Sprintf("w%d-%d", w, i), []byte(`{"ok":true}`))
			return err
		})
		if os.Getenv("REVIEW_ONE") != "" {
			continue
		}
		run(fmt.Sprintf("insert run-per-worker g=%d", g), g, per, func(w, i int) error {
			_, _, err := s.Insert(ctx, fmt.Sprintf("perf-own-%d-%d-%d", tag, g, w), fmt.Sprintf("s%d", i), []byte(`{"ok":true}`))
			return err
		})
		run(fmt.Sprintf("renew lease g=%d", g), g, per, func(w, i int) error {
			rid := fmt.Sprintf("perf-lease-%d-%d-%d", tag, g, w)
			if i == 0 {
				_, err := s.AcquireLease(ctx, rid, "h", time.Minute)
				return err
			}
			_, err := s.RenewLease(ctx, rid, "h", time.Minute)
			return err
		})
	}
}
