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
		sort.Strings(done)
		h.log("relay: delivery %s (%s, run %s) forwarded to %s", short(p.ID), p.Kind, p.RunID, strings.Join(done, ", "))
	}
	return failed
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
