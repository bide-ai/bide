package audit

import (
	"fmt"
	"time"

	"github.com/bide-ai/bide/agent"
)

// The timestamp rule. A signed tree head's TimestampNanos is when the head was signed, in Unix
// nanoseconds (time.Now().UnixNano(), as AuditedStore stamps it). A verifier holds every signed
// head it is shown to three checks:
//
//   - it is positive: a head with a zero or negative timestamp says nothing about when it was made;
//   - it is not later than the verifier's clock plus an allowed skew (DefaultClockSkew unless the
//     caller sets one), so a head cannot claim to have been signed in the future;
//   - where one head extends another, it is not earlier than the head it extends: a consistency
//     proof's earlier head is not later than the head it proves grown, and a run certificate's
//     used-policy head, projected from its journal head, is not earlier than that head.
//
// The rule orders heads and bounds them by the verifier's clock. It does not say how old a head
// may be (an audit reads old heads), and it does not apply across runs or anchor-log entries,
// whose heads are independent. A grant's NotAfterUnix is a separate, seconds-based expiry, checked by
// Grant.Expired and VerifyCurrentGrant against the caller's clock; an evidence package does not
// check it, because it carries no time for each action to compare against.

// DefaultClockSkew is how far past the verifier's clock a signed head's TimestampNanos may be before it
// is refused: five minutes, for clocks that are not exactly in step.
const DefaultClockSkew = 5 * time.Minute

// CheckTimestamp applies the timestamp rule to one signed head: its TimestampNanos must be positive
// and not later than now plus skew. A head that breaks the rule is an error wrapping
// ErrNotVerified; a negative skew is an error wrapping agent.ErrConfig.
func CheckTimestamp(th TreeHead, now time.Time, skew time.Duration) error {
	if skew < 0 {
		return fmt.Errorf("audit: clock skew %s is negative: %w", skew, agent.ErrConfig)
	}
	if th.TimestampNanos <= 0 {
		return notVerified("audit: the %s head of run %q has timestamp %d; a signed head's timestamp is positive Unix nanoseconds", th.Kind, th.RunID, th.TimestampNanos)
	}
	if limit := now.Add(skew); th.TimestampNanos > limit.UnixNano() {
		return notVerified("audit: the %s head of run %q was signed at %s, after the verifier's clock (%s) plus the allowed skew %s",
			th.Kind, th.RunID, time.Unix(0, th.TimestampNanos).UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), skew)
	}
	return nil
}

// CheckTimestampOrder checks that later, a head that extends earlier, was not signed before it. A
// pair out of order is an error wrapping ErrNotVerified.
func CheckTimestampOrder(earlier, later TreeHead) error {
	if later.TimestampNanos < earlier.TimestampNanos {
		return notVerified("audit: the %s head of run %q (timestamp %d) is earlier than the %s head it extends (timestamp %d)",
			later.Kind, later.RunID, later.TimestampNanos, earlier.Kind, earlier.TimestampNanos)
	}
	return nil
}

// WithVerifyTime sets the clock EvidencePackage.Verify checks timestamps against (time.Now by
// default), so a package can be checked as of a given moment.
func WithVerifyTime(now time.Time) EvidenceVerifyOption {
	return func(o *evidenceVerifyOptions) { o.now, o.hasNow = now, true }
}

// WithClockSkew sets how far past the verifier's clock a signed head's timestamp may be
// (DefaultClockSkew by default). A negative skew makes every timestamp check fail.
func WithClockSkew(d time.Duration) EvidenceVerifyOption {
	return func(o *evidenceVerifyOptions) { o.skew, o.hasSkew = d, true }
}

// clock returns the time and skew the options set, or the defaults.
func (o evidenceVerifyOptions) clock() (time.Time, time.Duration) {
	now, skew := time.Now(), DefaultClockSkew
	if o.hasNow {
		now = o.now
	}
	if o.hasSkew {
		skew = o.skew
	}
	return now, skew
}
