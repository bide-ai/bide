// Package capledger is the cap ledger of docs/design/untrusted-content-and-amount-caps.md, section
// 6.5: dense, insert-once slots in a reserved journal run, one ledger per window, each slot a reserve
// or a release carrying cum_after, the window's total after it. It is the code of model 17
// (spec/tla/caps), whose actions the protocol:caps regions name, and it holds nothing the model does
// not: a reservation reads the cap-intent (or writes it, after the clock check and the index
// insert), reads the ledger, looks for a live slot of the attempt, checks the limit and inserts slot
// n+1, and a loser of the insert reads again.
//
// It sits over a small Store, not over agent's Journal, so the stores' own test suites (the nested
// modules store/sqlite and store/postgres) can run it across real processes; the agent package
// adapts a Journal to it (agent/capledger.go). Nothing outside the tests calls it yet: no exported
// API of bide reaches it.
//
// The ledger relies on two things of the store, and nothing else (model 17's A1 and A2): an Insert of
// a taken name fails and returns the entry that holds the name (one winner per name), and a Scan
// returns a prefix of the run in commit order, so a slot is never visible before the one below it.
// It never relies on a lease, a clock agreement between processes, or a process staying alive. A
// store that fails is a failure of the call: every error is returned, so a cap is fail closed.
package capledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/internal/amount"
)

// RunPrefix starts the ID of every ledger and index run (agent's capRunPrefix is the same string).
const RunPrefix = "@cap/"

// Name prefixes of the entries this package writes. slot: is the ledger's, win: the index run's;
// cap-intent: and cap-reserve: are written in the run of the call that reserves.
const (
	slotPrefix    = "slot:"
	windowPrefix  = "win:"
	intentPrefix  = "cap-intent:"
	reservePrefix = "cap-reserve:"
)

// Store is what the ledger needs of a journal.
type Store interface {
	// Insert stores data under (run, name) if no entry has that name, and returns the entry the name
	// holds (the caller's, or the one already there) and whether this call stored it (A1).
	Insert(ctx context.Context, run, name string, data []byte) (stored []byte, inserted bool, err error)
	// Get returns the entry of run named name, if there is one.
	Get(ctx context.Context, run, name string) (data []byte, ok bool, err error)
	// Scan yields the entries of run whose Seq is greater than after (-1 for all), in ascending Seq:
	// a prefix of the run in commit order (A2). It yields an error at most once, as its last element.
	Scan(ctx context.Context, run string, after int64) iter.Seq2[Item, error]
}

// Item is one entry of a run: its position, name and bytes.
type Item struct {
	Seq  int64
	Name string
	Data []byte
}

// Kind is the kind of window of a rule.
type Kind string

const (
	// Run is one window per run tree: the window id is the root run's ID, and there is no clock.
	Run Kind = "run"
	// Hour is a UTC hour, "2006-01-02T15".
	Hour Kind = "hour"
	// Day is a UTC day, "2006-01-02".
	Day Kind = "day"
)

// Rule names a cap rule's ledger: the deployment's namespace, the rule, its currency and its kind of
// window.
type Rule struct {
	Namespace string
	Name      string
	Currency  string
	Kind      Kind
}

// Errors. A store error is returned as it came (wrapped with what was being done); these are the
// ledger's own refusals.
var (
	// ErrClock is a first intent refused by the clock bound: the driver's window is earlier than the
	// newest recorded one, or later by more than the bound.
	ErrClock = errors.New("capledger: the window is outside the clock bound")
	// ErrOverLimit is a reservation that would take the window over its limit, with no waiver.
	ErrOverLimit = errors.New("capledger: the reservation would exceed the limit")
	// ErrCorrupt is a ledger that breaks its own shape (a gap, a slot out of order, an undecodable
	// slot): a writer that is not this package has been at it. The call fails; nothing is repaired.
	ErrCorrupt = errors.New("capledger: the ledger is corrupt")
	// ErrIntentMismatch is a resume whose request differs from the cap-intent already written for the
	// attempt (another rule, currency, key or amount): the attempt's arguments changed after the decision.
	ErrIntentMismatch = errors.New("capledger: the request differs from the attempt's cap-intent")
	// ErrNotSealed is a release without the proof that the tool never began.
	ErrNotSealed = errors.New("capledger: a release needs the call's began word sealed")
	// ErrUnmarked is a release of the slot of an attempt that is not marked: a retry-safe attempt is
	// never released.
	ErrUnmarked = errors.New("capledger: only a marked attempt is released")
	// ErrContention is a reservation that lost the insert race more often than MaxRetries.
	ErrContention = errors.New("capledger: too many lost slot inserts")
	// ErrInvalid is a request the ledger refuses before it reads or writes anything: a non-positive
	// amount, a negative limit, a currency that is not three upper-case ASCII letters, a name left empty.
	// The caller treats an amount it could not parse, or parsed as negative, as not assessable (the
	// rule's strictest action applies), and never reserves zero.
	ErrInvalid = errors.New("capledger: invalid request")
)

// OverLimitError is the detail of ErrOverLimit.
type OverLimitError struct {
	Cum, Amount, Limit int64
}

func (e *OverLimitError) Error() string {
	return fmt.Sprintf("capledger: the window holds %d, the reservation adds %d, the limit is %d", e.Cum, e.Amount, e.Limit)
}

// Is reports ErrOverLimit.
func (e *OverLimitError) Is(target error) bool { return target == ErrOverLimit }

// Config configures a Ledger.
type Config struct {
	// Store is the journal the ledger writes.
	Store Store
	// Encode maps a free-text name to a key segment that carries none of the characters run IDs are
	// built with (agent's encodeID).
	Encode func(string) string
	// Clock reads now. It is read at a first intent only.
	Clock func() time.Time
	// Bound is how many windows later than the newest recorded one a first intent's window may be
	// (default one).
	Bound int
	// MaxRetries bounds the lost slot inserts of one reservation or release (default 1000): a loser
	// reads again, so contention costs a retry, and past the bound the call fails closed.
	MaxRetries int
	// MaxViews bounds the ledger runs whose reads are cached (default 256): the least recently used is
	// dropped, and read again from the store when next needed. The cache is a hint, so dropping one costs a read.
	MaxViews int
}

// Ledger reserves and releases against cap ledgers in one store. A Ledger is safe for concurrent use;
// its read cache is a hint and never a source of truth (every write is an insert that the store
// decides).
type Ledger struct {
	cfg   Config
	mu    sync.Mutex
	views map[string]*view
	lru   []string // run IDs in use order, least recently used first
}

// New returns a Ledger over cfg.
func New(cfg Config) *Ledger {
	if cfg.Bound <= 0 {
		cfg.Bound = 1
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 1000
	}
	if cfg.MaxViews <= 0 {
		cfg.MaxViews = 256
	}
	return &Ledger{cfg: cfg, views: map[string]*view{}}
}

// KeyDigest is the digest of a normalized key: a SHA-256 over a domain tag and the key, so a customer
// ID is not a run name in a store listing.
func KeyDigest(normalized string) string {
	h := sha256.New()
	h.Write([]byte("bide.cap.key.v1\x00"))
	h.Write([]byte(normalized))
	return hex.EncodeToString(h.Sum(nil))
}

// IndexRun is the ID of the index run of a rule and currency.
func (l *Ledger) IndexRun(r Rule) string {
	return RunPrefix + l.cfg.Encode(r.Namespace) + "/" + l.cfg.Encode(r.Name) + "/" + l.cfg.Encode(r.Currency) + "/index"
}

// LedgerRun is the ID of the ledger run of a rule, a window and a key digest. The index run has four
// segments and a ledger run five, so no ledger run is an index run.
func (l *Ledger) LedgerRun(r Rule, window, keyDigest string) string {
	return RunPrefix + l.cfg.Encode(r.Namespace) + "/" + l.cfg.Encode(r.Name) + "/" + l.cfg.Encode(r.Currency) +
		"/" + l.cfg.Encode(window) + "/" + keyDigest
}

// WindowID is the id of the window of kind k that holds t; for Run it is root, the tree root's run ID.
func WindowID(k Kind, t time.Time, root string) (string, error) {
	switch k {
	case Hour:
		return t.UTC().Format(hourLayout), nil
	case Day:
		return t.UTC().Format(dayLayout), nil
	case Run:
		return root, nil
	}
	return "", fmt.Errorf("capledger: unknown window kind %q", k)
}

const (
	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
)

// parseWindow is the start of window id of kind k, and its length.
func parseWindow(k Kind, id string) (time.Time, time.Duration, error) {
	switch k {
	case Hour:
		t, err := time.Parse(hourLayout, id)
		return t, time.Hour, err
	case Day:
		t, err := time.Parse(dayLayout, id)
		return t, 24 * time.Hour, err
	}
	return time.Time{}, 0, fmt.Errorf("capledger: kind %q has no clock", k)
}

// Slot is one entry of a ledger.
type Slot struct {
	N        int    `json:"n"`
	Kind     string `json:"kind"` // "reserve" or "release"
	Attempt  string `json:"attempt"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Ref      int    `json:"ref,omitempty"` // for a release, the slot it voids
	CumAfter int64  `json:"cum_after"`
	Limit    int64  `json:"limit"`
	Over     bool   `json:"over,omitempty"` // cum_after exceeds the limit, and a waiver covered it
	Marked   bool   `json:"marked,omitempty"`
	// Run, Tool and Att are the identity the Attempt joins, unencoded: the run whose journal holds the
	// call's attempt marker, the tool-use ID and the attempt ID, so a reconcile pass can find the marker.
	Run  string `json:"run,omitempty"`
	Tool string `json:"tool,omitempty"`
	Att  string `json:"att,omitempty"`
}

const (
	kindReserve = "reserve"
	kindRelease = "release"
)

func slotName(n int) string { return slotPrefix + strconv.Itoa(n) }

// view is a cached read of one ledger run: its slots in order, and which of them a release voids.
type view struct {
	mu       sync.Mutex
	seq      int64
	slots    []Slot
	released map[int]bool
}

// read calls f with the ledger run's current slots. It loads what the store holds after the last
// entry it saw, and fails closed on a ledger that breaks its shape: a slot whose number is not the
// next one is a gap, which no writer of this package leaves (Dense).
func (l *Ledger) read(ctx context.Context, run string, f func(v *view) error) error {
	v := l.viewOf(run)
	v.mu.Lock()
	defer v.mu.Unlock()
	seg := strings.Split(run, "/")
	if len(seg) != 6 || seg[0]+"/" != RunPrefix {
		return fmt.Errorf("%w: %q is not a ledger run", ErrCorrupt, run)
	}
	for it, err := range l.cfg.Store.Scan(ctx, run, v.seq) {
		if err != nil {
			return fmt.Errorf("capledger: read the ledger %s: %w", run, err)
		}
		if !strings.HasPrefix(it.Name, slotPrefix) {
			v.seq = it.Seq // the journal header, or an entry that is not a slot
			continue
		}
		s, err := decodeSlot(it.Data)
		if err != nil {
			return fmt.Errorf("%w: %s in %s: %v", ErrCorrupt, it.Name, run, err)
		}
		if err := l.accept(v, run, seg[3], it.Name, s); err != nil {
			return err
		}
		if s.Kind == kindRelease {
			v.released[s.Ref] = true
		}
		v.slots = append(v.slots, s)
		v.seq = it.Seq
	}
	return f(v)
}

// viewOf is the cached view of run, created if there is none; the cache holds at most MaxViews, the
// least recently used dropped.
func (l *Ledger) viewOf(run string) *view {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.views[run]
	if ok {
		for i, r := range l.lru {
			if r == run {
				l.lru = append(append(l.lru[:i:i], l.lru[i+1:]...), run)
				break
			}
		}
		return v
	}
	v = &view{seq: -1, released: map[int]bool{}}
	l.views[run] = v
	l.lru = append(l.lru, run)
	for len(l.lru) > l.cfg.MaxViews {
		delete(l.views, l.lru[0])
		l.lru = l.lru[1:]
	}
	return v
}

// accept checks a slot the store holds against everything a writer of this package guarantees, so a
// slot planted by anything else cannot move the window's total: a known kind, the next number, a
// positive amount, the ledger's currency (its run ID's fourth segment), a total that follows from the
// one before, the over flag the limit implies, and for a release an earlier live reserve of the same
// attempt, amount and marking. Any violation is ErrCorrupt, which refuses the call.
func (l *Ledger) accept(v *view, run, currencySeg, name string, s Slot) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s in %s: %s", ErrCorrupt, name, run, why) }
	if name != slotName(s.N) || s.N != len(v.slots) {
		return bad(fmt.Sprintf("slot %d after %d slots", s.N, len(v.slots)))
	}
	if s.Kind != kindReserve && s.Kind != kindRelease {
		return bad(fmt.Sprintf("unknown kind %q", s.Kind))
	}
	if s.Attempt == "" || s.Amount <= 0 || s.Limit < 0 {
		return bad("an empty attempt, a non-positive amount or a negative limit")
	}
	if l.cfg.Encode(s.Currency) != currencySeg || !validCurrency(s.Currency) {
		return bad(fmt.Sprintf("currency %q is not the ledger's", s.Currency))
	}
	prev, _ := v.last()
	var want int64
	if s.Kind == kindReserve {
		sum, err := amount.Add(prev, s.Amount)
		if err != nil {
			return bad("the total overflows")
		}
		want = sum
		if s.Over != (s.CumAfter > s.Limit) || s.Ref != 0 {
			return bad("the over flag or a reference that the slot's total does not allow")
		}
	} else {
		r := s.Ref
		if r < 0 || r >= len(v.slots) || v.slots[r].Kind != kindReserve || v.released[r] ||
			v.slots[r].Attempt != s.Attempt || v.slots[r].Amount != s.Amount || v.slots[r].Marked != s.Marked || !s.Marked || s.Over {
			return bad("a release that does not void an earlier live reserve of the same attempt and amount")
		}
		want = prev - s.Amount
	}
	if s.CumAfter != want {
		return bad(fmt.Sprintf("cum_after %d, want %d", s.CumAfter, want))
	}
	return nil
}

// validCurrency is three upper-case ASCII letters, byte for byte: nothing is normalised.
func validCurrency(c string) bool {
	if len(c) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if c[i] < 'A' || c[i] > 'Z' {
			return false
		}
	}
	return true
}

// last is the total after the last slot, and the number of slots.
func (v *view) last() (cum int64, n int) {
	if len(v.slots) == 0 {
		return 0, 0
	}
	return v.slots[len(v.slots)-1].CumAfter, len(v.slots)
}

// live is the slot of attempt that no release voids, if there is one.
func (v *view) live(attempt string) (Slot, bool) {
	for _, s := range v.slots {
		if s.Kind == kindReserve && s.Attempt == attempt && !v.released[s.N] {
			return s, true
		}
	}
	return Slot{}, false
}

// Slots returns every slot of a ledger run, in order: for an audit and for tests.
func (l *Ledger) Slots(ctx context.Context, run string) ([]Slot, error) {
	var out []Slot
	err := l.read(ctx, run, func(v *view) error {
		out = append([]Slot(nil), v.slots...)
		return nil
	})
	return out, err
}

// Live returns the live reserve slots of a ledger run (those no release voids), in slot order: for an
// audit of what a window holds.
func (l *Ledger) Live(ctx context.Context, run string) ([]Slot, error) {
	var out []Slot
	err := l.read(ctx, run, func(v *view) error {
		for _, s := range v.slots {
			if s.Kind == kindReserve && !v.released[s.N] {
				out = append(out, s)
			}
		}
		return nil
	})
	return out, err
}
