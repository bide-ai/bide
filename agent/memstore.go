package agent

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
	"time"
)

// MemStore is an in-memory Store for tests and local development. It also implements Lister and
// Leaser (leases are in-process), so Recover and Lease work on it as on the SQL stores. SQLite
// (store/sqlite) is the on-disk default and store/postgres the high-availability backend.
//
// Each run's entries get consecutive Seq values from 0, in insertion order, so MemStore meets the
// Store requirements trivially: an Insert is atomic under its mutex, and a read sees every entry
// inserted before it.
type MemStore struct {
	mu     sync.Mutex
	runs   map[string]*memRun
	holds  map[string]Hold     // active legal holds by ID (see Holder)
	leases map[string]memLease // run leasing (see lease.go); in-process, for tests and the reference
	now    func() time.Time    // lease clock (settable in tests); defaults to time.Now
}

type memRun struct {
	entries []Entry // in Seq order; an entry is never modified once appended, and a redaction replaces the slice (see Redact)
	byName  map[string]int
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{runs: map[string]*memRun{}, leases: map[string]memLease{}, now: time.Now}
}

// init makes the zero MemStore usable. The caller holds m.mu.
func (m *MemStore) init() {
	if m.runs == nil {
		m.runs = map[string]*memRun{}
	}
	if m.leases == nil {
		m.leases = map[string]memLease{}
	}
	if m.now == nil {
		m.now = time.Now
	}
}

// setNow replaces the lease clock under the mutex that guards its reads, so a test can install a
// controlled clock without racing the lease methods. Test-only; production uses time.Now.
func (m *MemStore) setNow(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.now = now
}

var (
	_ Store    = (*MemStore)(nil)
	_ Lister   = (*MemStore)(nil)
	_ Leaser   = (*MemStore)(nil)
	_ Redactor = (*MemStore)(nil)
	_ Holder   = (*MemStore)(nil)
)

// Insert implements Store.
func (m *MemStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	data = bytes.Clone(data) // outside the mutex: the critical section only places the entry
	m.mu.Lock()
	m.init()
	r := m.runs[runID]
	if r == nil {
		r = &memRun{byName: map[string]int{}}
		m.runs[runID] = r
	}
	if i, ok := r.byName[name]; ok {
		e := r.entries[i]
		m.mu.Unlock()
		return cloneEntry(e), false, nil // an entry is never modified once appended
	}
	e := Entry{Seq: int64(len(r.entries)), Name: name, Data: data}
	r.byName[name] = len(r.entries)
	r.entries = append(r.entries, e)
	m.mu.Unlock()
	return cloneEntry(e), true, nil
}

// Get implements Store.
func (m *MemStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	m.mu.Lock()
	var e Entry
	ok := false
	if r := m.runs[runID]; r != nil {
		var i int
		if i, ok = r.byName[name]; ok {
			e = r.entries[i]
		}
	}
	m.mu.Unlock()
	if !ok {
		return Entry{}, false, nil
	}
	return cloneEntry(e), true, nil // outside the mutex: an entry is never modified once appended
}

// Load implements Store. It takes the run's entries as of the call and yields them without
// holding the store's mutex, so the caller may write to the store inside the loop.
func (m *MemStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(Entry{}, err)
			return
		}
		m.mu.Lock()
		var snap []Entry
		if r := m.runs[runID]; r != nil {
			snap = r.entries[:len(r.entries):len(r.entries)] // appended entries never move
		}
		m.mu.Unlock()
		for _, e := range snap {
			if e.Seq <= after {
				continue
			}
			if !yield(cloneEntry(e), nil) {
				return
			}
		}
	}
}

func cloneEntry(e Entry) Entry { e.Data = bytes.Clone(e.Data); return e }

// Runs implements Lister: the runs f admits, in ascending order.
func (m *MemStore) Runs(ctx context.Context, f RunFilter) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		if err := ctx.Err(); err != nil {
			yield("", err)
			return
		}
		m.mu.Lock()
		m.init()
		now := m.now()
		ids := make([]string, 0, len(m.runs))
		for id, r := range m.runs {
			if f.Admits(id, func(name string) bool { _, ok := r.byName[name]; return ok }, func() bool { return m.lapsed(id, now) }) {
				ids = append(ids, id)
			}
		}
		m.mu.Unlock()
		slices.Sort(ids)
		for _, id := range ids {
			if !yield(id, nil) {
				return
			}
		}
	}
}

// Redact implements Redactor. A reader that took a snapshot of the run's entries (Load) keeps
// reading the entries as they were: the replacement is a new slice, never a write into the old one.
func (m *MemStore) Redact(ctx context.Context, runID, name string, current, tombstone []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := CheckRedaction(runID, name, current, tombstone); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	var st RedactState
	r := m.runs[runID]
	var i int
	if r != nil {
		i, st.Found = r.byName[name]
	}
	if st.Found {
		st.Row = r.entries[i]
		for _, end := range endOfRunMarkers {
			if _, ok := r.byName[end]; ok {
				st.Ended = true
			}
		}
		if cur, ok := m.leases[runID]; ok && m.now().Before(cur.expiry) {
			st.Leased = true
		}
		for _, anc := range RedactionAncestors(runID) {
			if a := m.runs[anc]; a != nil && len(a.entries) > 0 {
				over := false
				for _, end := range endOfRunMarkers {
					if _, ok := a.byName[end]; ok {
						over = true
					}
				}
				st.ParentLive = st.ParentLive || !over
			}
		}
		for _, h := range m.holds {
			st.Held = st.Held || h.Covers(runID)
		}
		if bytes.Equal(st.Row.Data, current) && st.Ended && !st.Leased && !st.ParentLive && !st.Held {
			entries := slices.Clone(r.entries)
			entries[i].Data = bytes.Clone(tombstone)
			r.entries = entries
			return true, nil
		}
	}
	return false, st.Explain(runID, name, current, tombstone)
}

// PlaceHold implements Holder.
func (m *MemStore) PlaceHold(ctx context.Context, h Hold) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckHold(h); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	if _, ok := m.holds[h.ID]; ok {
		return fmt.Errorf("hold %q: %w", h.ID, ErrHoldExists)
	}
	if m.holds == nil {
		m.holds = map[string]Hold{}
	}
	h.Runs = slices.Sorted(slices.Values(h.Runs))
	h.Runs = slices.Compact(h.Runs)
	h.PlacedAtMs = m.now().UnixMilli()
	m.holds[h.ID] = h
	return nil
}

// LiftHold implements Holder.
func (m *MemStore) LiftHold(ctx context.Context, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.holds[id]
	delete(m.holds, id)
	return ok, nil
}

// Holds implements Holder.
func (m *MemStore) Holds(ctx context.Context) ([]Hold, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Hold, 0, len(m.holds))
	for _, h := range m.holds {
		h.Runs = slices.Clone(h.Runs)
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b Hold) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}
