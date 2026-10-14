// Package filegate queues the writers in one process that share a database file, first come
// first served.
//
// SQLite lets one writer at a time hold a file's write lock, and a writer that finds it taken
// waits in SQLite's busy handler: a sleep, then another try, with no queue. A writer that just
// released the lock tries again at once and usually wins it back from the writers asleep in the
// handler, so under a burst of writers a single unlucky one can wait for everyone else's whole
// run, which a long busy_timeout only postpones failing on. A gate in front of the write makes the
// writers in one process take turns, so the number of writers that contend for the file is the
// number of processes, and a slow disk (where each write is long) cannot starve one of them.
//
// A gate does not order writers in different processes: they still contend through SQLite.
package filegate

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
)

var (
	mu    sync.Mutex
	gates = map[string]*gate{}
)

// A gate is a one-slot channel: senders blocked on a full channel are woken in the order they
// blocked, which is the queue.
type gate struct {
	slot chan struct{}
	refs int // handles holding the gate; the gate leaves the registry with the last
}

// A Gate is a handle on the queue of one database file.
type Gate struct {
	key  string
	g    *gate
	once sync.Once
}

// For returns the gate of the database file at path, shared by every handle in the process that
// opens the same file. An in-memory database (":memory:" or a "mode=memory" URI) is never shared
// between handles, so its gate admits writers without queueing. Release the handle with Close.
func For(path string) *Gate {
	if path == ":memory:" || path == "" || strings.Contains(path, "mode=memory") {
		return &Gate{}
	}
	key := key(path)
	mu.Lock()
	defer mu.Unlock()
	g, ok := gates[key]
	if !ok {
		g = &gate{slot: make(chan struct{}, 1)}
		gates[key] = g
	}
	g.refs++
	return &Gate{key: key, g: g}
}

// key names the file at path, so that two spellings of it share a gate. A path that cannot be
// resolved (the file does not exist yet) is cleaned and made absolute as far as it can be.
func key(path string) string {
	path, _, _ = strings.Cut(strings.TrimPrefix(path, "file:"), "?")
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(dir, filepath.Base(path))
	}
	return path
}

// Enter waits for this handle's turn to write and returns the function that ends it, which the
// caller must call exactly once. It returns ctx's error if ctx ends first, without taking a turn.
func (h *Gate) Enter(ctx context.Context) (leave func(), err error) {
	if h == nil || h.g == nil {
		return func() {}, ctx.Err()
	}
	select {
	case h.g.slot <- struct{}{}:
		return func() { <-h.g.slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close drops the handle; the gate is forgotten once every handle on it is closed. Closing a
// handle twice is the same as once. A closed handle still admits writers.
func (h *Gate) Close() {
	if h == nil || h.g == nil {
		return
	}
	h.once.Do(func() {
		mu.Lock()
		defer mu.Unlock()
		if h.g.refs--; h.g.refs == 0 {
			delete(gates, h.key)
		}
	})
}
