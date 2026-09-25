package agent

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"5", 5 * time.Second},
		{"120", 120 * time.Second},
		{"notanumber", 0},
	}
	for _, tc := range tests {
		if got := ParseRetryAfter(tc.in); got != tc.want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// An HTTP-date in the past yields 0 (never negative); one in the future is positive.
	if got := ParseRetryAfter(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)); got != 0 {
		t.Errorf("past HTTP-date = %v, want 0", got)
	}
	if got := ParseRetryAfter(time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)); got <= 0 {
		t.Errorf("future HTTP-date = %v, want > 0", got)
	}
}

func TestClassifyHTTPError(t *testing.T) {
	mk := func(status int, retryAfter string) *http.Response {
		h := http.Header{}
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader("boom"))}
	}

	err := ClassifyHTTPError("prov", mk(http.StatusTooManyRequests, "3"))
	var rl *RateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("429 -> %T, want *RateLimited", err)
	}
	if rl.RetryAfter != 3*time.Second {
		t.Errorf("RetryAfter = %v, want 3s", rl.RetryAfter)
	}
	if !errors.Is(err, ErrModel) {
		t.Error("RateLimited should wrap ErrModel")
	}

	err = ClassifyHTTPError("prov", mk(http.StatusInternalServerError, ""))
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("500 -> %T, want *APIError", err)
	}
	if ae.StatusCode != http.StatusInternalServerError || ae.Body != "boom" {
		t.Errorf("APIError = %+v, want status 500 body \"boom\"", ae)
	}
	if !errors.Is(err, ErrModel) {
		t.Error("APIError should wrap ErrModel")
	}
}

func TestSSEPayload(t *testing.T) {
	tests := []struct {
		line     string
		wantData string
		wantOK   bool
	}{
		{"data: {\"x\":1}", `{"x":1}`, true},
		{"data:{\"x\":1}", `{"x":1}`, true},
		{"data: ", "", false},
		{"data:", "", false},
		{"event: ping", "", false},
		{"", "", false},
		{"data: [DONE]", "[DONE]", true},
	}
	for _, tc := range tests {
		got, ok := SSEPayload(tc.line)
		if got != tc.wantData || ok != tc.wantOK {
			t.Errorf("SSEPayload(%q) = (%q, %v), want (%q, %v)", tc.line, got, ok, tc.wantData, tc.wantOK)
		}
	}
}

// TestNewSSEScanner_LargeLine verifies the scanner reads an SSE line larger than bufio's default
// 64KB cap (a big tool-call args blob), which the raised 1MB buffer must accommodate.
func TestNewSSEScanner_LargeLine(t *testing.T) {
	big := strings.Repeat("x", 200*1024)
	sc := NewSSEScanner(strings.NewReader("data: " + big + "\n"))
	if !sc.Scan() {
		t.Fatalf("scan failed on a %d-byte line: %v", len(big), sc.Err())
	}
	data, ok := SSEPayload(sc.Text())
	if !ok || data != big {
		t.Errorf("large line round-trip failed (ok=%v, len=%d, want %d)", ok, len(data), len(big))
	}
}
