package storetest

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// recorder is a reporter that records failures instead of failing the test.
type recorder struct{ failed []string }

func (r *recorder) Helper() {}
func (r *recorder) Errorf(f string, a ...any) {
	r.failed = append(r.failed, fmt.Sprintf(f, a...))
}
func (r *recorder) Fatalf(f string, a ...any) {
	r.failed = append(r.failed, fmt.Sprintf(f, a...))
	panic(r)
}

func check(wrap func(agent.Store) agent.Store, ctxs ...context.Context) (failed []string) {
	r := &recorder{}
	defer func() {
		if v := recover(); v != nil && v != r {
			panic(v)
		}
		failed = r.failed
	}()
	checkWrapper(r, wrap, ctxs...)
	return r.failed
}

type tenantKey struct{}

// tenantStore maps run IDs by a tenant in the context: a mapping A1 forbids.
type tenantStore struct{ agent.Store }

func tenant(ctx context.Context, runID string) string {
	t, _ := ctx.Value(tenantKey{}).(string)
	return t + "/" + runID
}
func (s tenantStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	return s.Store.Insert(ctx, tenant(ctx, runID), name, data)
}
func (s tenantStore) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	return s.Store.Get(ctx, tenant(ctx, runID), name)
}
func (s tenantStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return s.Store.Load(ctx, tenant(ctx, runID), after)
}

// fixedTenant maps every run ID under one tenant: a mapping a wrapper may make, but only if it
// does not unwrap (see unwrappingTenant).
type fixedTenant struct{ agent.Store }

func (s fixedTenant) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	return s.Store.Insert(ctx, "T/"+runID, name, data)
}
func (s fixedTenant) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	return s.Store.Get(ctx, "T/"+runID, name)
}
func (s fixedTenant) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return s.Store.Load(ctx, "T/"+runID, after)
}

// unwrappingTenant rewrites run IDs and implements Unwrap() Store, which only a wrapper that
// passes keys through may: Capability would expose the wrapped store's Lister and Leaser under the
// wrong keys, and Journals over it would share remembered claims and kept spend with every other
// tenant of the store beneath (the store identity follows Unwrap).
type unwrappingTenant struct{ fixedTenant }

func (w unwrappingTenant) Unwrap() agent.Store { return w.Store }

type passThrough struct{ agent.Store }

func (p passThrough) Unwrap() agent.Store { return p.Store }

func TestCheckWrapper(t *testing.T) {
	a := context.WithValue(context.Background(), tenantKey{}, "A")
	b := context.WithValue(context.Background(), tenantKey{}, "B")
	if f := check(func(s agent.Store) agent.Store { return tenantStore{s} }, a, b); len(f) == 0 {
		t.Error("CheckWrapper passed a wrapper whose keys depend on the context")
	}
	if f := check(func(s agent.Store) agent.Store { return passThrough{s} }, a, b); len(f) != 0 {
		t.Errorf("CheckWrapper failed a pass-through wrapper: %v", f)
	}
	if f := check(func(s agent.Store) agent.Store { return unwrappingTenant{fixedTenant{s}} }, a, b); !slices.ContainsFunc(f, func(m string) bool {
		return strings.Contains(m, "implements Unwrap") && strings.Contains(m, "remembered claims")
	}) {
		t.Errorf("CheckWrapper did not refuse a key-rewriting wrapper that implements Unwrap() Store for that reason: %v", f)
	}
	if f := check(func(s agent.Store) agent.Store { return fixedTenant{s} }, a, b); len(f) != 0 {
		t.Errorf("CheckWrapper failed a key-rewriting wrapper that does not unwrap: %v", f)
	}
	if f := check(func(s agent.Store) agent.Store { return passThrough{s} }); len(f) == 0 {
		t.Error("CheckWrapper with no contexts passed: it cannot check the context rule without two")
	}
}
