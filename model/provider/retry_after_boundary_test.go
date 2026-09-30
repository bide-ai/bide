package provider

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// A Retry-After header is provider input. A negative number of seconds is not a wait (the
// documented result is 0), and a number of seconds too large for a time.Duration saturates at the
// longest wait rather than overflowing to a negative or tiny one.
func TestParseRetryAfter_NegativeAndOverflow(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"-5":                   0,
		"-1":                   0,
		"-9223372036854775808": 0,
		"9223372036":           9223372036 * time.Second, // the largest whole number of seconds that fits
		"9223372037":           time.Duration(math.MaxInt64),
		"10000000000":          time.Duration(math.MaxInt64),
		"18446744074":          time.Duration(math.MaxInt64),
		"99999999999999999999": time.Duration(math.MaxInt64), // beyond int64: Atoi fails on range
	} {
		if got := ParseRetryAfter(in); got != want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

// A negative Retry-After header must not hide the wait Gemini put in the body.
func TestClassifyHTTPError_NegativeRetryAfterKeepsBodyDelay(t *testing.T) {
	gemini := `{"error":{"code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"30s"}]}}`
	err := ClassifyHTTPError("prov", errResp(429, "-5", gemini))
	var rl *agent.RateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("err = %T %v, want *agent.RateLimited", err, err)
	}
	if rl.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want the body's 30s", rl.RetryAfter)
	}
}
