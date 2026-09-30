package storetest

import (
	"context"
	"fmt"
	"iter"
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
	if f := check(func(s agent.Store) agent.Store { return passThrough{s} }); len(f) == 0 {
		t.Error("CheckWrapper with no contexts passed: it cannot check the context rule without two")
	}
}

func checkDurable(wrap func(agent.Durable) agent.Durable, ctxs ...context.Context) (failed []string) {
	r := &recorder{}
	defer func() {
		if v := recover(); v != nil && v != r {
			panic(v)
		}
		failed = r.failed
	}()
	checkDurableWrapper(r, wrap, ctxs...)
	return r.failed
}

// tenantDurable prefixes every run ID with a fixed tenant.
type tenantDurable struct {
	agent.Durable
	tenant string
}

func (w tenantDurable) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return w.Durable.Do(ctx, w.tenant+"/"+runID, name, fn)
}
func (w tenantDurable) History(ctx context.Context, runID string) ([]agent.Record, error) {
	return w.Durable.History(ctx, w.tenant+"/"+runID)
}

// unwrappingTenant is tenantDurable that implements Unwrap() Durable, which the contract forbids.
type unwrappingTenant struct{ tenantDurable }

func (w unwrappingTenant) Unwrap() agent.Durable { return w.Durable }

// passDurable forwards everything unchanged and unwraps: what audit.AuditedStore does with keys.
type passDurable struct{ agent.Durable }

func (p passDurable) Unwrap() agent.Durable { return p.Durable }

// ctxTenantDurable picks the tenant from the context: a mapping the rules forbid.
type ctxTenantDurable struct{ agent.Durable }

func (w ctxTenantDurable) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return w.Durable.Do(ctx, tenant(ctx, runID), name, fn)
}
func (w ctxTenantDurable) History(ctx context.Context, runID string) ([]agent.Record, error) {
	return w.Durable.History(ctx, tenant(ctx, runID))
}

func TestCheckDurableWrapper(t *testing.T) {
	a := context.WithValue(context.Background(), tenantKey{}, "A")
	b := context.WithValue(context.Background(), tenantKey{}, "B")
	if f := checkDurable(func(d agent.Durable) agent.Durable { return unwrappingTenant{tenantDurable{d, "T"}} }, a, b); len(f) == 0 {
		t.Error("CheckDurableWrapper passed a key-rewriting wrapper that implements Unwrap() Durable")
	}
	if f := checkDurable(func(d agent.Durable) agent.Durable { return tenantDurable{d, "T"} }, a, b); len(f) != 0 {
		t.Errorf("CheckDurableWrapper failed a key-rewriting wrapper that does not unwrap: %v", f)
	}
	if f := checkDurable(func(d agent.Durable) agent.Durable { return passDurable{d} }, a, b); len(f) != 0 {
		t.Errorf("CheckDurableWrapper failed a pass-through wrapper: %v", f)
	}
	if f := checkDurable(func(d agent.Durable) agent.Durable { return ctxTenantDurable{d} }, a, b); len(f) == 0 {
		t.Error("CheckDurableWrapper passed a wrapper whose keys depend on the context")
	}
	if f := checkDurable(func(d agent.Durable) agent.Durable { return passDurable{d} }); len(f) == 0 {
		t.Error("CheckDurableWrapper with no contexts passed")
	}
}
