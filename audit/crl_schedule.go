package audit

import (
	"fmt"
	"time"

	"github.com/bide-ai/bide/agent"
)

const (
	minRefreshInterval = 30 * time.Second
	maxCatchUp         = 5 * time.Minute
	maxAttemptDeadline = 2 * time.Minute
	defaultTolerance   = time.Minute // the status rule's CoverageTolerance default
)

// issuerTiming is what the refresher knows about one issuing CA from the approver's declared values: the
// same values the gate uses, so the alarm and the pause cannot disagree.
type issuerTiming struct {
	Period, Overlap time.Duration // the CA's declared CRL period and overlap
	Skew            time.Duration // the declared thisUpdate skew
	Tolerance       time.Duration // the status rule's coverage tolerance
	// Limit is the status age at which a gate starts to pause because of this CA: the smallest of the
	// ages it is judged by in each role it has (counting a decision; the leaf's issuing CA before a
	// call runs; a CA tier before a call runs).
	Limit time.Duration
	// Tier says the CA is declared a tier above the leaf's issuing CA (ExecStatusAgeFor), which is
	// where the margin between the CRL cycle and the limit is a requirement and not advice.
	Tier bool
}

// timingFor reads the declared values for the issuer with key k. It fails (ErrConfig) when a value the
// schedule needs is missing: a refresher with no period cannot know when the next CRL is due, and one
// with no MaxStatusAge would feed a snapshot the gate accepts nothing from.
func timingFor(a *CertApprover, k SPKIHash) (issuerTiming, error) {
	bad := func(format string, args ...any) (issuerTiming, error) {
		return issuerTiming{}, fmt.Errorf("audit: CRL refresher: issuer %x: %s: %w", k[:4], fmt.Sprintf(format, args...), agent.ErrConfig)
	}
	p := a.StatusParams
	t := issuerTiming{Period: p.CAPeriod, Skew: p.ThisUpdateSkew, Tolerance: p.CoverageTolerance}
	if v, ok := p.CAPeriodFor[k]; ok {
		t.Period = v
	}
	if v, ok := p.ThisUpdateSkewFor[k]; ok {
		t.Skew = v
	}
	if t.Tolerance <= 0 {
		t.Tolerance = defaultTolerance
	}
	t.Overlap = a.CAOverlapFor[k]
	maxAge := p.MaxStatusAge
	if v := p.MaxStatusAgeFor[k]; v > 0 {
		maxAge = v
	}
	if t.Period <= 0 {
		return bad("no CAPeriod or CAPeriodFor: the schedule cannot know when its next CRL is due")
	}
	if maxAge <= 0 {
		return bad("no MaxStatusAge: no status age is accepted, so a fetched CRL would never count")
	}
	t.Limit = maxAge
	if _, tier := a.ExecStatusAgeFor[k]; tier {
		t.Tier = true
		t.Limit = min(t.Limit, a.ExecStatusAgeFor[k])
	}
	// The leaf's issuing CA is a pinned intermediate, or an anchor when none is pinned.
	if _, inter := a.Policy.inter[k]; inter || (len(a.Policy.inter) == 0 && a.Policy.anchorBy[k] != nil) {
		if a.ExecStatusAge <= 0 {
			return bad("no ExecStatusAge")
		}
		t.Limit = min(t.Limit, a.ExecStatusAge)
	}
	return t, nil
}

// interval is the regular fetch interval for a feed whose CRLs come every period: min(period/2, Limit/4),
// at least 30 seconds. Startup validation (CAPeriod <= ExecStatusAge/2) makes period/2 the smaller
// term for the leaf's issuer.
func (t issuerTiming) interval(period time.Duration) time.Duration {
	return max(minRefreshInterval, min(period/2, t.Limit/4))
}

// catchUp is the poll interval while the next CRL is overdue.
func catchUp(period time.Duration) time.Duration {
	return min(max(period/16, minRefreshInterval), maxCatchUp)
}

// attemptDeadline bounds one attempt over all URLs.
func attemptDeadline(interval time.Duration) time.Duration { return min(interval, maxAttemptDeadline) }

// margin is how long the limit leaves after the CA's CRL cycle (period plus overlap) ends.
func (t issuerTiming) margin() time.Duration { return t.Limit - (t.Period + t.Overlap) }

// alarmAge is the age of the newest CRL beyond which the refresher alarms: half the limit, but not before
// the next CRL is due (a weekly root is half its limit old in the middle of every healthy cycle), and
// always at least one probe interval before the limit.
func (t issuerTiming) alarmAge(i time.Duration) time.Duration {
	return min(max(t.Limit/2, t.Period+t.Overlap+t.Tolerance), t.Limit-i)
}

// critical reports that a gate would not accept this CRL now, by the status rule's own freshness test.
func (t issuerTiming) critical(now, this, next time.Time) bool {
	return !statusFresh(now, this, next, t.Limit, t.Skew, t.Tolerance)
}

// checkMargin applies the startup rule: for a CA tier the limit must leave the probe interval and the
// tolerance after the CRL cycle, or the alarm could not come before the pause. For the leaf's issuing
// CA a thin margin is returned as a warning only.
func (t issuerTiming) checkMargin(i time.Duration) (warn string, err error) {
	need := t.Tolerance + i
	if m := t.margin(); m < need {
		msg := fmt.Sprintf("the limit (%v) leaves %v after the CRL cycle (period %v plus overlap %v); at least %v is needed for an alarm to come before gates pause: give the CRL a validity of two periods and raise ExecStatusAgeFor/MaxStatusAgeFor to it", t.Limit, m, t.Period, t.Overlap, need)
		if t.Tier {
			return "", fmt.Errorf("%s", msg)
		}
		return msg, nil
	}
	return "", nil
}

// feedState is what the schedule reads about one feed (the base or the delta CRLs of one issuer).
type feedState struct {
	have        bool      // a CRL of this kind is held
	this        time.Time // its thisUpdate
	lastAttempt time.Time
	failures    int           // consecutive failed attempts
	retryAfter  time.Duration // a server's Retry-After from the last attempt, if any
}

// jitter scales d by a factor in [0.9, 1.1] from rnd in [0, 1).
func jitter(d time.Duration, rnd float64) time.Duration {
	return time.Duration(float64(d) * (0.9 + 0.2*rnd))
}

// backoff is the wait after failures consecutive failures: 30s doubling up to the regular interval, with
// jitter over its upper half.
func backoff(failures int, interval time.Duration, rnd float64, retryAfter time.Duration) time.Duration {
	b := minRefreshInterval
	for i := 1; i < failures && b < interval; i++ {
		b *= 2
	}
	b = min(b, interval)
	d := b/2 + time.Duration(float64(b/2)*rnd)
	if retryAfter > d {
		d = min(retryAfter, interval)
	}
	return d
}

// wait is how long to sleep before the next attempt of a feed whose CRLs come every period. It is a pure
// function of the state, the declared values, the clock and the random draw.
func (t issuerTiming) wait(st feedState, period time.Duration, now time.Time, rnd float64) time.Duration {
	i := t.interval(period)
	switch {
	case st.lastAttempt.IsZero():
		return 0
	case st.failures > 0 || !st.have:
		return backoff(max(st.failures, 1), i, rnd, st.retryAfter)
	}
	exp := st.this.Add(t.Skew + period)
	var target time.Time
	if now.Before(exp) {
		target = st.lastAttempt.Add(jitter(i, rnd))
		if target.After(exp) {
			target = exp
		}
	} else {
		target = st.lastAttempt.Add(jitter(catchUp(period), rnd))
	}
	return max(0, target.Sub(now))
}
