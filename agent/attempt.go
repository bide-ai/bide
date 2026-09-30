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
	rec, ok, err := probe(ctx, d, runID, notStartedStep(key))
	if err != nil || !ok {
		return false, err
	}
	return rec.Kind == StepNotStarted && marker.Claim != "" && rec.Claim == marker.Claim, nil
}

// claimNextAttempt claims the next attempt of the effect whose first marker key is base: the
// first attempt whose marker is not yet recorded, provided every earlier one is recorded as not
// started. It returns whether this caller won, the marker that decides it (its own, or the one it
// lost to), and that marker's key. A caller that loses must not run the effect: another driver
// owns that attempt, or an earlier attempt may have fired it.
func claimNextAttempt(ctx context.Context, d Durable, runID, base string, rec Record) (bool, Record, string, error) {
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
	_, err := doShared(context.WithoutCancel(ctx), d, runID, notStartedStep(key), func(context.Context) (Record, error) {
		return Record{Kind: StepNotStarted, ToolUseID: marker.ToolUseID, Claim: marker.Claim}, nil
	})
	if err != nil {
		return fmt.Errorf("record that %s did not start: %w", key, err)
	}
	return nil
}

// liveAttempts returns, by the first attempt's marker key (toolAttemptStep or stepAttemptStep),
// the attempt markers in recs that no not-started record voids: the attempts that may have fired
// their effect. At most one per effect is live, since a re-attempt is claimed only once the
// attempt before it is recorded as not started.
func liveAttempts(recs []Record) map[string]Record {
	void := map[string]string{} // marker key -> the claim its not-started record carries
	for _, r := range recs {
		if r.Kind == StepNotStarted {
			void[strings.TrimPrefix(r.Name, notStartedPrefix)] = r.Claim
		}
	}
	live := map[string]Record{}
	for _, r := range recs {
		if r.Kind != StepAttempt {
			continue
		}
		if c, ok := void[r.Name]; ok && r.Claim != "" && c == r.Claim {
			continue
		}
		live[attemptBase(r.Name)] = r
	}
	return live
}
