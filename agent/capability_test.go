package agent

import (
	"context"
	"iter"
	"testing"
)

// unwrapStore wraps inner and exposes it through Unwrap, implementing nothing else.
type unwrapStore struct{ inner Store }

func (u unwrapStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	return u.inner.Insert(ctx, runID, name, data)
}
func (u unwrapStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	return u.inner.Get(ctx, runID, name)
}
func (u unwrapStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	return u.inner.Load(ctx, runID, after)
}
func (u unwrapStore) Unwrap() Store { return u.inner }

// listingWrapper overrides Lister itself, so it must take precedence over the inner store's.
type listingWrapper struct{ unwrapStore }

func (listingWrapper) Runs(context.Context, RunFilter) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) { yield("renamed", nil) }
}

// plainStore is a Store with no capabilities.
type plainStore struct{ Store }

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
	if _, ok := Capability[Lister](unwrapStore{plainStore{mem}}); ok {
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
	var runs []string
	for id := range l.Runs(context.Background(), RunFilter{}) {
		runs = append(runs, id)
	}
	if len(runs) != 1 || runs[0] != "renamed" {
		t.Fatalf("Capability[Lister] returned the inner store's Lister (runs %v), want the wrapper's", runs)
	}
}

// durableWrapper is the transitional form of a wrapper: a Durable that exposes the Durable it
// wraps through Unwrap.
type durableWrapper struct{ inner Durable }

func (u durableWrapper) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	return u.inner.Do(ctx, runID, name, fn)
}
func (u durableWrapper) History(ctx context.Context, runID string) ([]Record, error) {
	return u.inner.History(ctx, runID)
}
func (u durableWrapper) Unwrap() Durable { return u.inner }

// The engine finds a capability behind a Durable: on the Durable itself, on the store a Journal
// writes to, and through a Durable wrapper's Unwrap.
func TestCapabilityOfDurable(t *testing.T) {
	mem := NewMemStore()
	j, err := NewJournal(unwrapStore{mem})
	if err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]Durable{
		"MemStore":                         mem,
		"Journal over a wrapper":           j,
		"Durable wrapper":                  durableWrapper{mem},
		"Durable wrapper over the Journal": durableWrapper{j},
	} {
		if l, ok := capabilityOf[Leaser](d); !ok || l != Leaser(mem) {
			t.Errorf("%s: capabilityOf[Leaser] = %v, %v; want the MemStore", name, l, ok)
		}
	}
	if _, ok := capabilityOf[Leaser](durableWrapper{nil}); ok {
		t.Error("a Durable wrapper whose Unwrap returns nil reports a Leaser")
	}
	if _, ok := capabilityOf[Leaser](nil); ok {
		t.Error("a nil Durable reports a Leaser")
	}
}
