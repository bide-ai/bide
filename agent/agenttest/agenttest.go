// Package agenttest holds test doubles and helpers for code built on package agent: a scripted
// model that plays back the turns you give it, so an agent runs with no provider or API key
// (ScriptedModel, NewScriptedModel, ToolTurn, TextTurn, ErrorTurn); a Store that counts its round
// trips (CountingStore); and constructors that panic instead of returning an error (MemJournal,
// MustJournal, MustNew, Must), for tests whose setup cannot fail.
package agenttest

import (
	"context"
	"iter"
	"sync"

	"github.com/bide-ai/bide/agent"
)

// Counts is how many round trips a CountingStore served, and how many entries its Loads yielded.
type Counts struct {
	Insert, Get, Load int
	// Inserted is how many Inserts stored their entry (the rest found one already there).
	Inserted int
	// EntriesRead is how many entries every Load yielded, in all.
	EntriesRead int
	// Names lists the name of every Insert and Get, in order, as "insert <name>" or "get <name>".
	Names []string
}

// CountingStore wraps a Store and counts the round trips it serves, for tests that hold the engine
// to a budget of store calls per operation. It passes run IDs and names through unchanged, so it
// implements Unwrap, and agent.Capability finds the wrapped store's Lister and Leaser through it
// (their calls are not counted).
type CountingStore struct {
	inner agent.Store
	mu    sync.Mutex
	c     Counts
}

// NewCountingStore returns a CountingStore over inner.
func NewCountingStore(inner agent.Store) *CountingStore { return &CountingStore{inner: inner} }

// Unwrap returns the wrapped store.
func (s *CountingStore) Unwrap() agent.Store { return s.inner }

// Counts returns the counts so far.
func (s *CountingStore) Counts() Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.c
	c.Names = append([]string(nil), s.c.Names...)
	return c
}

// Reset zeroes the counts and returns the ones it cleared.
func (s *CountingStore) Reset() Counts {
	c := s.Counts()
	s.mu.Lock()
	s.c = Counts{}
	s.mu.Unlock()
	return c
}

// Insert implements agent.Store.
func (s *CountingStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	e, ok, err := s.inner.Insert(ctx, runID, name, data)
	s.mu.Lock()
	s.c.Insert++
	if ok {
		s.c.Inserted++
	}
	s.c.Names = append(s.c.Names, "insert "+name)
	s.mu.Unlock()
	return e, ok, err
}

// Get implements agent.Store.
func (s *CountingStore) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	s.mu.Lock()
	s.c.Get++
	s.c.Names = append(s.c.Names, "get "+name)
	s.mu.Unlock()
	return s.inner.Get(ctx, runID, name)
}

// Load implements agent.Store.
func (s *CountingStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	s.mu.Lock()
	s.c.Load++
	s.mu.Unlock()
	return func(yield func(agent.Entry, error) bool) {
		for e, err := range s.inner.Load(ctx, runID, after) {
			if err == nil {
				s.mu.Lock()
				s.c.EntriesRead++
				s.mu.Unlock()
			}
			if !yield(e, err) {
				return
			}
		}
	}
}
