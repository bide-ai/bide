package crlrefresh

import (
	"github.com/bide-ai/bide/audit"
	"net/url"
	"testing"
	"time"
)

func tm(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// a 30 minute CA with a one hour limit, and a weekly-ish root with a two day limit.
var (
	fastCA = issuerTiming{Period: 30 * time.Minute, Tolerance: time.Minute, Limit: time.Hour}
	root   = issuerTiming{Period: 24 * time.Hour, Tolerance: time.Minute, Limit: 48 * time.Hour, Tier: true}
)

func TestIntervalIsHalfThePeriodAtLeastThirtySeconds(t *testing.T) {
	if got := fastCA.interval(fastCA.Period); got != 15*time.Minute {
		t.Fatalf("%v", got)
	}
	// limit/4 is the smaller term when the period is long compared with the limit.
	if got := (issuerTiming{Period: time.Hour, Limit: 2 * time.Hour}).interval(time.Hour); got != 30*time.Minute {
		t.Fatalf("%v", got)
	}
	if got := (issuerTiming{Period: time.Minute, Limit: time.Hour}).interval(time.Minute); got != minRefreshInterval {
		t.Fatalf("floor: %v", got)
	}
	if catchUp(time.Hour) != 3*time.Minute+45*time.Second || catchUp(24*time.Hour) != maxCatchUp || catchUp(time.Minute) != minRefreshInterval {
		t.Fatal("catch-up clamp")
	}
	if attemptDeadline(15*time.Minute) != 2*time.Minute || attemptDeadline(30*time.Second) != 30*time.Second {
		t.Fatal("attempt deadline is min(interval, 2 minutes)")
	}
}

func TestWaitSchedule(t *testing.T) {
	now := tm("2026-10-01T12:00:00Z")
	iv := fastCA.interval(fastCA.Period) // 15m
	st := feedState{have: true, this: now.Add(-time.Minute), lastAttempt: now}
	if got := fastCA.wait(feedState{}, fastCA.Period, now, 0.5); got != 0 {
		t.Fatalf("first attempt is immediate: %v", got)
	}
	// Regular: the interval, jittered to [0.9, 1.1].
	for _, rnd := range []float64{0, 0.5, 0.999} {
		got := fastCA.wait(st, fastCA.Period, now, rnd)
		if lo, hi := time.Duration(0.9*float64(iv)), time.Duration(1.1*float64(iv)); got < lo || got > hi {
			t.Fatalf("rnd %v: wait %v outside [%v, %v]", rnd, got, lo, hi)
		}
	}
	// Never later than the expected next CRL.
	st.this = now.Add(-25 * time.Minute) // next expected at +5m
	if got := fastCA.wait(st, fastCA.Period, now, 0.5); got != 5*time.Minute {
		t.Fatalf("wait %v, want to wake at the expected issue", got)
	}
	// Declared skew moves the expected issue later.
	sk := fastCA
	sk.Skew = 10 * time.Minute
	if got := sk.wait(st, sk.Period, now, 0.5); got != 15*time.Minute {
		t.Fatalf("with skew: %v", got)
	}
	// Overdue: catch-up polling, 30m/16 clamped to 30s... 112s.
	st.this = now.Add(-40 * time.Minute)
	got := fastCA.wait(st, fastCA.Period, now, 0.5)
	if want := catchUp(fastCA.Period); got != want {
		t.Fatalf("overdue wait %v, want %v", got, want)
	}
	// A root asks twice a cycle, then every 5 minutes once overdue.
	rst := feedState{have: true, this: now.Add(-23 * time.Hour), lastAttempt: now}
	if got := root.wait(rst, root.Period, now, 0.5); got != time.Hour {
		t.Fatalf("root wakes at the expected issue: %v", got)
	}
	rst.this = now.Add(-25 * time.Hour)
	if got := root.wait(rst, root.Period, now, 0.5); got != maxCatchUp {
		t.Fatalf("overdue root: %v", got)
	}
}

func TestBackoffDoublesToTheIntervalAndResets(t *testing.T) {
	now := tm("2026-10-01T12:00:00Z")
	iv := 15 * time.Minute
	var prev time.Duration
	for n := 1; n <= 8; n++ {
		d := backoff(n, iv, 1.0, 0)
		if d < prev {
			t.Fatalf("backoff shrank at %d: %v < %v", n, d, prev)
		}
		if d > iv {
			t.Fatalf("backoff %v above the interval", d)
		}
		prev = d
	}
	if prev != iv {
		t.Fatalf("backoff did not reach the interval: %v", prev)
	}
	if got := backoff(1, iv, 0, 0); got != 15*time.Second {
		t.Fatalf("first backoff with no jitter draw: %v (half of 30s)", got)
	}
	// Retry-After longer than the backoff is honoured, up to the interval.
	if got := backoff(1, iv, 0.5, 5*time.Minute); got != 5*time.Minute {
		t.Fatalf("retry-after: %v", got)
	}
	if got := backoff(1, iv, 0.5, time.Hour); got != iv {
		t.Fatalf("retry-after beyond the interval: %v", got)
	}
	// A failed feed backs off; a success returns to the schedule.
	st := feedState{have: true, this: now, lastAttempt: now, failures: 3}
	if got := fastCA.wait(st, fastCA.Period, now, 0.5); got > fastCA.interval(fastCA.Period) {
		t.Fatalf("failed feed waits %v", got)
	}
	st.failures = 0
	if got := fastCA.wait(st, fastCA.Period, now, 0.5); got < 13*time.Minute {
		t.Fatalf("success did not reset the backoff: %v", got)
	}
}

func TestAlarmThresholds(t *testing.T) {
	iv := fastCA.interval(fastCA.Period)
	// A fast CA: half the limit is 30m, the CRL cycle plus tolerance 31m: the cycle governs, under limit-I.
	if got := fastCA.alarmAge(iv); got != 31*time.Minute {
		t.Fatalf("fast CA alarm age %v", got)
	}
	// A weekly-style root with a limit of one period would alarm in mid-cycle at limit/2; the cycle governs.
	rt := issuerTiming{Period: 24 * time.Hour, Tolerance: time.Minute, Limit: 48 * time.Hour, Tier: true}
	if got := rt.alarmAge(rt.interval(rt.Period)); got != 24*time.Hour+time.Minute {
		t.Fatalf("root alarm age %v: a healthy root is 24h old before its next CRL", got)
	}
	// A limit barely above the cycle: capped one probe interval below the limit, so the alarm precedes the pause.
	tight := issuerTiming{Period: 24 * time.Hour, Tolerance: time.Minute, Limit: 24*time.Hour + 10*time.Minute, Tier: true}
	i := tight.interval(tight.Period)
	if got := tight.alarmAge(i); got != tight.Limit-i {
		t.Fatalf("tight alarm age %v", got)
	}
	// ... and the startup rule refuses that for a tier, and only warns for the leaf's CA.
	if _, err := tight.checkMargin(i); err == nil {
		t.Fatal("a tier with no room for an alarm was accepted")
	}
	leaf := tight
	leaf.Tier = false
	if w, err := leaf.checkMargin(i); err != nil || w == "" {
		t.Fatalf("leaf CA: %q %v", w, err)
	}
	if w, err := root.checkMargin(root.interval(root.Period)); err != nil || w != "" {
		t.Fatalf("a root with a two period limit: %q %v", w, err)
	}
}

func TestCriticalUsesTheStatusRulesOwnTest(t *testing.T) {
	this := tm("2026-10-01T12:00:00Z")
	next := this.Add(3 * time.Hour)
	tt := issuerTiming{Period: 30 * time.Minute, Tolerance: time.Minute, Limit: time.Hour, Skew: 10 * time.Minute}
	for _, c := range []struct {
		d    time.Duration
		want bool
	}{{time.Hour + 10*time.Minute, false}, {time.Hour + 10*time.Minute + time.Nanosecond, true}, {0, false}, {-2 * time.Minute, true}} {
		if got := tt.critical(this.Add(c.d), this, next); got != c.want {
			t.Errorf("age %v: critical=%v, want %v", c.d, got, c.want)
		}
	}
	if !tt.critical(next, this, next) {
		t.Error("a CRL at its nextUpdate is not fresh")
	}
}

func TestRedirectRule(t *testing.T) {
	u := func(s string) *url.URL { p, _ := url.Parse(s); return p }
	for _, c := range []struct {
		origin, next string
		hops         int
		refused      bool
	}{
		{"http://pki.example/a.crl", "http://pki.example/b.crl", 1, false},
		{"http://pki.example/a.crl", "https://pki.example/b.crl", 1, false}, // the usual move to TLS
		{"http://pki.example:80/a.crl", "https://pki.example:443/b.crl", 1, false},
		{"http://pki.example/a.crl", "https://pki.example:8443/b.crl", 1, true}, // not the default port
		{"http://pki.example:8080/a.crl", "https://pki.example/b.crl", 1, true}, // not the default port
		{"http://pki.example/a.crl", "https://other.example/b.crl", 1, true},
		{"https://pki.example/a.crl", "https://PKI.example:443/b.crl", 1, false},
		{"https://pki.example/a.crl", "http://pki.example:443/b.crl", 1, true}, // downgrade
		{"http://pki.example/a.crl", "http://other.example/a.crl", 1, true},
		{"http://pki.example/a.crl", "http://pki.example:8080/a.crl", 1, true},
		{"http://pki.example/a.crl", "http://pki.example/b.crl", 4, true},
		{"http://pki.example/a.crl", "http://pki.example/b.crl", 3, false},
	} {
		got := redirectRefusal(u(c.origin), u(c.next), c.hops) != ""
		if got != c.refused {
			t.Errorf("%s -> %s (%d hops): refused=%v, want %v", c.origin, c.next, c.hops, got, c.refused)
		}
	}
}

func TestURLRedaction(t *testing.T) {
	for in, want := range map[string]string{
		"http://u:p@h.example/a.crl?token=S#f": "http://h.example/a.crl",
		"https://h.example/a.crl?x=1":          "https://h.example/a.crl",
	} {
		if got := audit.RedactURL(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
	if _, err := audit.CheckCRLURL("ldap://h.example/cn=x"); err == nil {
		t.Error("ldap accepted")
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := tm("2026-10-01T12:00:00Z")
	if parseRetryAfter("120", now) != 2*time.Minute || parseRetryAfter("", now) != 0 || parseRetryAfter("soon", now) != 0 || parseRetryAfter("-5", now) != 0 {
		t.Fatal("delta-seconds")
	}
	if got := parseRetryAfter(now.Add(90*time.Second).Format("Mon, 02 Jan 2006 15:04:05 GMT"), now); got != 90*time.Second {
		t.Fatalf("http date: %v", got)
	}
	if parseRetryAfter("99999999", now) != maxRetryAfterRaw {
		t.Fatal("cap")
	}
}

func TestFileNameRoundTrip(t *testing.T) {
	a, ok := parseFileName("12345678901234567890-20261001T120000Z.rollback-0a1b2c3d.crl")
	if !ok || a.Marker != MarkerRollback || a.Number.String() != "12345678901234567890" || !a.ThisUpdate.Equal(tm("2026-10-01T12:00:00Z")) {
		t.Fatalf("%+v %v", a, ok)
	}
	for _, bad := range []string{"x-20261001T120000Z.crl", "-5-20261001T120000Z.crl", "5-notatime.crl", "5.crl", "5-20261001T120000Z.weird.crl"} {
		if _, ok := parseFileName(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}
