package capledger

import (
	"context"
	"fmt"
	"time"

	"github.com/bide-ai/bide/internal/amount"
)

// Request is one reservation: an attempt of a call asks for amount against rule's window.
type Request struct {
	Rule Rule
	// KeyDigest is KeyDigest of the normalized key the rule is by ("" digests as the unkeyed total).
	KeyDigest string
	// RunID is the run whose journal holds the attempt's cap-intent and cap-reserve: the run that made the call.
	RunID string
	// RootRunID is the root of the run's tree: the window id of a Run window.
	RootRunID string
	// Attempt identifies the attempt: "<root run id>/<tool-use id>/<attempt id>". A retry of the same
	// attempt, a resume, a second driver and a middleware's second next carry the same Attempt.
	Attempt string
	// Marked says the attempt has an exclusive claim (a tool that is not retry-safe). Only a marked
	// attempt is ever released.
	Marked bool
	Amount int64
	Limit  int64
	// Waiver says the call carries an approved waiver for the rule: a reservation over the limit is
	// written, marked over.
	Waiver bool
}

// Outcome is where a reservation landed.
type Outcome struct {
	Ledger string // the ledger run
	Window string
	Slot   Slot
	// Resumed says the attempt already held a live slot (a retry, a resume or a second driver).
	Resumed bool
}

// intent is the cap-intent record: what an attempt reserves, and the window fixed for it once.
type intent struct {
	Format    string `json:"format"`
	Attempt   string `json:"attempt"`
	Namespace string `json:"namespace"`
	Rule      string `json:"rule"`
	Currency  string `json:"currency"`
	Kind      Kind   `json:"window_kind"`
	Key       string `json:"key"`
	Amount    int64  `json:"amount"`
	Marked    bool   `json:"marked,omitempty"`
	Window    string `json:"window"`
	// Floor is the newest window the index held when the clock bound was checked (the audit's
	// evidence of what the first intent was checked against); empty when there was none.
	Floor string `json:"floor,omitempty"`
}

const intentFormat = "bide.cap-intent.v1"

func (r Request) intentName(l *Ledger) string { return intentPrefix + l.cfg.Encode(r.Attempt) }

// matches reports whether in is the intent of r: the rule, currency, key and amount an attempt reserved
// under are fixed by its first intent.
func (in intent) matches(r Request) bool {
	return in.Attempt == r.Attempt && in.Namespace == r.Rule.Namespace && in.Rule == r.Rule.Name &&
		in.Currency == r.Rule.Currency && in.Kind == r.Rule.Kind && in.Key == r.KeyDigest &&
		in.Amount == r.Amount && in.Marked == r.Marked
}

// Reserve reserves r.Amount for r.Attempt and returns the slot that holds it. It is idempotent: an
// attempt that already holds a live slot gets that slot, whoever asks and however often. When the
// reservation would take the window over its limit and the call has no waiver it returns an error
// that is ErrOverLimit and writes nothing.
//
// A crash at any point leaves the window counting the attempt (a slot is inserted) or not (nothing
// was), and a later Reserve of the same attempt finds the same ledger through the intent.
func (l *Ledger) Reserve(ctx context.Context, r Request) (Outcome, error) {
	if r.Attempt == "" || r.RunID == "" || r.Rule.Currency == "" {
		return Outcome{}, fmt.Errorf("capledger: a reservation needs an attempt, a run and a currency")
	}
	in, err := l.window(ctx, r)
	if err != nil {
		return Outcome{}, err
	}
	run := l.LedgerRun(r.Rule, in.Window, r.KeyDigest)
	slot, resumed, err := l.insertSlot(ctx, run, r)
	if err != nil {
		return Outcome{}, err
	}
	// cap-reserve is the audit's record of the slot, written after it; recovery does not read it.
	rec, err := encodeReserve(reserveRecord{Format: reserveFormat, Ledger: run, Slot: slot.N, Cum: slot.CumAfter})
	if err != nil {
		return Outcome{}, err
	}
	if _, _, err := l.cfg.Store.Insert(ctx, r.RunID, reservePrefix+l.cfg.Encode(r.Attempt), rec); err != nil {
		return Outcome{}, fmt.Errorf("capledger: record the reservation of %s: %w", r.Attempt, err)
	}
	return Outcome{Ledger: run, Window: in.Window, Slot: slot, Resumed: resumed}, nil
}

// protocol:caps begin Start Check IdxInsert Intent

// window returns the attempt's intent, writing it first if there is none. A resume reads the window
// from the intent it finds and never derives it again, and the clock bound is not applied to it. A
// first intent derives the window once from the clock, checks it against the index run, inserts
// win:<window> into the index (insert-once, so racing drivers agree), then writes the intent; a crash
// between the index insert and the intent leaves an index entry for a window with no intent, which
// costs nothing. A driver that loses the intent insert adopts the winner's window.
func (l *Ledger) window(ctx context.Context, r Request) (intent, error) {
	name := r.intentName(l)
	if data, ok, err := l.cfg.Store.Get(ctx, r.RunID, name); err != nil {
		return intent{}, fmt.Errorf("capledger: read the cap-intent of %s: %w", r.Attempt, err)
	} else if ok {
		return l.adopt(r, data)
	}
	w, err := WindowID(r.Rule.Kind, l.cfg.Clock(), r.RootRunID)
	if err != nil {
		return intent{}, err
	}
	floor := ""
	if r.Rule.Kind != Run {
		if floor, err = l.checkClock(ctx, r.Rule, w); err != nil {
			return intent{}, err
		}
		b, err := encodeWindow(w)
		if err != nil {
			return intent{}, err
		}
		if _, _, err := l.cfg.Store.Insert(ctx, l.IndexRun(r.Rule), windowPrefix+w, b); err != nil {
			return intent{}, fmt.Errorf("capledger: record the window %s: %w", w, err)
		}
	}
	in := intent{Format: intentFormat, Attempt: r.Attempt, Namespace: r.Rule.Namespace, Rule: r.Rule.Name,
		Currency: r.Rule.Currency, Kind: r.Rule.Kind, Key: r.KeyDigest, Amount: r.Amount, Marked: r.Marked,
		Window: w, Floor: floor}
	b, err := encodeIntent(in)
	if err != nil {
		return intent{}, err
	}
	stored, _, err := l.cfg.Store.Insert(ctx, r.RunID, name, b)
	if err != nil {
		return intent{}, fmt.Errorf("capledger: write the cap-intent of %s: %w", r.Attempt, err)
	}
	return l.adopt(r, stored)
}

// adopt decodes an intent the store holds and checks it is r's.
func (l *Ledger) adopt(r Request, data []byte) (intent, error) {
	in, err := decodeIntent(data)
	if err != nil {
		return intent{}, fmt.Errorf("%w: the cap-intent of %s does not decode: %v", ErrCorrupt, r.Attempt, err)
	}
	if !in.matches(r) {
		return intent{}, fmt.Errorf("%w: %s", ErrIntentMismatch, r.Attempt)
	}
	return in, nil
}

// checkClock applies the clock bound at a first intent: w must not be earlier than the newest window
// the index holds, nor more than Bound windows later. It returns the newest window it saw ("" if the
// index is empty). The index run is read whole: it holds one small entry per window of the rule.
func (l *Ledger) checkClock(ctx context.Context, rule Rule, w string) (string, error) {
	start, dur, err := parseWindow(rule.Kind, w)
	if err != nil {
		return "", err
	}
	newest := ""
	var newestT time.Time
	for it, err := range l.cfg.Store.Scan(ctx, l.IndexRun(rule), -1) {
		if err != nil {
			return "", fmt.Errorf("capledger: read the index of %s: %w", rule.Name, err)
		}
		if len(it.Name) <= len(windowPrefix) || it.Name[:len(windowPrefix)] != windowPrefix {
			continue
		}
		id := it.Name[len(windowPrefix):]
		t, _, err := parseWindow(rule.Kind, id)
		if err != nil {
			return "", fmt.Errorf("%w: the index of %s holds %q", ErrCorrupt, rule.Name, it.Name)
		}
		if newest == "" || t.After(newestT) {
			newest, newestT = id, t
		}
	}
	if newest == "" {
		return "", nil
	}
	if start.Before(newestT) {
		return newest, fmt.Errorf("%w: the driver's window %s is earlier than the newest recorded window %s", ErrClock, w, newest)
	}
	if start.After(newestT.Add(time.Duration(l.cfg.Bound) * dur)) {
		return newest, fmt.Errorf("%w: the driver's window %s is more than %d window(s) later than the newest recorded window %s", ErrClock, w, l.cfg.Bound, newest)
	}
	return newest, nil
}

// protocol:caps end

// protocol:caps begin Read Insert

// insertSlot is the reserve loop: read the ledger, return the attempt's live slot if it has one,
// else check the limit against the last slot's cum_after and insert slot n+1. The store has one
// winner per name, so two reservations that read the same last slot race for the same name and
// exactly one wins, and its cum_after is exact: slot n was the last entry when its insert succeeded.
// A loser reads again.
func (l *Ledger) insertSlot(ctx context.Context, run string, r Request) (Slot, bool, error) {
	for try := 0; try < l.cfg.MaxRetries; try++ {
		var (
			found    Slot
			resumed  bool
			next     Slot
			insertAt int
		)
		err := l.read(ctx, run, func(v *view) error {
			if s, ok := v.live(r.Attempt); ok {
				found, resumed = s, true
				return nil
			}
			cum, n := v.last()
			after, err := amount.Add(cum, r.Amount)
			if err != nil {
				return fmt.Errorf("capledger: the window total overflows: %w", err)
			}
			if after > r.Limit && !r.Waiver {
				return &OverLimitError{Cum: cum, Amount: r.Amount, Limit: r.Limit}
			}
			insertAt = n
			next = Slot{N: n, Kind: kindReserve, Attempt: r.Attempt, Amount: r.Amount, Currency: r.Rule.Currency,
				CumAfter: after, Limit: r.Limit, Over: after > r.Limit, Marked: r.Marked}
			return nil
		})
		if err != nil {
			return Slot{}, false, err
		}
		if resumed {
			return found, true, nil
		}
		b, err := encodeSlot(next)
		if err != nil {
			return Slot{}, false, err
		}
		if _, inserted, err := l.cfg.Store.Insert(ctx, run, slotName(insertAt), b); err != nil {
			return Slot{}, false, fmt.Errorf("capledger: insert %s of %s: %w", slotName(insertAt), run, err)
		} else if inserted {
			return next, false, nil
		}
		// Another writer took the name: read the ledger again.
	}
	return Slot{}, false, fmt.Errorf("%w: reserve in %s", ErrContention, run)
}

// protocol:caps end
