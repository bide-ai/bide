package agent

import (
	"context"
	"testing"
)

// unwrapStore wraps inner and exposes it through Unwrap, implementing nothing else.
type unwrapStore struct{ inner Durable }

func (u unwrapStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	return u.inner.Do(ctx, runID, name, fn)
}
func (u unwrapStore) History(ctx context.Context, runID string) ([]Record, error) {
	return u.inner.History(ctx, runID)
}
func (u unwrapStore) Unwrap() Durable { return u.inner }

// listingWrapper overrides Lister itself, so it must take precedence over the inner store's.
type listingWrapper struct{ unwrapStore }

func (listingWrapper) Runs(context.Context) ([]string, error) { return []string{"renamed"}, nil }

func TestCapability(t *testing.T) {
	mem := NewMemStore()

	if l, ok := Capability[Leaser](mem); !ok || l != Leaser(mem) {
		t.Fatalf("Capability[Leaser](MemStore) = %v, %v; want the store itself", l, ok)
	}
	// Two wrappers deep: the lookup follows every Unwrap to the store that has it.
	deep := unwrapStore{unwrapStore{mem}}
	if l, ok := Capability[Leaser](deep); !ok || l != Leaser(mem) {
		t.Fatalf("Capability[Leaser] through two wrappers = %v, %v; want the inner MemStore", l, ok)
	}
	if l, ok := Capability[Lister](deep); !ok || l != Lister(mem) {
		t.Fatalf("Capability[Lister] through two wrappers = %v, %v; want the inner MemStore", l, ok)
	}
	// No more capabilities than the wrapped store: a store without Lister stays without one.
	if _, ok := Capability[Lister](unwrapStore{noListStore{}}); ok {
		t.Fatal("a wrapper over a non-Lister reports a Lister")
	}
	// Unwrap returning nil ends the search instead of panicking.
	if _, ok := Capability[Leaser](unwrapStore{nil}); ok {
		t.Fatal("a wrapper whose Unwrap returns nil reports a Leaser")
	}
	if _, ok := Capability[Leaser](nil); ok {
		t.Fatal("a nil store reports a Leaser")
	}
	// A wrapper that implements the capability itself wins over the store it wraps.
	lw := listingWrapper{unwrapStore{mem}}
	l, ok := Capability[Lister](lw)
	if !ok {
		t.Fatal("the overriding wrapper reports no Lister")
	}
	if runs, _ := l.Runs(context.Background()); len(runs) != 1 || runs[0] != "renamed" {
		t.Fatalf("Capability[Lister] returned the inner store's Lister (runs %v), want the wrapper's", runs)
	}
}
