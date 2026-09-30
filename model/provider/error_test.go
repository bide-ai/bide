package provider

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func errResp(status int, retryAfter, body string) *http.Response {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

// A 429 that says the account is out of quota or credit is not a rate limit: no wait lifts
// it. It must not come back as *agent.RateLimited (which every retry policy retries), and the
// provider's message must survive.
func TestClassifyHTTPError_QuotaExhaustedIsNotARateLimit(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		msg    string
	}{
		"openai insufficient_quota": {429, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`, "You exceeded your current quota"},
		"openai billing_not_active": {429, `{"error":{"message":"Your account is not active","type":"invalid_request_error","code":"billing_not_active"}}`, "Your account is not active"},
		"anthropic billing_error":   {400, `{"type":"error","error":{"type":"billing_error","message":"Your credit balance is too low"}}`, "Your credit balance is too low"},
		"402":                       {402, `{"error":{"message":"Insufficient Balance"}}`, "Insufficient Balance"},
		"gemini daily quota": {429, `{"error":{"code":429,"message":"You exceeded your current quota, please check your plan","status":"RESOURCE_EXHAUSTED","details":[` +
			`{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier"}]},` +
			`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"43s"}]}}`, "You exceeded your current quota, please check your plan"},
	} {
		err := ClassifyHTTPError("prov", errResp(tc.status, "", tc.body))
		var rl *agent.RateLimited
		var ae *agent.APIError
		if errors.As(err, &rl) || !errors.As(err, &ae) || !errors.Is(err, agent.ErrQuotaExhausted) || !errors.Is(err, agent.ErrModel) {
			t.Errorf("%s: err = %T %v, want an *agent.APIError wrapping agent.ErrQuotaExhausted", name, err, err)
			continue
		}
		if ae.Message != tc.msg || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: Message = %q, error %q; want the provider's message %q", name, ae.Message, err, tc.msg)
		}
		if ae.StatusCode != tc.status {
			t.Errorf("%s: StatusCode = %d, want %d", name, ae.StatusCode, tc.status)
		}
	}
}

// A plain rate limit stays *agent.RateLimited, keeps the provider's message, and takes its wait from
// Retry-After or, failing that, Gemini's RetryInfo.retryDelay.
func TestClassifyHTTPError_RateLimitKeepsMessageAndDelay(t *testing.T) {
	gemini := `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerMinutePerProjectPerModel"}]},` +
		`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"37.5s"}]}}`
	for name, tc := range map[string]struct {
		retryAfter, body, msg string
		wait                  time.Duration
	}{
		"gemini retryDelay":  {"", gemini, "Resource has been exhausted", 37500 * time.Millisecond},
		"header wins":        {"2", gemini, "Resource has been exhausted", 2 * time.Second},
		"openai rate limit":  {"", `{"error":{"message":"Rate limit reached for requests","type":"requests","code":"rate_limit_exceeded"}}`, "Rate limit reached", 0},
		"anthropic":          {"7", `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`, "exceeded your rate limit", 7 * time.Second},
		"not json":           {"", `too many`, "", 0},
		"gemini array frame": {"", `[` + gemini + `]`, "Resource has been exhausted", 37500 * time.Millisecond},
	} {
		err := ClassifyHTTPError("prov", errResp(429, tc.retryAfter, tc.body))
		var rl *agent.RateLimited
		if !errors.As(err, &rl) || errors.Is(err, agent.ErrQuotaExhausted) {
			t.Errorf("%s: err = %T %v, want *agent.RateLimited", name, err, err)
			continue
		}
		if rl.RetryAfter != tc.wait {
			t.Errorf("%s: RetryAfter = %v, want %v", name, rl.RetryAfter, tc.wait)
		}
		if !strings.Contains(rl.Message, tc.msg) || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: Message = %q, error %q; want it to carry %q", name, rl.Message, err, tc.msg)
		}
	}
}

// Other statuses keep the provider's error fields on the *agent.APIError.
func TestClassifyHTTPError_ParsesProviderFields(t *testing.T) {
	err := ClassifyHTTPError("prov", errResp(400, "", `{"error":{"message":"Invalid schema for function","type":"invalid_request_error","param":"tools[0]","code":"invalid_function_parameters"}}`))
	var ae *agent.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %T", err)
	}
	if ae.Message != "Invalid schema for function" || ae.Type != "invalid_request_error" || ae.Code != "invalid_function_parameters" {
		t.Fatalf("agent.APIError = %+v", ae)
	}
	if !strings.Contains(err.Error(), "Invalid schema for function") {
		t.Fatalf("error %q lacks the provider's message", err)
	}
	err = ClassifyHTTPError("prov", errResp(503, "", `{"error":{"code":503,"message":"The model is overloaded.","status":"UNAVAILABLE"}}`))
	if !errors.As(err, &ae) || ae.Type != "UNAVAILABLE" || ae.Message != "The model is overloaded." {
		t.Fatalf("gemini agent.APIError = %+v", ae)
	}
}

// An error the provider sends inside a 200 stream is classified like one in a failed response,
// using the code it carries (Gemini) or its type (OpenAI, Anthropic).
func TestClassifyStreamError(t *testing.T) {
	err := ClassifyStreamError("prov", []byte(`{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}`))
	var ae *agent.APIError
	if !errors.As(err, &ae) || ae.StatusCode != 503 || !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("gemini 503: %T %v", err, err)
	}
	err = ClassifyStreamError("prov", []byte(`{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`))
	if !errors.As(err, &ae) || ae.StatusCode != 400 {
		t.Errorf("gemini 400: %T %v", err, err)
	}
	err = ClassifyStreamError("prov", []byte(`{"error":{"message":"The server had an error","type":"server_error"}}`))
	if !errors.Is(err, agent.ErrModel) || !strings.Contains(err.Error(), "The server had an error") || errors.As(err, &ae) {
		t.Errorf("openai server_error: %T %v", err, err)
	}
	err = ClassifyStreamError("prov", []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	var rl *agent.RateLimited
	if !errors.As(err, &rl) || rl.Message != "slow down" {
		t.Errorf("anthropic rate_limit_error: %T %v", err, err)
	}
	err = ClassifyStreamError("prov", []byte(`{"error":{"message":"Rate limit reached","type":"tokens","code":"rate_limit_exceeded"}}`))
	if !errors.As(err, &rl) || rl.Message != "Rate limit reached" {
		t.Errorf("openai rate_limit_exceeded: %T %v", err, err)
	}
	err = ClassifyStreamError("prov", []byte(`{"error":{"message":"quota","code":"insufficient_quota"}}`))
	if !errors.Is(err, agent.ErrQuotaExhausted) {
		t.Errorf("quota: %T %v", err, err)
	}
	err = ClassifyStreamError("prov", []byte(`not json`))
	if !errors.Is(err, agent.ErrModel) || !strings.Contains(err.Error(), "not json") {
		t.Errorf("unparsed: %T %v", err, err)
	}
}

// A failed response's body is read only so far, and the error text is bounded, whatever the
// endpoint sends: a hostile or broken endpoint must not turn one error into megabytes of
// memory and log line (a body that echoes the prompt would otherwise land in every trace).
func TestClassifyHTTPError_BoundsTheBody(t *testing.T) {
	huge := strings.Repeat("x", 32<<20)
	for name, body := range map[string]string{
		"plain":           huge,
		"message and pad": `{"error":{"message":"bad request","type":"invalid_request_error"}}` + huge,
	} {
		err := ClassifyHTTPError("prov", errResp(400, "", body))
		var ae *agent.APIError
		if !errors.As(err, &ae) {
			t.Fatalf("%s: err = %T", name, err)
		}
		if len(ae.Body) > maxErrorBody+len(truncatedNote) || !strings.HasSuffix(ae.Body, truncatedNote) {
			t.Errorf("%s: Body is %d bytes, want at most %d and marked truncated", name, len(ae.Body), maxErrorBody+len(truncatedNote))
		}
		if n := len(err.Error()); n > 2*maxErrorBody {
			t.Errorf("%s: error text is %d bytes", name, n)
		}
	}
	// A provider message is kept even when the body around it is cut.
	err := ClassifyHTTPError("prov", errResp(400, "", `{"error":{"message":"bad request","type":"invalid_request_error"}}`))
	if !strings.Contains(err.Error(), "bad request") {
		t.Errorf("err = %v, want the provider's message", err)
	}
	// An exhausted quota's body too.
	var qe *agent.APIError
	if err := ClassifyHTTPError("prov", errResp(402, "", huge)); !errors.As(err, &qe) || len(qe.Body) > maxErrorBody+len(truncatedNote) {
		t.Errorf("402: err = %T, Body %d bytes", err, len(qe.Body))
	}
	// Only so much of the body is read at all.
	body := &countingReader{r: strings.NewReader(huge)}
	ClassifyHTTPError("prov", &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(body)})
	if body.n > maxErrorRead {
		t.Errorf("read %d bytes of the body, want at most %d", body.n, maxErrorRead)
	}
	// So is a stream error's text.
	for _, data := range []string{huge, `{"error":{"message":"` + huge + `"}}`, `{"error":{"code":500,"message":"` + huge + `"}}`,
		`{"error":{"type":"server_error","detail":"` + huge + `"}}`, `{"error":{"message":"m","code":"` + huge + `"}}`,
		`{"error":{"message":"m","type":"` + huge + `"}}`, `{"error":{"message":"m","code":"insufficient_quota","pad":"` + huge + `"}}`} {
		if n := len(ClassifyStreamError("prov", []byte(data)).Error()); n > 2*maxErrorBody {
			t.Errorf("stream error text is %d bytes", n)
		}
	}
}

// A line longer than the scanner's cap is a deterministic failure the same request repeats: it
// must be an agent.ErrResponseTooLarge (an agent.ErrModel), not a bare bufio error. Lines up to the cap read.
func TestSSEReadError_LineTooLong(t *testing.T) {
	sc := NewSSEScanner(strings.NewReader("data: " + strings.Repeat("A", 8<<20) + "\n"))
	if !sc.Scan() {
		t.Fatalf("an 8MB line (a large inline image) did not scan: %v", sc.Err())
	}
	sc = NewSSEScanner(strings.NewReader("data: " + strings.Repeat("A", MaxSSELine) + "\n"))
	for sc.Scan() {
	}
	err := SSEReadError("prov", sc.Err())
	if !errors.Is(err, agent.ErrResponseTooLarge) || !errors.Is(err, agent.ErrModel) {
		t.Fatalf("err = %v, want agent.ErrResponseTooLarge", err)
	}
	if err := SSEReadError("prov", io.ErrUnexpectedEOF); !errors.Is(err, agent.ErrModel) || !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, agent.ErrResponseTooLarge) {
		t.Fatalf("read error = %v, want agent.ErrModel wrapping the cause", err)
	}
}
