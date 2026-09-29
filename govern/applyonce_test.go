package govern_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

// buildTwoCounters has two independent events, inc_a and inc_b.
func buildTwoCounters(t *testing.T) *gsm.Machine {
	t.Helper()
	r := gsm.NewRegistry("two")
	a, b := r.Int("a", 0, 3), r.Int("b", 0, 3)
	r.DeclInvariant("a_cap", gsm.Le(gsm.V(a), gsm.Lit(2)), gsm.Do(gsm.Set(a, gsm.Lit(2))))
	r.DeclInvariant("b_cap", gsm.Le(gsm.V(b), gsm.Lit(2)), gsm.Do(gsm.Set(b, gsm.Lit(2))))
	r.DeclEvent("inc_a", gsm.Do(gsm.Set(a, gsm.Add(gsm.V(a), gsm.Lit(1)))))
	r.DeclEvent("inc_b", gsm.Do(gsm.Set(b, gsm.Add(gsm.V(b), gsm.Lit(1)))))
	m, rep, err := r.Build()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, rep)
	}
	return m
}

// ApplyOnce applies an id once on every governor: a repeat returns the first result even after
// later events, reusing the id for another event is an ErrConfig error (not a storage failure)
// that applies nothing, and an empty id is rejected.
func TestApplyOnce_OncePerID(t *testing.T) {
	ctx := context.Background()
	m := buildTwoCounters(t)
	pg, err := govern.NewPersistent(ctx, m, govern.NewMemEventLog(), "e", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	for name, g := range map[string]govern.Applier{"in-memory": govern.New(m, m.NewState()), "persistent": pg} {
		first, err := g.ApplyOnce(ctx, "id-1", "inc_a")
		if err != nil || first.Position != 0 {
			t.Fatalf("%s: first ApplyOnce = %+v, %v; want position 0", name, first, err)
		}
		if _, err := g.Apply(ctx, "inc_a"); err != nil {
			t.Fatal(err)
		}
		again, err := g.ApplyOnce(ctx, "id-1", "inc_a")
		if err != nil || again.Position != 0 || again.State.Digest() != first.State.Digest() {
			t.Fatalf("%s: repeated ApplyOnce = %+v, %v; want the first result %+v", name, again, err, first)
		}
		_, err = g.ApplyOnce(ctx, "id-1", "inc_b")
		if !errors.Is(err, agent.ErrConfig) || errors.Is(err, agent.ErrStorage) {
			t.Fatalf("%s: ApplyOnce reusing an id for another event: err = %v, want ErrConfig and not ErrStorage", name, err)
		}
		if _, err := g.ApplyOnce(ctx, "", "inc_a"); !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("%s: ApplyOnce with an empty id: err = %v, want ErrConfig", name, err)
		}
		if a, err := g.Apply(ctx, "inc_b"); err != nil || a.Position != 2 {
			t.Fatalf("%s: Apply after the repeats = %+v, %v; want position 2 (the repeats applied nothing)", name, a, err)
		}
	}
}

// The federated governor keeps the same rules.
func TestFederatedApplyOnce_OncePerID(t *testing.T) {
	ctx := context.Background()
	fm, mfr, sup, _, _ := buildMfrSupFederation(t)
	fg, err := govern.NewFederated(ctx, fm, govern.NewMemEventLog(), "f", fm.NewState())
	if err != nil {
		t.Fatal(err)
	}
	digest := func(st gsm.FedState) string { return fm.Of(st, mfr).Digest() + "/" + fm.Of(st, sup).Digest() }
	first, err := fg.ApplyOnce(ctx, "id-1", "supplier", "eexp")
	if err != nil || first.Position != 0 {
		t.Fatalf("first ApplyOnce = %+v, %v; want position 0", first, err)
	}
	if _, err := fg.Apply(ctx, "manufacturer", "epub"); err != nil {
		t.Fatal(err)
	}
	again, err := fg.ApplyOnce(ctx, "id-1", "supplier", "eexp")
	if err != nil || again.Position != 0 || digest(again.State) != digest(first.State) {
		t.Fatalf("repeated ApplyOnce = %+v, %v; want the first result %+v", again, err, first)
	}
	_, err = fg.ApplyOnce(ctx, "id-1", "manufacturer", "epub")
	if !errors.Is(err, agent.ErrConfig) || errors.Is(err, agent.ErrStorage) {
		t.Fatalf("ApplyOnce reusing an id for another event: err = %v, want ErrConfig and not ErrStorage", err)
	}
}

// flakyReads is a log whose reads fail once fail is set: appends land, but a governor cannot
// compute the state after them.
type flakyReads struct {
	govern.EventLog
	fail bool
}

func (l *flakyReads) Events(ctx context.Context, entity string, from int64) ([]string, error) {
	if l.fail {
		return nil, errors.New("read timed out")
	}
	return l.EventLog.Events(ctx, entity, from)
}

// rewrittenLog breaks the EventLog contract: once rewrite is set, reads return a history whose
// first entry is an event no machine declares.
type rewrittenLog struct {
	govern.EventLog
	rewrite bool
}

func (l *rewrittenLog) Events(ctx context.Context, entity string, from int64) ([]string, error) {
	evs, err := l.EventLog.Events(ctx, entity, from)
	if l.rewrite && err == nil && from == 0 && len(evs) > 0 {
		evs[0] = "rewritten"
	}
	return evs, err
}

// Even a log that rewrites history cannot make a governor panic: rebuilding the state for a
// repeated ApplyOnce reports the bad entry as an error.
func TestApplyOnce_ReplayOfARewrittenLogIsAnError(t *testing.T) {
	ctx := context.Background()
	m := buildCounter(t)
	log := &rewrittenLog{EventLog: govern.NewMemEventLog()}
	g, err := govern.NewPersistent(ctx, m, log, "e", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.ApplyOnce(ctx, "id-1", "inc_a"); err != nil {
		t.Fatal(err)
	}
	log.rewrite = true
	noPanic(t, "ApplyOnce over a rewritten log", func() {
		if _, err := g.ApplyOnce(ctx, "id-1", "inc_a"); !errors.Is(err, agent.ErrProtocol) {
			t.Fatalf("repeated ApplyOnce over a rewritten log: err = %v, want ErrProtocol", err)
		}
	})
}

// When the append lands but the state after it cannot be computed, the error says the event is
// recorded and where, so a caller does not treat it as not applied and apply it again.
func TestApply_RecordedButStateUnknownSaysSo(t *testing.T) {
	ctx := context.Background()
	m := buildCounter(t)
	log := &flakyReads{EventLog: govern.NewMemEventLog()}
	g, err := govern.NewPersistent(ctx, m, log, "e", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.ApplyOnce(ctx, "id-1", "inc_a"); err != nil {
		t.Fatal(err)
	}
	log.fail = true
	recorded := func(what string, err error, pos string) {
		t.Helper()
		if !errors.Is(err, agent.ErrStorage) || !strings.Contains(err.Error(), "recorded at position "+pos) {
			t.Fatalf("%s: err = %v, want ErrStorage saying the event is recorded at position %s", what, err, pos)
		}
	}
	_, err = g.Apply(ctx, "inc_a")
	recorded("Apply whose fold failed", err, "1")
	_, err = g.ApplyOnce(ctx, "id-1", "inc_a")
	recorded("repeated ApplyOnce whose replay failed", err, "0")

	fm, _, _, _, _ := buildMfrSupFederation(t)
	flog := &flakyReads{EventLog: govern.NewMemEventLog()}
	fg, err := govern.NewFederated(ctx, fm, flog, "f", fm.NewState())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fg.ApplyOnce(ctx, "id-1", "supplier", "eexp"); err != nil {
		t.Fatal(err)
	}
	flog.fail = true
	_, err = fg.Apply(ctx, "manufacturer", "epub")
	recorded("federated Apply whose fold failed", err, "1")
	_, err = fg.ApplyOnce(ctx, "id-1", "supplier", "eexp")
	recorded("repeated federated ApplyOnce whose replay failed", err, "0")
}
