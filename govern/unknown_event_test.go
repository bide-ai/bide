package govern_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

// buildCounter is a one-event machine: inc_a raises a, capped at 3.
func buildCounter(t *testing.T) *gsm.Machine {
	t.Helper()
	r := gsm.NewRegistry("counter")
	a := r.Int("a", 0, 5)
	r.DeclInvariant("a_cap", gsm.Le(gsm.V(a), gsm.Lit(3)), gsm.Do(gsm.Set(a, gsm.Lit(3))))
	r.DeclEvent("inc_a", gsm.Do(gsm.Set(a, gsm.Add(gsm.V(a), gsm.Lit(1)))))
	m, rep, err := r.Build()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, rep)
	}
	return m
}

// noPanic runs fn and fails the test (rather than crashing the test binary) if fn panics.
func noPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", what, r)
		}
	}()
	fn()
}

// An unknown event name is rejected before anything is written: Apply returns an ErrConfig
// error, and the shared log stays empty, so no other process ever reads the bad entry.
func TestPersistentGovernor_UnknownEventRejectedBeforeAppend(t *testing.T) {
	ctx := context.Background()
	m := buildCounter(t)
	log := govern.NewMemEventLog()
	g, err := govern.NewPersistent(ctx, m, log, "e", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	noPanic(t, "Apply(unknown event)", func() {
		a, err := g.Apply(ctx, "inc_typo")
		if !errors.Is(err, agent.ErrConfig) || a.Position != -1 {
			t.Fatalf("Apply(unknown) = %+v, %v; want position -1 and an ErrConfig error", a, err)
		}
	})
	if evs, _ := log.Events(ctx, "e", 0); len(evs) != 0 {
		t.Fatalf("a rejected event was written to the shared log: %q", evs)
	}
	if a, err := g.Apply(ctx, "inc_a"); err != nil || a.Position != 0 {
		t.Fatalf("Apply(inc_a) after a rejected event = %+v, %v; want position 0", a, err)
	}
}

// A log that already holds an entry the machine does not know (written by an older or
// foreign writer) is an error for every fold path, never a panic: NewPersistent, Sync, and
// Apply each report it, so one bad entry cannot crash every process that shares the log.
func TestPersistentGovernor_BadLogEntryIsAnErrorNotAPanic(t *testing.T) {
	ctx := context.Background()
	m := buildCounter(t)
	log := govern.NewMemEventLog()
	early, err := govern.NewPersistent(ctx, m, log, "e", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"inc_a", "inc_typo"} { // a foreign writer, bypassing validation
		if _, err := log.Append(ctx, "e", "foreign-"+ev, ev); err != nil {
			t.Fatal(err)
		}
	}
	noPanic(t, "NewPersistent over a bad log", func() {
		if _, err := govern.NewPersistent(ctx, m, log, "e", m.NewState()); !errors.Is(err, agent.ErrProtocol) {
			t.Fatalf("NewPersistent over a log with an unknown event: err = %v, want ErrProtocol", err)
		}
	})
	noPanic(t, "Sync over a bad log", func() {
		if _, err := early.Sync(ctx); !errors.Is(err, agent.ErrProtocol) {
			t.Fatalf("Sync over a log with an unknown event: err = %v, want ErrProtocol", err)
		}
	})
	// The fold stopped at the bad entry: the good event before it is folded in, and the bad
	// one is not skipped, so the state never silently diverges from a replay of the log.
	if got := early.State(); got.Digest() != m.Apply(m.NewState(), "inc_a").Digest() {
		t.Fatalf("state after a failed fold = %v, want the state through the last good entry", got)
	}
	noPanic(t, "Apply over a bad log", func() {
		if _, err := early.Apply(ctx, "inc_a"); !errors.Is(err, agent.ErrProtocol) {
			t.Fatalf("Apply over a log with an unknown event: err = %v, want ErrProtocol", err)
		}
	})
	// A retried fold does not apply the good entries before the bad one a second time.
	if got := early.State(); got.Digest() != m.Apply(m.NewState(), "inc_a").Digest() {
		t.Fatalf("state after repeated failed folds = %v, want the state through the last good entry", got)
	}
}

// The in-memory Governor rejects an unknown event with an error instead of panicking.
func TestGovernor_UnknownEventIsAnError(t *testing.T) {
	m := buildCounter(t)
	g := govern.New(m, m.NewState())
	noPanic(t, "Governor.Apply(unknown event)", func() {
		if _, err := g.Apply(context.Background(), "inc_typo"); !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("Apply(unknown) err = %v, want ErrConfig", err)
		}
	})
	if a, err := g.Apply(context.Background(), "inc_a"); err != nil || a.Position != 0 {
		t.Fatalf("Apply(inc_a) after a rejected event = %+v, %v; want position 0", a, err)
	}
}

// An EventTool whose event is misspelled fails its tool call; it does not crash the process.
func TestEventTool_UnknownEventFailsTheCallWithoutPanicking(t *testing.T) {
	ctx := context.Background()
	m := buildCounter(t)
	g, err := govern.NewPersistent(ctx, m, govern.NewMemEventLog(), "e", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	tool := govern.EventTool(g, govern.EventToolConfig{Name: "bump", Description: "", Event: "inc_typo", Safety: agent.Safety{Idempotent: true}})
	noPanic(t, "EventTool with an unknown event", func() {
		if _, err := tool.Call(ctx, []byte(`{}`)); !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("tool call err = %v, want ErrConfig", err)
		}
	})
}

// A federated log holding an entry the federation does not know is an error on every fold
// path, never a panic.
func TestFederatedGovernor_BadLogEntryIsAnErrorNotAPanic(t *testing.T) {
	ctx := context.Background()
	m, _, _, _, _ := buildMfrSupFederation(t)
	log := govern.NewMemEventLog()
	early, err := govern.NewFederated(ctx, m, log, "e", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"manufacturer\x1fepub", "manufacturer\x1fnonesuch", "no-separator"} {
		l := govern.NewMemEventLog()
		if _, err := l.Append(ctx, "e", "foreign", entry); err != nil {
			t.Fatal(err)
		}
		noPanic(t, "NewFederated over "+entry, func() {
			_, err := govern.NewFederated(ctx, m, l, "e", m.NewState())
			if entry == "manufacturer\x1fepub" {
				if err != nil {
					t.Fatalf("NewFederated over a good entry: %v", err)
				}
				return
			}
			if !errors.Is(err, agent.ErrProtocol) {
				t.Fatalf("NewFederated over %q: err = %v, want ErrProtocol", entry, err)
			}
		})
	}
	if _, err := log.Append(ctx, "e", "foreign", "manufacturer\x1fnonesuch"); err != nil {
		t.Fatal(err)
	}
	noPanic(t, "federated Sync over a bad log", func() {
		if _, err := early.Sync(ctx); !errors.Is(err, agent.ErrProtocol) {
			t.Fatalf("Sync: err = %v, want ErrProtocol", err)
		}
	})
	noPanic(t, "federated Apply over a bad log", func() {
		if _, err := early.Apply(ctx, "manufacturer", "epub"); !errors.Is(err, agent.ErrProtocol) {
			t.Fatalf("Apply: err = %v, want ErrProtocol", err)
		}
	})
}
