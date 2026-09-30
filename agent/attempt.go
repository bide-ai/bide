// attempt.go holds the attempt-marker protocol shared by tool calls and Step: an exclusive claim
// before a non-retriable side effect, a record that the effect provably never started, and the
// numbered re-attempts that record allows.
//
// A marker says "this effect may have fired". Resume halts on a marker with no result, because
// the outcome is unknown. When the driver that won the claim can prove the effect was never
// called (it was cancelled, or its store failed, after the claim and before the call), it records
// that under the marker's not-started key, bound to its claim. That attempt then no longer counts:
// a resume re-attempts the effect under a new, numbered marker, claimed exclusively like the
// first. A process that dies in the same window records nothing, so its marker still halts.

package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// errNoRecord is what a probe's fn returns from Do when the key is not recorded, so that Do
// records nothing.
var errNoRecord = errors.New("agent: no record")

// doShared is Do for a write that may race a probe of the same key in this process. A store's Do
// hands concurrent callers of one key a single call's outcome, so a write that joins a probe in
// flight gets the probe's errNoRecord instead of writing; it then tries again.
func doShared(ctx context.Context, d Durable, runID, key string, fn func(context.Context) (Record, error)) (Record, error) {
	for {
		rec, err := d.Do(ctx, runID, key, fn)
		if !errors.Is(err, errNoRecord) {
			return rec, err
		}
		if err := ctx.Err(); err != nil {
			return Record{}, err
		}
	}
}

// probe returns the record stored under key, if any, without writing one.
func probe(ctx context.Context, d Durable, runID, key string) (Record, bool, error) {
	rec, err := d.Do(ctx, runID, key, func(context.Context) (Record, error) { return Record{}, errNoRecord })
	if errors.Is(err, errNoRecord) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

// voided reports whether the attempt with marker key key, whose marker is marker, is recorded as
// never started by the driver that claimed it.
func voided(ctx context.Context, d Durable, runID, key string, marker Record) (bool, error) {
	rec, ok, err := probe(ctx, d, runID, notStartedStep(key, marker.claim))
	if err != nil || !ok {
		return false, err
	}
	return rec.Kind == StepNotStarted && marker.claim != "" && rec.claim == marker.claim, nil
}

// claimNextAttempt claims the next attempt of the effect whose first marker key is base: the
// first attempt whose marker is not yet recorded, provided every earlier one is recorded as not
// started. It returns whether this caller won, the marker that decides it (its own, or the one it
// lost to), and that marker's key. A caller that loses must not run the effect: another driver
// owns that attempt, or an earlier attempt may have fired it.
func claimNextAttempt(ctx context.Context, d Durable, runID, base string, rec Record) (bool, Record, string, error) {
	if j := journalOf(d); j != nil {
		return j.claimNext(ctx, runID, base, rec)
	}
	for gen := 0; ; gen++ {
		key := retryAttemptStep(base, gen)
		won, got, err := claimAttempt(ctx, d, runID, key, rec)
		if err != nil || won {
			return won, got, key, err
		}
		if ok, err := voided(ctx, d, runID, key, got); err != nil || !ok {
			return false, got, key, err
		}
	}
}

// liveAttempt returns the marker of the effect whose first marker key is base that is not
// recorded as not started, if there is one: the attempt that may have fired the effect.
func liveAttempt(ctx context.Context, d Durable, runID, base string) (Record, bool, error) {
	if j := journalOf(d); j != nil {
		return j.liveAttempt(ctx, runID, base)
	}
	for gen := 0; ; gen++ {
		key := retryAttemptStep(base, gen)
		marker, ok, err := probe(ctx, d, runID, key)
		if err != nil || !ok {
			return Record{}, false, err
		}
		if v, err := voided(ctx, d, runID, key, marker); err != nil {
			return Record{}, false, err
		} else if !v {
			return marker, true, nil
		}
	}
}

// recordNotStarted records that the attempt with marker key key, which this driver claimed with
// marker, never called its effect. It is written whatever ctx's state, since a cancellation is
// the usual reason the effect did not start. If it cannot be written, the marker stands and a
// resume halts, which is safe.
func recordNotStarted(ctx context.Context, d Durable, runID, key string, marker Record) error {
	if j := journalOf(d); j != nil {
		return j.notStarted(ctx, runID, key, marker)
	}
	_, err := doShared(context.WithoutCancel(ctx), d, runID, notStartedStep(key, marker.claim), func(context.Context) (Record, error) {
		return Record{Kind: StepNotStarted, ToolUseID: marker.ToolUseID, claim: marker.claim}, nil
	})
	if err != nil {
		// Remembered as the Journal remembers it (see Journal.notStarted), so the next claim of the
		// key in this process, or a resume that meets the attempt, writes it again.
		if id, keyed := durableIdentity(d); keyed {
			pendingClaims.remember(flightKey{id, runID, key}, marker.claim)
		}
		return fmt.Errorf("record that %s did not start: %w", key, err)
	}
	return nil
}

// retryNotStarted writes again the not-started record of the attempt with marker key key, whose
// marker is marker, when this process remembers that marker's own claim id, and reports whether
// the attempt is now recorded as not started (see Journal.retryNotStarted). Through a Durable the
// engine drives through its Do, it writes through d.
func retryNotStarted(ctx context.Context, d Durable, runID, key string, marker Record) bool {
	if j := journalOf(d); j != nil {
		return j.retryNotStarted(ctx, runID, key, marker)
	}
	id, keyed := durableIdentity(d)
	if !keyed || marker.claim == "" || !pendingClaims.takeID(flightKey{id, runID, key}, marker.claim) {
		return false
	}
	return recordNotStarted(ctx, d, runID, key, marker) == nil
}

// durableIdentity is the identity under which this process keys what it keeps for d's runs
// (remembered claims, kept spend): the identity of the store beneath the Journal d writes through
// (storeIdentity, which in-flight steps share too), following the Unwrap() Durable of a wrapper
// such as audit.AuditedStore down to it, or else the innermost Durable itself when it is a
// pointer. ok is false when there is neither, and nothing is kept.
func durableIdentity(d Durable) (any, bool) {
	for range maxUnwrap {
		if d == nil || isNil(d) {
			return nil, false
		}
		if j := journalOf(d); j != nil {
			return j.id, true
		}
		u, ok := d.(interface{ Unwrap() Durable })
		if !ok {
			break
		}
		d = u.Unwrap()
	}
	if d != nil && !isNil(d) && reflect.ValueOf(d).Kind() == reflect.Pointer {
		return d, true
	}
	return nil, false
}

// liveAttempts returns, by the first attempt's marker key (toolAttemptStep or stepAttemptStep),
// the attempt markers in recs that no not-started record voids: the attempts that may have fired
// their effect. At most one per effect is live, since a re-attempt is claimed only once the
// attempt before it is recorded as not started.
func liveAttempts(recs []Record) map[string]Record {
	void := map[string]bool{} // keys of not-started records written by the claim they name
	for _, r := range recs {
		if r.Kind == StepNotStarted && r.claim != "" && strings.HasPrefix(r.Name, notStartedPrefix+r.claim+":") {
			void[r.Name] = true
		}
	}
	live := map[string]Record{}
	for _, r := range recs {
		if r.Kind != StepAttempt {
			continue
		}
		if r.claim != "" && void[notStartedStep(r.Name, r.claim)] {
			continue
		}
		live[attemptBase(r.Name)] = r
	}
	return live
}
