// Package provider is the kit a model adapter is built from: HTTP error classification, SSE
// framing, a cap on a streamed reply, Retry-After parsing, and the tool-result codec. The
// first-party adapters (model/anthropic, model/openai, model/gemini) use it, and a third-party
// adapter can too.
//
// The agent core never imports this package. It holds adapter plumbing, not the contract a Model
// meets: that contract (agent.Model, agent.Stream, agent.NewStreamFunc, agent.Emit and the
// events, agent.APIError, agent.RateLimited and the error sentinels) stays in package agent.
package provider

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// ParseRetryAfter parses an HTTP Retry-After header value. It accepts either an integer number of
// seconds or an HTTP-date, and returns 0 if the value is absent, unparseable, negative, or already
// in the past. A number of seconds too large for a time.Duration saturates at the longest one
// rather than overflowing.
func ParseRetryAfter(s string) time.Duration {
	if s == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		switch {
		case secs <= 0:
			return 0
		case secs > math.MaxInt64/int64(time.Second):
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(s); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// A provider's error text is bounded wherever it lands: ClassifyHTTPError reads at most
// maxErrorRead bytes of a failed response (enough for any provider's error object), and the
// body or message an error carries is cut to maxErrorBody bytes, ending in truncatedNote. A
// broken or hostile endpoint, or one that echoes the prompt back, cannot turn one error into
// megabytes of memory and log line.
const (
	maxErrorRead  = 64 << 10
	maxErrorBody  = 8 << 10
	truncatedNote = " ...(truncated)"
)

// truncate cuts s to maxErrorBody bytes, marking the cut.
func truncate(s string) string { return cutTo(s, maxErrorBody) }

// cutTo cuts s to n bytes, marking the cut.
func cutTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + truncatedNote
}

// ClassifyHTTPError turns a non-2xx model response into the appropriate SDK error. It reads at
// most 64KB of the body and parses the provider's error object ({"error":{...}} from OpenAI,
// Anthropic, and Gemini) so the result carries the provider's message, type, and code, each
// (and the Body) cut to 8KB:
//
//   - The account's quota or credit is used up (HTTP 402; an error type or code of
//     insufficient_quota, billing_hard_limit_reached, billing_not_active, or billing_error; a
//     Gemini quota violation whose quotaId is per day): an *agent.APIError wrapping agent.ErrQuotaExhausted,
//     whatever the status, since no wait lifts it.
//   - HTTP 429 otherwise: a *agent.RateLimited whose RetryAfter is the Retry-After header or, without
//     one, Gemini's RetryInfo.retryDelay.
//   - Anything else: an *agent.APIError with the status code.
//
// It reads and closes resp.Body, so call it only on a response the adapter is abandoning (not one
// it will go on to stream). provider names the adapter for the wrapped message.
func ClassifyHTTPError(provider string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorRead))
	resp.Body.Close()
	pe, _ := parseProviderError(b)
	return classifyProviderError(provider, resp.StatusCode, ParseRetryAfter(resp.Header.Get("Retry-After")), pe, string(b))
}

// ClassifyStreamError turns an error object a provider sends inside a 200 stream (an OpenAI or
// Gemini data line holding {"error":{...}}, an Anthropic "error" event) into an SDK error. With
// a numeric code (Gemini sends the HTTP status) it is classified as ClassifyHTTPError would a
// response with that status. Without one, an exhausted quota is an *agent.APIError wrapping
// agent.ErrQuotaExhausted, a rate limit (type or code rate_limit_error or rate_limit_exceeded) is a
// *agent.RateLimited, and anything else (overloaded, server_error) is an agent.ErrModel carrying the
// provider's message, which middleware.Retryable retries.
func ClassifyStreamError(provider string, data []byte) error {
	pe, ok := parseProviderError(data)
	if !ok {
		return fmt.Errorf("%s stream error: %s (%w)", provider, truncate(string(data)), agent.ErrModel)
	}
	if pe.status != 0 {
		return classifyProviderError(provider, pe.status, 0, pe, string(data))
	}
	if pe.quotaExhausted(0) {
		return classifyProviderError(provider, 0, 0, pe, string(data))
	}
	if pe.Type == "rate_limit_error" || pe.Code == "rate_limit_exceeded" {
		return classifyProviderError(provider, http.StatusTooManyRequests, 0, pe, string(data))
	}
	return fmt.Errorf("%s stream error: %s (%w)", provider, pe.describe(truncate(string(data))), agent.ErrModel)
}

// providerError is the error object OpenAI, Anthropic, and Gemini put in a failed response body
// or a mid-stream error event.
type providerError struct {
	Message string
	Type    string // OpenAI and Anthropic "type"; Gemini "status" (RESOURCE_EXHAUSTED, ...)
	Code    string // OpenAI's string "code"
	status  int    // Gemini's numeric "code", the HTTP status
	delay   time.Duration
	perDay  bool // a Gemini QuotaFailure violation names a per-day quota
}

// parseProviderError reads b as {"error":{...}}, or a JSON array whose first element is one (the
// framing Gemini uses without alt=sse). ok is false when b holds no error object.
func parseProviderError(b []byte) (providerError, bool) {
	type wire struct {
		Error *struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Status  string          `json:"status"`
			Code    json.RawMessage `json:"code"`
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string `json:"retryDelay"`
				Violations []struct {
					QuotaID string `json:"quotaId"`
				} `json:"violations"`
			} `json:"details"`
		} `json:"error"`
	}
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		var ws []wire
		if json.Unmarshal(b, &ws) != nil || len(ws) == 0 {
			return providerError{}, false
		}
		w = ws[0]
	}
	if w.Error == nil {
		return providerError{}, false
	}
	e := w.Error
	pe := providerError{Message: truncate(e.Message), Type: truncate(cmp.Or(e.Type, e.Status))}
	if err := json.Unmarshal(e.Code, &pe.status); err != nil {
		json.Unmarshal(e.Code, &pe.Code) // a string code (OpenAI); null or absent leaves it empty
		pe.Code = truncate(pe.Code)
	}
	for _, d := range e.Details {
		switch {
		case strings.HasSuffix(d.Type, "google.rpc.RetryInfo"):
			// A protobuf Duration in JSON: decimal seconds with an "s" suffix, e.g. "37.5s".
			if dur, err := time.ParseDuration(d.RetryDelay); err == nil && dur > 0 {
				pe.delay = dur
			}
		case strings.HasSuffix(d.Type, "google.rpc.QuotaFailure"):
			for _, v := range d.Violations {
				if strings.Contains(v.QuotaID, "PerDay") {
					pe.perDay = true
				}
			}
		}
	}
	return pe, true
}

// quotaExhausted reports whether the error says the account's quota or credit is used up.
func (pe providerError) quotaExhausted(status int) bool {
	if status == http.StatusPaymentRequired || pe.perDay {
		return true
	}
	for _, s := range []string{pe.Type, pe.Code} {
		switch s {
		case "insufficient_quota", "billing_hard_limit_reached", "billing_not_active", "billing_error":
			return true
		}
	}
	return false
}

// describe is the provider's message with its type or code, or body when there is no message.
func (pe providerError) describe(body string) string {
	if pe.Message == "" {
		return body
	}
	if kind := cmp.Or(pe.Code, pe.Type); kind != "" {
		return pe.Message + " (" + kind + ")"
	}
	return pe.Message
}

// classifyProviderError builds the SDK error for a provider error with the given HTTP status
// (0 for a mid-stream error that carries none). retryAfter is the Retry-After header's hint.
func classifyProviderError(provider string, status int, retryAfter time.Duration, pe providerError, body string) error {
	if pe.quotaExhausted(status) {
		return &agent.APIError{StatusCode: status, Body: truncate(body), Message: pe.Message, Type: pe.Type, Code: pe.Code,
			Err: fmt.Errorf("%s: %w", provider, agent.ErrQuotaExhausted)}
	}
	if status == http.StatusTooManyRequests {
		return &agent.RateLimited{
			RetryAfter: cmp.Or(retryAfter, pe.delay),
			Message:    pe.Message,
			Err:        fmt.Errorf("%s: rate limited (%w)", provider, agent.ErrModel),
		}
	}
	return &agent.APIError{StatusCode: status, Body: truncate(body), Message: pe.Message, Type: pe.Type, Code: pe.Code,
		Err: fmt.Errorf("%s (%w)", provider, agent.ErrModel)}
}

// MaxSSELine is the longest SSE line NewSSEScanner reads. It is sized for the largest single
// event a model streams: a big tool-call arguments blob, or an image Gemini returns inline as
// base64, which runs to several megabytes.
const MaxSSELine = 32 << 20

// NewSSEScanner returns a bufio.Scanner over an SSE stream body with the default 64KB line cap raised
// to MaxSSELine. A single SSE line can carry a large tool-call arguments blob or inline image, and the
// 64KB default would fail the stream on it (the openai-go #368 lesson). The buffer grows only as a
// line needs it. Every adapter reads its event stream through this so the cap is fixed in exactly one
// place; pass the scanner's Err to SSEReadError. Over a body from LimitResponse, a reply that
// exceeds its cap ends the scan with that error before the cut-off last line is returned, so an
// adapter never decodes a line the cap truncated.
func NewSSEScanner(body io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), MaxSSELine)
	if lb, ok := body.(*limitedBody); ok {
		sc.Split(lb.split)
	}
	return sc
}

// DefaultMaxResponseBytes is the most bytes of one streamed model reply an adapter reads unless it
// is configured otherwise (each adapter's WithMaxResponseBytes). MaxSSELine caps one line; this
// caps the whole reply, so a reply of many small lines cannot grow without limit either. A line is
// part of the reply, so a cap below MaxSSELine also bounds every line.
const DefaultMaxResponseBytes = 32 << 20

// LimitResponse returns body limited to max bytes (DefaultMaxResponseBytes when max <= 0): once
// the reply has more, a read returns the bytes up to the limit and an error wrapping
// agent.ErrResponseTooLarge, which SSEReadError passes on. Closing the result closes body. Every adapter
// wraps its streamed reply with it, so the cap is enforced in one place.
func LimitResponse(body io.ReadCloser, max int64) io.ReadCloser {
	if max <= 0 {
		max = DefaultMaxResponseBytes
	}
	return &limitedBody{body: body, max: max, left: max}
}

// limitedBody is LimitResponse's reader.
type limitedBody struct {
	body      io.ReadCloser
	max, left int64
	err       error // set once the reply has run past max
}

// split is bufio.ScanLines, except that once the reply has run past its cap it does not return
// the partial last line the cap cut off: the lines read in full are returned, and then the scan ends
// with the cap's error, which the reader reported.
func (l *limitedBody) split(data []byte, atEOF bool) (int, []byte, error) {
	return bufio.ScanLines(data, atEOF && l.err == nil)
}

func (l *limitedBody) Read(p []byte) (int, error) {
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1] // one byte past the limit is enough to tell a longer reply
	}
	n, err := l.body.Read(p)
	if int64(n) > l.left {
		n, l.left = int(l.left), 0
		l.err = fmt.Errorf("the reply exceeded %d bytes: %w", l.max, agent.ErrResponseTooLarge)
		return n, l.err
	}
	l.left -= int64(n)
	return n, err
}

func (l *limitedBody) Close() error { return l.body.Close() }

// SSEReadError wraps a failed read of a model's SSE stream (the Err of a NewSSEScanner scanner) as
// an agent.ErrModel. A line over MaxSSELine, or a reply over its LimitResponse cap, is
// agent.ErrResponseTooLarge, which the same request would hit again, so middleware.Retryable does not
// retry it; any other read failure (a connection cut partway through) is retried.
func SSEReadError(provider string, err error) error {
	if errors.Is(err, agent.ErrResponseTooLarge) {
		return fmt.Errorf("%s: %w", provider, err)
	}
	if errors.Is(err, bufio.ErrTooLong) {
		return fmt.Errorf("%s: %w: a line exceeded %d bytes (%w)", provider, agent.ErrResponseTooLarge, MaxSSELine, err)
	}
	return fmt.Errorf("%s stream read: %w (%w)", provider, err, agent.ErrModel)
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
