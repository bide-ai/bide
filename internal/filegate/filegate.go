// Package filegate queues the writers in one process that share a database file, first come
// first served, with lease statements ahead of the rest.
//
// SQLite lets one writer at a time hold a file's write lock, and a writer that finds it taken
// waits in SQLite's busy handler: a sleep, then another try, with no queue. A writer that just
// released the lock tries again at once and usually wins it back from the writers asleep in the
// handler, so under a burst of writers a single unlucky one can wait for everyone else's whole
// run, which a long busy_timeout only postpones failing on. A gate in front of the write makes the
// writers in one process take turns, so the number of writers that contend for the file is the
// number of processes, and a slow disk (where each write is long) cannot starve one of them.
//
// A lease renewal has a deadline (a fraction of the lease's TTL) that an ordinary write does not,
// so EnterFirst puts it ahead of every waiting ordinary writer; it waits only for the writer
// already inside.
//
// Handles are matched by the identity of the file (device and inode, or volume and file index), so
// every spelling of a path (a relative path, a symlink, a different case on a case-insensitive
// file system) shares one gate. A gate does not order writers in different processes: they still
// contend through SQLite.
package filegate

import (
	"container/list"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	mu    sync.Mutex
	gates = map[string]*gate{}
)

// A gate admits one writer at a time. Waiters sit in one of two FIFO lists; a leaving writer hands
// the gate to the first first-class waiter, else to the first ordinary one.
type gate struct {
	mu          sync.Mutex
	held        bool
	first, rest list.List // of chan struct{}, closed when the waiter is admitted
	refs        int       // handles holding the gate; the gate leaves the registry with the last
}

// A Gate is a handle on the queue of one database file.
type Gate struct {
	key  string
	g    *gate
	once sync.Once
}

// For returns the gate of the database file at path, shared by every handle in the process that
// opens the same file. A file that does not exist yet is created empty first (SQLite takes an
// empty file for a new database), so that it has an identity; if it cannot be, the gate is keyed
// by the cleaned absolute path. An in-memory database (":memory:" or a "mode=memory" URI) is never
// shared between handles, so its gate admits writers without queueing. Release the handle with
// Close.
func For(path string) *Gate {
	if path == ":memory:" || path == "" || strings.Contains(path, "mode=memory") {
		return &Gate{}
	}
	key := key(path)
	mu.Lock()
	defer mu.Unlock()
	g, ok := gates[key]
	if !ok {
		g = &gate{}
		gates[key] = g
	}
	g.refs++
	return &Gate{key: key, g: g}
}

// key names the file at path by its identity, or by its path when it has none.
func key(path string) string {
	path, _, _ = strings.Cut(strings.TrimPrefix(path, "file:"), "?")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644); err == nil {
			f.Close()
		}
	}
	if id, ok := identity(path); ok {
		return "id:" + id
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(dir, filepath.Base(path))
	}
	return "path:" + path
}

// Enter waits for this handle's turn to write and returns the function that ends it, which the
// caller must call exactly once. It returns ctx's error if ctx ends first, without taking a turn.
func (h *Gate) Enter(ctx context.Context) (leave func(), err error) { return h.enter(ctx, false) }

// EnterFirst is Enter for a statement with a deadline: it is admitted ahead of every writer waiting
// in Enter, behind only the writer inside and the EnterFirst callers that asked before it.
func (h *Gate) EnterFirst(ctx context.Context) (leave func(), err error) { return h.enter(ctx, true) }

func (h *Gate) enter(ctx context.Context, first bool) (func(), error) {
	if h == nil || h.g == nil {
		return func() {}, ctx.Err()
	}
	g := h.g
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if !g.held {
		g.held = true
		g.mu.Unlock()
		return g.leave, nil
	}
	ch := make(chan struct{})
	q := &g.rest
	if first {
		q = &g.first
	}
	el := q.PushBack(ch)
	g.mu.Unlock()
	select {
	case <-ch:
		return g.leave, nil
	case <-ctx.Done():
		g.mu.Lock()
		select {
		case <-ch: // admitted as the context ended: pass the turn on
			g.mu.Unlock()
			g.leave()
		default:
			q.Remove(el)
			g.mu.Unlock()
		}
		return nil, ctx.Err()
	}
}

// Waiting is the number of writers queued behind the one inside, in Enter and EnterFirst. A test
// uses it to hold the gate until every writer it started is queued.
func (h *Gate) Waiting() int {
	if h == nil || h.g == nil {
		return 0
	}
	h.g.mu.Lock()
	defer h.g.mu.Unlock()
	return h.g.first.Len() + h.g.rest.Len()
}

// leave ends a turn and admits the next waiter.
func (g *gate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, q := range []*list.List{&g.first, &g.rest} {
		if el := q.Front(); el != nil {
			close(q.Remove(el).(chan struct{}))
			return // the gate stays held, now by that waiter
		}
	}
	g.held = false
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
