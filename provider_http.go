package agent

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// This file holds the HTTP/SSE plumbing every model adapter (anthropic, gemini, openai) shares, so
// the retry-after parsing, error classification, and stream framing live in one place rather than
// being copied per provider.

// ParseRetryAfter parses an HTTP Retry-After header value. It accepts either an integer number of
// seconds or an HTTP-date, and returns 0 if the value is absent, unparseable, or already in the past.
func ParseRetryAfter(s string) time.Duration {
	if s == "" {
		return 0
	}
	if secs, err := strconv.Atoi(s); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(s); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// ClassifyHTTPError turns a non-2xx model response into the appropriate SDK error: a *RateLimited
// (with the Retry-After hint) for HTTP 429, or a *APIError carrying the status code and a body
// snippet otherwise. It reads and closes resp.Body, so call it only on a response the adapter is
// abandoning (not one it will go on to stream). provider names the adapter for the wrapped message.
func ClassifyHTTPError(provider string, resp *http.Response) error {
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimited{
			RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After")),
			Err:        fmt.Errorf("%s: rate limited (%w)", provider, ErrModel),
		}
	}
	return &APIError{StatusCode: resp.StatusCode, Body: string(b), Err: fmt.Errorf("%s (%w)", provider, ErrModel)}
}

// NewSSEScanner returns a bufio.Scanner over an SSE stream body with the default 64KB line cap raised
// to 1MB. A single SSE line can carry a large tool-call arguments blob, and the 64KB default would
// silently truncate it and desync the stream (the openai-go #368 lesson). Every adapter reads its
// event stream through this so the cap is fixed in exactly one place.
func NewSSEScanner(body io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return sc
}

// SSEPayload extracts the data payload from one raw SSE line. It reports ok=false for a line that is
// not `data:` framing (e.g. an `event:` line or a blank separator) or whose payload is empty, both of
// which callers skip; otherwise it returns the payload with the "data:" prefix stripped and
// surrounding whitespace trimmed. It does not interpret provider sentinels such as "[DONE]".
func SSEPayload(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	data := strings.TrimSpace(line[len("data:"):])
	if data == "" {
		return "", false
	}
	return data, true
}
