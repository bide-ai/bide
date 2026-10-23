package capledger

import (
	"context"
	"fmt"
	"sync/atomic"
)

// Seal is the proof that one attempt's tool never began and never will: the began word of its call,
// shared by every invocation of its handler, moved from none to sealed. Only NewSeal makes one, from the
// word itself, and Release takes one for the attempt it names, so a release is written only by a caller
// that sealed that attempt's word (model 17's LoopSeal). A handler's own "I did not begin" is not a
// Seal, because a second invocation of the handler can find the word set by the first (errReinvoked),
// and a retry-safe attempt has no word at all; and a Seal of one attempt, copied, releases no other.
type Seal struct {
	attempt string
	sealed  bool
}

// protocol:caps begin LoopSeal

// NewSeal moves word, the began word of attempt's call, from none to sealed with a compare and swap
// that the ledger performs itself, and returns the Seal if the swap won. A word that says the tool began
// (or is already sealed by another) makes no Seal: a Seal is never made for a call that began.
func NewSeal(attempt string, word *atomic.Int32, none, sealed int32) (Seal, bool) {
	if attempt == "" || word == nil || none == sealed || !word.CompareAndSwap(none, sealed) {
		return Seal{}, false
	}
	return Seal{attempt: attempt, sealed: true}, true
}

// Target names an attempt whose slot is released: the rule, the key, the run that made the call and the
// attempt's three parts, as in the Request that reserved it. The ledger is not named: the window is read
// from the attempt's cap-intent, as Reserve reads it, so a release cannot land in another ledger.
type Target struct {
	Rule                 Rule
	KeyDigest            string
	RunID, RootRunID     string
	ToolUseID, AttemptID string
}

// Release releases the live slot of the attempt in the ledger its cap-intent names, once the call's
// began word is sealed (s must be the seal of this attempt), and reports whether it wrote a release
// (false when there is no intent, so nothing was ever reserved, or the attempt holds no live slot, or is
// already released). The caller voids the attempt afterwards (recordNotStarted), never before: a void
// without its release would leave the slot counted until Reconcile.
//
// Only a marked attempt is released. A retry-safe attempt is shared by any number of drivers of one
// tool-use ID, so one driver's proof says nothing about the others; its slot stays counted, which is
// the safe direction (model 17, findings/release-unmarked).
func (l *Ledger) Release(ctx context.Context, t Target, s Seal) (bool, error) {
	attempt := l.AttemptKey(t.RootRunID, t.ToolUseID, t.AttemptID)
	if !s.sealed || s.attempt != attempt {
		return false, ErrNotSealed
	}
	in, ok, err := l.intentOf(ctx, t, attempt)
	if err != nil || !ok {
		return false, err
	}
	if !in.Marked {
		return false, fmt.Errorf("%w: %s", ErrUnmarked, attempt)
	}
	return l.release(ctx, l.LedgerRun(t.Rule, in.Window, t.KeyDigest), attempt)
}

// intentOf reads the cap-intent of an attempt in the run that made the call, and checks it names the
// rule, currency and key the caller says.
func (l *Ledger) intentOf(ctx context.Context, t Target, attempt string) (intent, bool, error) {
	data, ok, err := l.cfg.Store.Get(ctx, t.RunID, intentPrefix+l.cfg.Encode(attempt))
	if err != nil {
		return intent{}, false, fmt.Errorf("capledger: read the cap-intent of %s: %w", attempt, err)
	}
	if !ok {
		return intent{}, false, nil
	}
	in, err := decodeIntent(data)
	if err != nil {
		return intent{}, false, fmt.Errorf("%w: the cap-intent of %s does not decode: %v", ErrCorrupt, attempt, err)
	}
	if in.Attempt != attempt || in.Namespace != t.Rule.Namespace || in.Rule != t.Rule.Name || in.Currency != t.Rule.Currency ||
		in.Kind != t.Rule.Kind || in.Key != t.KeyDigest {
		return intent{}, false, fmt.Errorf("%w: %s", ErrIntentMismatch, attempt)
	}
	return in, true, nil
}

// protocol:caps end

// protocol:caps begin Read Insert

// release is the release loop: read the ledger, find the live slot of the attempt (the dedupe scan: a
// slot already released is done), and insert a release of it at the next name; a loser reads again.
func (l *Ledger) release(ctx context.Context, run, attempt string) (bool, error) {
	for try := 0; try < l.cfg.MaxRetries; try++ {
		var (
			next     Slot
			insertAt int
			none     bool
		)
		err := l.read(ctx, run, func(v *view) error {
			s, ok := v.live(attempt)
			if !ok {
				none = true
				return nil
			}
			if !s.Marked {
				return fmt.Errorf("%w: %s", ErrUnmarked, attempt)
			}
			cum, n := v.last()
			if cum < s.Amount {
				return fmt.Errorf("%w: releasing %d from a window holding %d", ErrCorrupt, s.Amount, cum)
			}
			insertAt = n
			next = Slot{N: n, Kind: kindRelease, Attempt: attempt, Amount: s.Amount, Currency: s.Currency, Ref: s.N,
				CumAfter: cum - s.Amount, Limit: s.Limit, Marked: true, Run: s.Run, Tool: s.Tool, Att: s.Att}
			return nil
		})
		if err != nil {
			return false, err
		}
		if none {
			return false, nil
		}
		b, err := encodeSlot(next)
		if err != nil {
			return false, err
		}
		if _, inserted, err := l.cfg.Store.Insert(ctx, run, slotName(insertAt), b); err != nil {
			return false, fmt.Errorf("capledger: insert %s of %s: %w", slotName(insertAt), run, err)
		} else if inserted {
			return true, nil
		}
	}
	return false, fmt.Errorf("%w: release in %s", ErrContention, run)
}

// protocol:caps end

// protocol:caps begin Reconcile

// Reconcile releases the live slots of marked attempts that voided reports as recorded not started (it is given the slot: its Run, Tool and Att find the attempt marker): the
// slots a void left counted (a void written without a release, a handler that reserved after the
// call was sealed). voided reads the run journals, which the ledger does not know. An attempt that is
// not voided is left alone, as is every unmarked attempt and a call resolved by ResolveHalt: a window
// only over-counts. It returns the attempts it released.
func (l *Ledger) Reconcile(ctx context.Context, run string, voided func(ctx context.Context, s Slot) (bool, error)) ([]string, error) {
	live, err := l.Live(ctx, run)
	if err != nil {
		return nil, err
	}
	var released []string
	for _, s := range live {
		if !s.Marked {
			continue
		}
		v, err := voided(ctx, s)
		if err != nil {
			return released, fmt.Errorf("capledger: read whether %s was voided: %w", s.Attempt, err)
		}
		if !v {
			continue
		}
		// The ledger a slot is released in is the one its attempt's cap-intent names: a slot that sits in
		// any other ledger (written there by something that is not this package) is refused, not released.
		data, found, err := l.cfg.Store.Get(ctx, s.Run, intentPrefix+l.cfg.Encode(s.Attempt))
		if err != nil {
			return released, fmt.Errorf("capledger: read the cap-intent of %s: %w", s.Attempt, err)
		}
		if !found {
			return released, fmt.Errorf("%w: the live slot of %s has no cap-intent in run %q", ErrCorrupt, s.Attempt, s.Run)
		}
		in, err := decodeIntent(data)
		if err != nil {
			return released, fmt.Errorf("%w: the cap-intent of %s does not decode: %v", ErrCorrupt, s.Attempt, err)
		}
		if in.Attempt != s.Attempt || l.LedgerRun(Rule{Namespace: in.Namespace, Name: in.Rule, Currency: in.Currency, Kind: in.Kind}, in.Window, in.Key) != run {
			return released, fmt.Errorf("%w: the slot of %s is in %s, not the ledger its cap-intent names", ErrIntentMismatch, s.Attempt, run)
		}
		ok, err := l.release(ctx, run, s.Attempt)
		if err != nil {
			return released, err
		}
		if ok {
			released = append(released, s.Attempt)
		}
	}
	return released, nil
}

// protocol:caps end
