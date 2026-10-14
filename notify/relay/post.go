package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Defaults of an outbound call, shared by the Slack and PagerDuty sinks.
const (
	// DefaultAttempts is how many requests one Send may make.
	DefaultAttempts = 3
	// DefaultRequestTimeout bounds each request.
	DefaultRequestTimeout = 3 * time.Second
	// DefaultBackoff is the wait before the second request; it doubles for each further one.
	DefaultBackoff = 250 * time.Millisecond
	// maxRetryAfter is the longest Retry-After a Send waits out. The sender's own request has a
	// timeout too (10 s unless configured), so a long wait is worse than failing now.
	maxRetryAfter = 2 * time.Second
	// maxErrBody is how much of a refusal's body goes in the error text.
	maxErrBody = 200
)

// outbound posts JSON to a third party with a timeout and a few bounded retries. Its errors name
// the sink and the host or status, never the URL: a Slack webhook URL is itself a credential.
type outbound struct {
	name     string
	rawURL   string
	host     string
	client   *http.Client
	attempts int
	timeout  time.Duration
	backoff  time.Duration
	sleep    func(context.Context, time.Duration) error
}

func newOutbound(name, rawURL string, client *http.Client, attempts int, timeout, backoff time.Duration) (*outbound, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("relay: %s: the URL is not a valid absolute URL", name)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil, fmt.Errorf("relay: %s: the URL must be https (http is allowed only for localhost, 127.0.0.1 and [::1])", name)
	}
	if attempts <= 0 {
		attempts = DefaultAttempts
	}
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	if backoff <= 0 {
		backoff = DefaultBackoff
	}
	if client == nil {
		client = &http.Client{}
	}
	// A body that carries a credential-bearing request must not go somewhere the operator did not
	// name: copy the client so the redirect rule is ours and the caller's client is untouched.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &outbound{name: name, rawURL: rawURL, host: u.Host, client: &c, attempts: attempts, timeout: timeout, backoff: backoff, sleep: sleepCtx}, nil
}

func isLoopback(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// post sends body and returns nil once the third party answered 2xx. Network errors, timeouts,
// 429 and 5xx are retried within the attempts and ctx's deadline; any other status is final.
func (o *outbound) post(ctx context.Context, body []byte) error {
	var last error
	wait := o.backoff
	for attempt := 1; attempt <= o.attempts; attempt++ {
		status, retryAfter, err := o.once(ctx, body)
		if err == nil {
			return nil
		}
		last = err
		if status != 0 && status != http.StatusTooManyRequests && status < 500 {
			return err // a refusal retrying cannot fix
		}
		if ctx.Err() != nil || attempt == o.attempts {
			break
		}
		d := wait
		if retryAfter > 0 {
			d = min(retryAfter, maxRetryAfter)
		}
		if dl, ok := ctx.Deadline(); ok && time.Now().Add(d).After(dl) {
			break
		}
		if o.sleep(ctx, d) != nil {
			break
		}
		wait *= 2
	}
	return last
}

func (o *outbound) once(ctx context.Context, body []byte) (status int, retryAfter time.Duration, err error) {
	rctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, o.rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, 0, fmt.Errorf("relay: %s: building the request failed", o.name)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, 0, fmt.Errorf("relay: %s: POST to %s failed: %w", o.name, o.host, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody+1))
	if resp.StatusCode/100 == 2 {
		return resp.StatusCode, 0, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		if n, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && n > 0 {
			retryAfter = time.Duration(n) * time.Second
		}
	}
	return resp.StatusCode, retryAfter, &StatusError{Sink: o.name, Status: resp.StatusCode, Body: printable(b)}
}

// A StatusError is a non-2xx answer from a third party. Body is the start of its reply, which both
// Slack ("no_service") and PagerDuty (a JSON message) use to say what is wrong; it holds no URL.
type StatusError struct {
	Sink   string
	Status int
	Body   string
}

// Error names the sink, the status and the start of the reply; never a URL.
func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("relay: %s answered HTTP %d", e.Sink, e.Status)
	}
	return fmt.Sprintf("relay: %s answered HTTP %d: %s", e.Sink, e.Status, e.Body)
}

// printable keeps the printable start of a reply for an error line.
func printable(b []byte) string {
	if len(b) > maxErrBody {
		b = b[:maxErrBody]
	}
	s := strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, string(b))
	return strings.TrimSpace(s)
}
