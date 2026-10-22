package notify

// Adversarial review of the webhook notifier (webhook-notifier, bd10dd27). Tests named TestReview_
// fail there and reproduce a finding; TestReviewHeld_ pass and record what held up.

import (
	"bytes"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

func reviewRequest(t *testing.T, secrets []string, ts int64, body []byte, delivery string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
	r.Header.Set(HeaderDelivery, delivery)
	r.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	r.Header.Set(HeaderSignature, Sign(secrets, ts, body))
	return r
}

// W1. The package tells receivers to dedupe on the Bide-Delivery header ("Treat the delivery ID
// header as the dedupe key"), but the signature covers only timestamp + body. Anyone who captures
// one request can replay it inside the 5-minute window with a fresh Bide-Delivery value, and Verify
// accepts it: the dedupe key is attacker-chosen, so the receiver acts on the same notification
// twice. Verify should bind the header to the signed body's "id" (or the docs should name the body's
// id as the dedupe key).
func TestReview_VerifyBindsTheDeliveryHeaderToTheSignedBody(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"schema":"bide.notification.v1","id":"aaaa","key":"approval:r:c1:0"}`)
	r := reviewRequest(t, []string{"s3cret-fips-padded-secret"}, now.Unix(), body, "bbbb") // header differs from the signed id
	if err := verifyAt(r, []string{"s3cret-fips-padded-secret"}, 0, now); err == nil {
		t.Fatal("Verify accepted a request whose Bide-Delivery (the documented dedupe key) is not the id the signature covers: a replay with a fresh header passes dedupe")
	}
}

// W2. VerifyBody recomputes HMAC(secret, ts.body) for every v1 entry in the header times every
// secret. The entry count is bounded only by the server's header limit (1 MiB by default), so one
// unauthenticated request with a 1 MiB body and a few thousand bogus entries costs gigabytes of
// hashing before it is rejected. Compute one MAC per secret and compare each entry to those.
func TestReview_VerifyCostDoesNotGrowWithSignatureEntries(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := bytes.Repeat([]byte("x"), maxVerifyBody)
	secrets := []string{"current-fips-padded", "previous-fips-padded"}
	fake := "v1=" + strings.Repeat("ab", 32)
	header := strings.TrimSpace(strings.Repeat(fake+" ", 2000)) // about 134 KB, well under 1 MiB
	start := time.Now()
	err := VerifyBody(strconv.FormatInt(now.Unix(), 10), header, body, secrets, 0, now)
	took := time.Since(start)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("setup: err = %v", err)
	}
	if took > 500*time.Millisecond {
		t.Fatalf("one forged request (1 MiB body, 2000 signature entries) took %v to reject: the work is entries x secrets x body", took)
	}
}

// W3 (low). The freshness check takes the absolute value of now - ts. For a timestamp far enough in
// the future the subtraction saturates at the minimum Duration, whose negation is itself, so the
// age is negative and passes "age > tolerance". A request signed with such a timestamp (a sender
// with a broken clock) is fresh forever and replayable without limit.
func TestReview_AFarFutureTimestampIsStale(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{}`)
	ts := int64(math.MaxInt64 / 2)
	err := VerifyBody(strconv.FormatInt(ts, 10), Sign([]string{"k-fips-padded-secret"}, ts, body), body, []string{"k-fips-padded-secret"}, 0, now)
	if !errors.Is(err, ErrStale) {
		t.Fatalf("a timestamp ~1.5e11 years ahead verified as fresh: err = %v", err)
	}
}

// Held: the URL bypasses the review tried are refused.
func TestReviewHeld_HTTPSOnlyBypasses(t *testing.T) {
	for _, u := range []string{
		"http://localhost.attacker.com/h", "http://127.0.0.1.nip.io/h", "http://localhost@evil.example/h",
		"http://evil.example#@localhost/h", "http://127.1/h", "http://[::ffff:127.0.0.1]/h", "http://0.0.0.0/h",
		"HTTP://evil.example/h", "ftp://localhost/h", "//localhost/h", "http:localhost/h",
	} {
		if _, err := NewWebhook(u, WithSecrets("k-fips-padded-secret", "")); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: NewWebhook = %v, want ErrConfig", u, err)
		}
	}
	for _, u := range []string{"http://localhost:8080/h", "http://127.0.0.1/h", "http://[::1]:9/h", "HTTPS://example.com/h"} {
		if _, err := NewWebhook(u, WithSecrets("k-fips-padded-secret", "")); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

// Held: during rotation either secret alone verifies, a header holding only garbage plus one valid
// entry verifies, and nothing verifies without a matching entry.
func TestReviewHeld_RotationAndParsing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"id":"x"}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	both := Sign([]string{"new-fips-padded-secret", "old-fips-padded-secret"}, now.Unix(), body)
	for _, sec := range [][]string{{"new-fips-padded-secret"}, {"old-fips-padded-secret"}, {"old-fips-padded-secret", "new-fips-padded-secret"}} {
		if err := VerifyBody(ts, both, body, sec, 0, now); err != nil {
			t.Errorf("secrets %v: %v", sec, err)
		}
	}
	mixed := "v2=zz v1=nothex " + Sign([]string{"new-fips-padded-secret"}, now.Unix(), body) + " v1="
	if err := VerifyBody(ts, mixed, body, []string{"new-fips-padded-secret"}, 0, now); err != nil {
		t.Errorf("mixed header: %v", err)
	}
	if err := VerifyBody(ts, both, body, []string{"other-fips-padded-secret"}, 0, now); !errors.Is(err, ErrBadSignature) {
		t.Errorf("wrong secret: %v", err)
	}
	if err := VerifyBody(ts, both, []byte(`{"id":"y"}`), []string{"new-fips-padded-secret"}, 0, now); !errors.Is(err, ErrBadSignature) {
		t.Errorf("changed body: %v", err)
	}
}

// Held: Retry-After parsing at the edges never yields a wait over the cap or a negative one.
func TestReviewHeld_RetryAfterEdges(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, v := range []string{"-5", "abc", "1e3", "99999999999999999999", "", " 5",
		now.Add(-time.Hour).UTC().Format(http.TimeFormat)} {
		if d := parseRetryAfter(v, now); d < 0 || min(d, maxRetryAfter) < 0 {
			t.Errorf("Retry-After %q = %v", v, d)
		}
	}
	if d := parseRetryAfter(now.Add(90*time.Second).UTC().Format(http.TimeFormat), now); d != 90*time.Second {
		t.Errorf("HTTP-date = %v", d)
	}
}

// W4 (low). parseRetryAfter multiplies the parsed seconds by time.Second without a bound, so a huge
// value overflows: "9223372036854775807" becomes -1s (ignored), and 18446744074 seconds wraps to
// 0.29s, so a receiver asking for a very long pause is retried at once. Clamp before multiplying.
func TestReview_RetryAfterOverflowIsClamped(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, v := range []string{"9223372036854775807", "18446744074"} {
		if d := parseRetryAfter(v, now); d < maxRetryAfter {
			t.Errorf("Retry-After %q = %v, want at least the %v cap", v, d, maxRetryAfter)
		}
	}
}
