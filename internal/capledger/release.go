package capledger

import (
	"context"
	"fmt"
)

// Seal is the proof that a call's tool never began and never will: the began word of the call, shared
// by every invocation of its handler, moved from none to sealed. Only NewSeal makes one, and Release
// takes one, so a release is written only by a caller that sealed the word (model 17's LoopSeal): a
// handler's own "I did not begin" is not a Seal, because a second invocation of the handler can find
// the word set by the first (errReinvoked), and a retry-safe attempt has no word at all.
type Seal struct{ sealed bool }

// protocol:caps begin LoopSeal

// NewSeal runs seal, which tries to move the call's began word from none to sealed (a compare and swap
// that fails when the tool began), and returns the Seal if it did. A Seal is never made for a call
// that began.
func NewSeal(seal func() bool) (Seal, bool) {
	if seal == nil || !seal() {
		return Seal{}, false
	}
	return Seal{sealed: true}, true
}

// protocol:caps end

// Release releases the live slot of attempt in the ledger run, once the call's began word is sealed,
// and reports whether it wrote a release (false when the attempt holds no live slot: it was never
// reserved, or is already released). The caller voids the attempt afterwards (recordNotStarted), never
// before: a void without its release would leave the slot counted until Reconcile.
//
// Only a marked attempt is released. A retry-safe attempt is shared by any number of drivers of one
// tool-use ID, so one driver's proof says nothing about the others; its slot stays counted, which is
// the safe direction (model 17, findings/release-unmarked).
func (l *Ledger) Release(ctx context.Context, run, attempt string, s Seal) (bool, error) {
	if !s.sealed {
		return false, ErrNotSealed
	}
	return l.release(ctx, run, attempt)
}

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
			insertAt = n
			next = Slot{N: n, Kind: kindRelease, Attempt: attempt, Amount: s.Amount, Currency: s.Currency, Ref: s.N,
				CumAfter: cum - s.Amount, Limit: s.Limit, Marked: true}
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

// Reconcile releases the live slots of marked attempts that voided reports as recorded not started: the
// slots a void left counted (a void written without a release, a handler that reserved after the
// call was sealed). voided reads the run journals, which the ledger does not know. An attempt that is
// not voided is left alone, as is every unmarked attempt and a call resolved by ResolveHalt: a window
// only over-counts. It returns the attempts it released.
func (l *Ledger) Reconcile(ctx context.Context, run string, voided func(ctx context.Context, attempt string) (bool, error)) ([]string, error) {
	live, err := l.Live(ctx, run)
	if err != nil {
		return nil, err
	}
	var released []string
	for _, s := range live {
		if !s.Marked {
			continue
		}
		v, err := voided(ctx, s.Attempt)
		if err != nil {
			return released, fmt.Errorf("capledger: read whether %s was voided: %w", s.Attempt, err)
		}
		if !v {
			continue
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
