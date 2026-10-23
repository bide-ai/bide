// Package relay receives bide's signed webhooks and forwards each verified notification to Slack,
// PagerDuty, or any other Sink. It is the library behind bide notify relay: embed Handler in your
// own server, or run the command.
//
// A Handler is the receiver side of the notifier's at-least-once contract. It verifies every
// request with notify.Verify, so a bad, stale or replayed-with-a-changed-ID delivery is refused
// with 401; it answers 2xx only after every sink took the notification, and 502 when one did not,
// which makes bide retry. Because bide may deliver an event more than once (a lost reply, a
// restart, a retry after a 502), it remembers the delivery IDs it has forwarded, in a bounded
// cache, and acknowledges a repeat without forwarding it again. With several sinks it remembers
// which of them already took the event, so a retry after a partial failure reaches only the sink
// that failed.
//
// The relay never forwards error text or tool arguments, even if bide was told to send them: a
// notification that reaches a chat room or a pager has left the access control the viewer applies.
package relay

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/notify"
)

// Defaults of a Handler.
const (
	// DefaultDedupeSize is how many delivery IDs a Handler remembers.
	DefaultDedupeSize = 10000
	// DefaultForwardTimeout bounds one delivery's forwarding. It is below the notifier's own
	// per-request timeout (10 s), so a slow third party makes the relay answer 502 before bide
	// gives up on the request.
	DefaultForwardTimeout = 8 * time.Second
	// DefaultStateFlushDelay is how long a state file write may be deferred and coalesced.
	DefaultStateFlushDelay = time.Second
)

// A Sink takes a verified notification to somewhere else. Send returns nil only once the
// destination accepted it, and may be called again for the same notification after an error, so a
// destination that can dedupe (PagerDuty can) should be given a key derived from the payload.
// Name identifies the sink in the dedupe cache and in logs: it must be unique in a Handler.
type Sink interface {
	Name() string
	Send(ctx context.Context, p notify.Payload) error
}

// Config configures a Handler.
type Config struct {
	// Secrets are the signing secrets to verify with: the current one, and the previous one during a
	// rotation. Required.
	Secrets []string
	// Tolerance is how far a request's timestamp may be from the relay's clock; zero means
	// notify.DefaultTolerance.
	Tolerance time.Duration
	// Sinks are where a verified notification goes. At least one is required.
	Sinks []Sink
	// DedupeSize bounds the delivery IDs remembered; zero means DefaultDedupeSize. When the cache is
	// full the least recently seen ID is forgotten, and an old event redelivered after that is
	// forwarded again: the bound trades memory for a rare repeat, never for a lost notification.
	DedupeSize int
	// ClearsID says whether p is a clear and the ID of the event it ends ("" when it is not); nil
	// means ClearsID. The relay remembers the IDs it saw cleared (as many as DedupeSize) and drops a
	// later notification of such an event instead of forwarding it: a trigger that arrives after its
	// clear (the event was cleared while its request failed, or its reply was lost) would reopen an
	// incident that is over.
	ClearsID func(notify.Payload) string
	// StatePath, if set, is a file the Handler keeps its memory in, so the delivery IDs it forwarded
	// and the event IDs it saw cleared survive a restart: without it a trigger that arrives after a
	// restart, for an event cleared before it, is forwarded and reopens the incident, and a
	// redelivery is forwarded again. The file is written atomically after each change (bounded by
	// DedupeSize, like the in-memory sets), holds IDs and sink names only, and is read at New: a
	// missing file is a first start, an unreadable or invalid one is an error. If a later write
	// fails the Handler logs it and carries on, with its memory in the process as without a file.
	// StateFlushDelay is how long a change to the forwarded-delivery set waits before the file is
	// rewritten, so a burst of events costs one write, not one each (the whole file is rewritten and
	// synced, which grows with DedupeSize). Zero means DefaultStateFlushDelay; negative writes through
	// on every change. A clear is never delayed: its ID is written at once, because a late trigger
	// after a crash is the failure the file exists to prevent. Close writes what is pending. A crash
	// (not Close) can lose the forwarded-delivery entries of the last StateFlushDelay, so a redelivery
	// of those events is forwarded once more: a repeat, never a lost notification.
	StateFlushDelay time.Duration
	// The Handler holds an exclusive operating-system lock on StatePath+".lock" until Close or the
	// process ends (a crash releases it), and New fails if another relay holds it: two relays
	// sharing one file would overwrite each other's memory.
	StatePath string
	// ForwardTimeout bounds the forwarding of one delivery; zero means DefaultForwardTimeout. Forwarding
	// continues after the sender gives up, so a retry finds it done.
	ForwardTimeout time.Duration
	// Logf, if set, receives one line per rejection, duplicate and failure. Lines hold delivery IDs,
	// sink names and error classes; they never hold a secret, a URL or a payload.
	Logf func(format string, args ...any)
}

// A Handler is an http.Handler for bide's webhook requests. Build one with New. It is safe for
// concurrent use.
type Handler struct {
	secrets []string
	tol     time.Duration
	sinks   []Sink
	timeout time.Duration
	logf    func(string, ...any)
	seen    *dedupe
	cleared *idSet
	state   *stateStore
	// pmu guards the pending state write.
	pmu   sync.Mutex
	dirty bool
	// retryDelay, if set, replaces stateRetryDelay; tests shorten it.
	retryDelay time.Duration
	// failures counts the saves that failed in a row, and slowFailures those past the quick retries.
	failures, slowFailures int
	timer                  stopper
	// afterFunc stands in for time.AfterFunc, so a test records the waits and fires them itself.
	afterFunc func(time.Duration, func()) stopper
	closed    bool
	delay     time.Duration
	clears    func(notify.Payload) string
}

// New returns a Handler, or an error for a configuration that cannot work.
func New(c Config) (*Handler, error) {
	live := 0
	for _, s := range c.Secrets {
		if s != "" {
			live++
		}
	}
	if live == 0 {
		return nil, errors.New("relay: Secrets needs at least one non-empty secret")
	}
	for _, s := range c.Secrets {
		if err := notify.CheckSecret(s); err != nil {
			return nil, fmt.Errorf("relay: Secrets: %w (use at least 32 bytes)", err)
		}
	}
	if len(c.Sinks) == 0 {
		return nil, errors.New("relay: Sinks needs at least one sink")
	}
	names := map[string]bool{}
	for _, s := range c.Sinks {
		if s == nil || s.Name() == "" || names[s.Name()] {
			return nil, errors.New("relay: every sink needs a distinct, non-empty Name")
		}
		names[s.Name()] = true
	}
	h := &Handler{
		secrets: append([]string(nil), c.Secrets...),
		tol:     c.Tolerance,
		sinks:   append([]Sink(nil), c.Sinks...),
		timeout: c.ForwardTimeout,
		logf:    c.Logf,
		seen:    newDedupe(c.DedupeSize),
		cleared: newIDSet(c.DedupeSize),
		clears:  c.ClearsID,
	}
	if c.StatePath != "" {
		h.state = &stateStore{path: c.StatePath, syncDir: syncDir}
		h.delay = c.StateFlushDelay
		if h.delay == 0 {
			h.delay = DefaultStateFlushDelay
		}
		if err := h.state.acquire(); err != nil {
			return nil, err
		}
		if err := h.state.load(h.seen, h.cleared); err != nil {
			h.state.release()
			return nil, err
		}
		if err := h.state.save(h.seen, h.cleared, true); err != nil { // fail now if it cannot be written
			h.state.release()
			return nil, err
		}
	}
	if h.clears == nil {
		h.clears = ClearsID
	}
	if h.timeout <= 0 {
		h.timeout = DefaultForwardTimeout
	}
	return h, nil
}

func (h *Handler) log(format string, args ...any) {
	if h.logf != nil {
		h.logf(format, args...)
	}
}

// ServeHTTP verifies the request and forwards it.
//
//	401  the signature, timestamp or delivery ID did not verify (one answer for all of them)
//	400  verified, but not a bide.notification.v1 document
//	405  not a POST
//	502  a sink did not take the notification: bide retries
//	503  the same delivery is being forwarded by another request: bide retries
//	204  every sink has the notification (including a repeat that was already forwarded)
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := notify.Verify(r, h.secrets, h.tol); err != nil {
		h.log("relay: rejected a delivery: %s", RejectionClass(err))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var p notify.Payload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Schema != notify.Schema {
		h.log("relay: rejected a verified delivery %s: not a %s document", short(r.Header.Get(notify.HeaderDelivery)), notify.Schema)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if id := h.clears(p); id != "" {
		if h.cleared.add(id) { // before forwarding: a retry of this clear must still guard the event
			h.persist(true)
		}
	} else if h.cleared.has(p.ID) {
		h.log("relay: delivery %s (%s, run %s) dropped: already cleared", short(p.ID), p.Kind, p.RunID)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	todo, st := h.seen.begin(p.ID, h.sinks)
	switch st {
	case stateDone:
		h.log("relay: delivery %s is a repeat of one already forwarded; acknowledged", short(p.ID))
		w.WriteHeader(http.StatusNoContent)
		return
	case stateBusy:
		h.log("relay: delivery %s is being forwarded by another request; asked the sender to retry", short(p.ID))
		w.Header().Set("Retry-After", "1")
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	// Forwarding outlives the request: if bide gives up first, the work still finishes and marks
	// the delivery done, so bide's retry is acknowledged as a repeat instead of posting twice.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), h.timeout)
	defer cancel()
	failed := h.forward(ctx, p, todo)
	if len(failed) > 0 {
		http.Error(w, "forwarding failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// forward sends p to each sink in todo concurrently, records the ones that took it, and returns
// the names of those that did not.
func (h *Handler) forward(ctx context.Context, p notify.Payload, todo []Sink) []string {
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		done   []string
		failed []string
	)
	for _, s := range todo {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := safeSend(ctx, s, p)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed = append(failed, s.Name())
				h.log("relay: delivery %s: %s failed: %v", short(p.ID), s.Name(), err)
				return
			}
			done = append(done, s.Name())
		}()
	}
	wg.Wait()
	h.seen.end(p.ID, done)
	if len(done) > 0 {
		h.persist(false)
	}
	if len(done) > 0 {
		sort.Strings(done)
		h.log("relay: delivery %s (%s, run %s) forwarded to %s", short(p.ID), p.Kind, p.RunID, strings.Join(done, ", "))
	}
	return failed
}

// Close writes any pending state and releases the state file's lock, so another relay may use the
// file. A Handler without a StatePath has nothing to do. The lock is also released when the process
// exits.
func (h *Handler) Close() error {
	if h.state == nil {
		return nil
	}
	h.pmu.Lock()
	h.closed = true
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	var err error
	if h.dirty {
		h.dirty = false
		err = h.state.save(h.seen, h.cleared, true)
	}
	h.pmu.Unlock()
	if rerr := h.state.release(); err == nil {
		err = rerr
	}
	return err
}

// crash drops the lock and any pending write without saving, as a killed process does. Tests only.
func (h *Handler) crash() {
	h.pmu.Lock()
	h.closed = true
	if h.timer != nil {
		h.timer.Stop()
	}
	h.dirty = false
	h.pmu.Unlock()
	h.state.release()
}

// persist saves the state file, if there is one: now when urgent or when the delay is negative,
// else within StateFlushDelay together with whatever else changes by then. A failure is logged,
// never fatal: a notification must still go out.
func (h *Handler) persist(urgent bool) {
	if h.state == nil {
		return
	}
	h.pmu.Lock()
	defer h.pmu.Unlock()
	if h.closed {
		return
	}
	if urgent || h.delay < 0 {
		h.dirty = false
		h.saveLocked()
		return
	}
	h.dirty = true
	if h.timer == nil {
		h.timer = h.after(h.delay)
	}
}

// stopper is what a pending write's timer offers: *time.Timer, or a test's.
type stopper interface{ Stop() bool }

// after schedules flush in d, on the clock the Handler has.
func (h *Handler) after(d time.Duration) stopper {
	if h.afterFunc != nil {
		return h.afterFunc(d, h.flush)
	}
	return time.AfterFunc(d, h.flush)
}

func (h *Handler) flush() {
	h.pmu.Lock()
	defer h.pmu.Unlock()
	h.timer = nil
	if h.closed || !h.dirty {
		return
	}
	h.dirty = false
	h.saveLocked()
}

const (
	// stateRetryDelay is the least wait before a failed save is tried again once the quick retries are spent,
	// and it doubles with each failure up to stateRetryMax.
	stateRetryDelay = time.Second
	stateRetryMax   = 30 * time.Second
	// A refusal that may pass (a reader holding the file on Windows) is retried at once on a timer: 5 ms
	// doubling, up to transientRetries times (about 300 ms in all), without logging.
	transientRetries = 6
	transientBase    = 5 * time.Millisecond
)

// backoffWait is base doubled slow times, never more than stateRetryMax (and never negative: a large base shifted
// left would overflow, so it doubles one step at a time and stops at the cap).
func backoffWait(base time.Duration, slow int) time.Duration {
	d := min(base, stateRetryMax)
	for i := 0; i < slow && d < stateRetryMax; i++ {
		d = min(d*2, stateRetryMax)
	}
	return d
}

// saveLocked saves the state, with h.pmu held, in one attempt, so nothing waits behind a sleep. A save that fails
// leaves the state dirty with a retry on a timer, so it is written when the file can be written again, not only
// when the next event changes it (Close writes it too): a refusal that may pass is retried within milliseconds
// and not logged; anything else, or one that does not pass, is logged once per streak of failures and retried
// after the flush delay (at least a second), doubling to 30 seconds. The save that ends a streak is logged.
func (h *Handler) saveLocked() {
	err := h.state.save(h.seen, h.cleared, false)
	if err == nil {
		if h.slowFailures > 0 {
			h.log("relay: the state file was written again after %d failed attempts", h.failures)
		}
		h.failures, h.slowFailures = 0, 0
		return
	}
	h.dirty = true
	h.failures++
	var after time.Duration
	if h.failures <= h.state.quickRetries(err) {
		after = transientBase << (h.failures - 1)
	} else {
		floor := h.retryDelay
		if floor <= 0 {
			floor = stateRetryDelay
		}
		after = backoffWait(max(h.delay, floor), h.slowFailures)
		if h.slowFailures == 0 {
			h.log("relay: %v (the memory of forwarded and cleared events is in the process only until this works; retrying, with a growing wait)", err)
		}
		h.slowFailures++
	}
	if h.closed {
		return
	}
	if h.timer != nil {
		h.timer.Stop()
	}
	h.timer = h.after(after)
}

// safeSend turns a panicking sink into an error, so one bad sink cannot leave a delivery marked
// in flight forever.
func safeSend(ctx context.Context, s Sink, p notify.Payload) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("relay: %s panicked", s.Name())
		}
	}()
	return s.Send(ctx, p)
}

// RejectionClass names which check a failed notify.Verify failed, for a log line. The sender is
// told only "unauthorized".
func RejectionClass(err error) string {
	switch {
	case errors.Is(err, notify.ErrNoSignature):
		return "missing signature or timestamp"
	case errors.Is(err, notify.ErrBadTimestamp):
		return "malformed timestamp"
	case errors.Is(err, notify.ErrStale):
		return "timestamp outside the tolerance"
	case errors.Is(err, notify.ErrBadSignature):
		return "signature does not match"
	case errors.Is(err, notify.ErrDeliveryID):
		return "delivery ID missing or not the signed id"
	case errors.Is(err, notify.ErrBodyTooLarge):
		return "body too large"
	case errors.Is(err, notify.ErrShortSecret):
		return "a configured secret is too short for FIPS mode"
	default:
		return "unreadable request"
	}
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

type state int

const (
	stateRun state = iota
	stateDone
	stateBusy
)

// dedupe remembers, per delivery ID, which sinks have taken the event, and which request is
// forwarding it now. It is a bounded LRU: only the IDs it holds are remembered.
type dedupe struct {
	mu  sync.Mutex
	max int
	ll  *list.List // front is the most recently used
	m   map[string]*list.Element
}

type entry struct {
	id   string
	done map[string]bool
	busy bool
}

func newDedupe(max int) *dedupe {
	if max <= 0 {
		max = DefaultDedupeSize
	}
	return &dedupe{max: max, ll: list.New(), m: map[string]*list.Element{}}
}

// begin claims id for forwarding. It returns the sinks that still need the event with stateRun; with
// stateDone every sink has it already, and with stateBusy another request holds the claim.
func (d *dedupe) begin(id string, sinks []Sink) ([]Sink, state) {
	d.mu.Lock()
	defer d.mu.Unlock()
	el, ok := d.m[id]
	if !ok {
		el = d.ll.PushFront(&entry{id: id, done: map[string]bool{}})
		d.m[id] = el
		d.evict()
	} else {
		d.ll.MoveToFront(el)
	}
	e := el.Value.(*entry)
	if e.busy {
		return nil, stateBusy
	}
	var todo []Sink
	for _, s := range sinks {
		if !e.done[s.Name()] {
			todo = append(todo, s)
		}
	}
	if len(todo) == 0 {
		return nil, stateDone
	}
	e.busy = true
	return todo, stateRun
}

// end releases the claim on id and records the sinks that took the event.
func (d *dedupe) end(id string, done []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	el, ok := d.m[id]
	if !ok {
		return
	}
	e := el.Value.(*entry)
	e.busy = false
	for _, n := range done {
		e.done[n] = true
	}
}

// evict drops the least recently used entries beyond the bound, never one being forwarded (it
// holds a claim that end must find) and never the front one, which the caller is about to claim.
func (d *dedupe) evict() {
	for el := d.ll.Back(); el != nil && d.ll.Len() > d.max; {
		prev := el.Prev()
		if e := el.Value.(*entry); !e.busy && el != d.ll.Front() {
			d.ll.Remove(el)
			delete(d.m, e.id)
		}
		el = prev
	}
}

// idSet is a bounded set of IDs; adding past the bound forgets the least recently added.
type idSet struct {
	mu  sync.Mutex
	max int
	ll  *list.List
	m   map[string]*list.Element
}

func newIDSet(max int) *idSet {
	if max <= 0 {
		max = DefaultDedupeSize
	}
	return &idSet{max: max, ll: list.New(), m: map[string]*list.Element{}}
}

// add records id and reports whether it was not there before.
func (s *idSet) add(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.m[id]; ok {
		s.ll.MoveToFront(el)
		return false
	}
	s.m[id] = s.ll.PushFront(id)
	for s.ll.Len() > s.max {
		el := s.ll.Back()
		s.ll.Remove(el)
		delete(s.m, el.Value.(string))
	}
	return true
}

// snapshot is the IDs, oldest first.
func (s *idSet) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, s.ll.Len())
	for el := s.ll.Back(); el != nil; el = el.Prev() {
		out = append(out, el.Value.(string))
	}
	return out
}

func (s *idSet) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id] != nil
}

// snapshot is the entries with at least one sink done, oldest first. An entry with none has
// nothing to remember.
func (d *dedupe) snapshot() []seenState {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]seenState, 0, d.ll.Len())
	for el := d.ll.Back(); el != nil; el = el.Prev() {
		e := el.Value.(*entry)
		if len(e.done) == 0 {
			continue
		}
		names := make([]string, 0, len(e.done))
		for n := range e.done {
			names = append(names, n)
		}
		sort.Strings(names)
		out = append(out, seenState{ID: e.id, Done: names})
	}
	return out
}

// restore adds an entry loaded from a state file as the most recent.
func (d *dedupe) restore(id string, done []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e := &entry{id: id, done: map[string]bool{}}
	if el, ok := d.m[id]; ok { // a duplicate in the file: one entry, with the sinks of both, most recent
		e = el.Value.(*entry)
		d.ll.MoveToFront(el)
	} else {
		d.m[id] = d.ll.PushFront(e)
	}
	for _, n := range done {
		e.done[n] = true
	}
	d.evict()
}
