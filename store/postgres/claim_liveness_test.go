package postgres_test

// FINDING 1 of the review of the claim protocol, on Postgres: a claim taken back is pinned with a
// claim-held record; a cancellation before the effect then records "not started" onto that pin,
// which is taken as success, and the run halts for ever though the effect never ran.

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/store/postgres"
)

type pgR3 struct {
	s      agent.Store
	mu     sync.Mutex
	faults []string // prefixes that fail once, not committed
	after  map[string]func()
}

func (w *pgR3) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	w.mu.Lock()
	for i, p := range w.faults {
		if strings.HasPrefix(name, p) {
			w.faults = append(w.faults[:i:i], w.faults[i+1:]...)
			w.mu.Unlock()
			return agent.Entry{}, false, errors.New("connection lost")
		}
	}
	w.mu.Unlock()
	e, ok, err := w.s.Insert(ctx, runID, name, data)
	if err == nil {
		for p, f := range w.after {
			if strings.HasPrefix(name, p) && f != nil {
				w.after[p] = nil
				f()
			}
		}
	}
	return e, ok, err
}
func (w *pgR3) Get(ctx context.Context, r, n string) (agent.Entry, bool, error) {
	return w.s.Get(ctx, r, n)
}
func (w *pgR3) Load(ctx context.Context, r string, a int64) iter.Seq2[agent.Entry, error] {
	return w.s.Load(ctx, r, a)
}

func TestPostgres_ClaimHeldThenCancelled_HaltsForever(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("PG_DSN not set")
	}
	ctx := context.Background()
	s, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run := fmt.Sprintf("rev3-%d", time.Now().UnixNano())
	fired := 0
	body := func(context.Context) (string, error) { fired++; return "ok", nil }

	w := &pgR3{s: s, faults: []string{"attempt:step:pay", "attempt:not-started:"}}
	j1, _ := agent.NewJournal(w)
	_, err1 := j1.Step(ctx, run, "pay", body)

	ctx2, cancel := context.WithCancel(ctx)
	w.after = map[string]func(){"attempt:not-started:": cancel}
	j2, _ := agent.NewJournal(w)
	_, err2 := j2.Step(ctx2, run, "pay", body)

	j3, _ := agent.NewJournal(s) // another process
	_, err3 := j3.Step(ctx, run, "pay", body)
	t.Logf("drive 1: %v\ndrive 2: %v\ndrive 3: %v; fired %d", err1, err2, err3, fired)
	var halt *agent.OutcomeUnknown
	if fired == 0 && errors.As(err3, &halt) {
		t.Fatalf("the effect never started, but the run halts for ever")
	}
}
