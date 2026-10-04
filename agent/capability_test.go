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

// The engine finds a capability on the store a Journal writes to, directly and through a wrapper's
// Unwrap.
func TestCapabilityOfJournalStore(t *testing.T) {
	mem := NewMemStore()
	j2 := mustJournal(mem)
	j, err := NewJournal(unwrapStore{mem})
	if err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]*Journal{
		"MemStore":               j2,
		"Journal over a wrapper": j,
	} {
		if l, ok := Capability[Leaser](d.Store()); !ok || l != Leaser(mem) {
			t.Errorf("%s: Capability[Leaser] = %v, %v; want the MemStore", name, l, ok)
		}
	}
}
