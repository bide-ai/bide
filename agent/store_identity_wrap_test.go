package agent

import (
	"context"
	"iter"
	"testing"
)

// valStore is a Store with a value receiver (a struct holding a pointer to its data).
type revValStore struct{ m *MemStore }

func (s revValStore) Insert(ctx context.Context, r, n string, d []byte) (Entry, bool, error) {
	return s.m.Insert(ctx, r, n, d)
}
func (s revValStore) Get(ctx context.Context, r, n string) (Entry, bool, error) {
	return s.m.Get(ctx, r, n)
}
func (s revValStore) Load(ctx context.Context, r string, a int64) iter.Seq2[Entry, error] {
	return s.m.Load(ctx, r, a)
}

type revWrap struct{ inner Store }

func (w *revWrap) Insert(ctx context.Context, r, n string, d []byte) (Entry, bool, error) {
	return w.inner.Insert(ctx, r, n, d)
}
func (w *revWrap) Get(ctx context.Context, r, n string) (Entry, bool, error) {
	return w.inner.Get(ctx, r, n)
}
func (w *revWrap) Load(ctx context.Context, r string, a int64) iter.Seq2[Entry, error] {
	return w.inner.Load(ctx, r, a)
}
func (w *revWrap) Unwrap() Store { return w.inner }

// Two Journals over one pointer wrapper of a value-typed store share its identity: the innermost
// pointer on the Unwrap chain (the value store beneath cannot be compared safely).
func TestStoreIdentity_PointerWrapperOverAValueStore(t *testing.T) {
	w := &revWrap{inner: revValStore{m: NewMemStore()}}
	j1, j2 := newJournal(w), newJournal(w)
	if !sameStore(j1, j2) {
		t.Fatalf("two Journals over the same wrapper %p are not one store (ids %p, %p)", w, j1.id, j2.id)
	}
}
