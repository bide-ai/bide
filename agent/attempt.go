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
	"strings"
)

// protocol:claims begin Claim Lost NotStarted GateTake GateWrite LoserWait Open RCheck

// claimNextAttempt claims the next attempt of the effect whose first marker key is base: the
// first attempt whose marker is not yet recorded, provided every earlier one is recorded as not
// started. It returns whether this caller won, the marker that decides it (its own, or the one it
// lost to), and that marker's key. A caller that loses must not run the effect: another driver
// owns that attempt, or an earlier attempt may have fired it.
func claimNextAttempt(ctx context.Context, j *Journal, runID, base string, rec Record) (bool, Record, string, error) {
	return j.claimNext(ctx, runID, base, rec)
}

// liveAttempt returns the marker of the effect whose first marker key is base that is not
// recorded as not started, if there is one: the attempt that may have fired the effect.
func liveAttempt(ctx context.Context, j *Journal, runID, base string) (Record, bool, error) {
	return j.liveAttempt(ctx, runID, base)
}

// recordNotStarted records that the attempt with marker key key, which this driver claimed with
// marker, never called its effect. It is written whatever ctx's state, since a cancellation is
// the usual reason the effect did not start. If it cannot be written, the marker stands and a
// resume halts, which is safe.
func recordNotStarted(ctx context.Context, j *Journal, runID, key string, marker Record) error {
	return j.notStarted(ctx, runID, key, marker)
}

// retryNotStarted writes again the not-started record of the attempt with marker key key, whose
// marker is marker, when this process remembers that marker's own claim id, and reports whether
// the attempt is now recorded as not started (see Journal.retryNotStarted).
func retryNotStarted(ctx context.Context, j *Journal, runID, key string, marker Record) bool {
	return j.retryNotStarted(ctx, runID, key, marker)
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

// protocol:claims end
