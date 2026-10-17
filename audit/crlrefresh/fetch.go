package crlrefresh

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/bide-ai/bide/audit"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Fetch defaults. Each is the library's; an application client supplied in Options.Client brings
// its own transport.
const (
	dialTimeout      = 5 * time.Second
	tlsTimeout       = 5 * time.Second
	headerTimeout    = 10 * time.Second
	requestTimeout   = 60 * time.Second
	maxRedirects     = 3
	crlUserAgent     = "bide-crl-refresher/1"
	dateSkewWarnAt   = 5 * time.Minute
	maxRetryAfterRaw = 24 * time.Hour
)

// newCRLClient is the library's HTTP client: no proxy (library code never reads the environment; cmd
// builds a client that does), no transparent compression, timeouts on every stage.
func newCRLClient() *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
			TLSHandshakeTimeout:   tlsTimeout,
			ResponseHeaderTimeout: headerTimeout,
			DisableCompression:    true,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
		},
	}
}

// outcome kinds of one URL in one attempt.
const (
	outOK          = "ok"
	outNotModified = "not-modified"
	outOversize    = "oversize"
	outTimeout     = "timeout"
	outRedirect    = "redirect-refused"
	outHTTP        = "http-status"
	outError       = "error"
	outNotCRL      = "not-a-crl"
	// outRefused marks a URL whose CRL was fetched but not accepted; Detail says why.
	outRefused = "refused"
	// outStale marks a URL whose CRL was fetched, archived and added, but was already past its nextUpdate,
	// so it does not count as fresh.
	outStale = "stale"
)

// URLOutcome is the result of asking one URL in one attempt, for the event.
type URLOutcome struct {
	URL    string `json:"url"` // without query and fragment
	Kind   string `json:"kind"`
	Status int    `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type fetched struct {
	URLOutcome
	der        []byte
	retryAfter time.Duration
	date       time.Time // the server's Date header, advisory only
	// val is the validator the server offered for this body. It is stored for the next conditional request
	// only if the CRL is then accepted (or is the one already held): a refused CRL must not be hidden
	// behind a 304.
	val validator
}

type validator struct{ etag, lastModified string }

// fetchOne asks one configured URL. It enforces the size limit on the decoded body, the redirect rule and
// the content form, and never reads a header for freshness.
func (r *Refresher) fetchOne(ctx context.Context, raw string, maxBytes int) fetched {
	out := fetched{URLOutcome: URLOutcome{URL: audit.RedactURL(raw)}}
	origin, err := audit.CheckCRLURL(raw)
	if err != nil {
		out.Kind, out.Detail = outError, err.Error()
		return out
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		out.Kind, out.Detail = outError, "bad request"
		return out
	}
	req.Header.Set("User-Agent", crlUserAgent)
	req.Header.Set("Accept", "application/pkix-crl")
	r.mu.Lock()
	v := r.validators[raw]
	r.mu.Unlock()
	if v.etag != "" {
		req.Header.Set("If-None-Match", v.etag)
	}
	if v.lastModified != "" {
		req.Header.Set("If-Modified-Since", v.lastModified)
	}
	c := *r.client
	c.Jar = nil
	refused := ""
	c.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		refused = redirectRefusal(origin, next.URL, len(via))
		if refused == "" {
			return nil
		}
		return errors.New(refused)
	}
	resp, err := c.Do(req)
	if err != nil {
		switch {
		case refused != "":
			out.Kind, out.Detail = outRedirect, refused
		case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
			out.Kind, out.Detail = outTimeout, "no complete response in time"
		default:
			out.Kind, out.Detail = outError, redactError(err)
		}
		return out
	}
	defer resp.Body.Close()
	out.Status = resp.StatusCode
	if d, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		out.date = d
	}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		out.Kind = outNotModified
		return out
	case resp.StatusCode != http.StatusOK:
		out.Kind, out.Detail = outHTTP, resp.Status
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			out.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), r.now())
		}
		return out
	case resp.ContentLength > int64(maxBytes):
		out.Kind, out.Detail = outOversize, fmt.Sprintf("Content-Length %d is above the %d byte limit", resp.ContentLength, maxBytes)
		return out
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	switch {
	case err != nil && (errors.Is(err, context.DeadlineExceeded) || isTimeout(err)):
		out.Kind, out.Detail = outTimeout, "the body did not arrive in time"
		return out
	case err != nil:
		out.Kind, out.Detail = outError, redactError(err)
		return out
	case len(body) > maxBytes:
		out.Kind, out.Detail = outOversize, fmt.Sprintf("the body is above the %d byte limit", maxBytes)
		return out
	}
	if t := bytes.TrimSpace(body); bytes.HasPrefix(t, []byte("-----BEGIN")) {
		blk, rest := pem.Decode(t)
		if blk == nil || blk.Type != "X509 CRL" || len(bytes.TrimSpace(rest)) != 0 {
			out.Kind, out.Detail = outNotCRL, "the body is PEM but not one X509 CRL block"
			return out
		}
		body = blk.Bytes
	}
	out.Kind, out.der = outOK, body
	out.val = validator{etag: resp.Header.Get("ETag"), lastModified: resp.Header.Get("Last-Modified")}
	return out
}

// redirectRefusal is the redirect rule: at most three hops, only to the same host and port as the
// configured URL, never from https to another scheme. It returns why a redirect is refused, or "".
func redirectRefusal(origin, next *url.URL, hops int) string {
	switch {
	case hops > maxRedirects:
		return "more than 3 redirects"
	case hostPort(next) != hostPort(origin) && !isHTTPSUpgrade(origin, next):
		return "redirect to another host or port: " + hostPort(next)
	case strings.EqualFold(origin.Scheme, "https") && !strings.EqualFold(next.Scheme, "https"):
		return "redirect from https to " + next.Scheme
	}
	return ""
}

// isHTTPSUpgrade is an http URL on its default port redirected to the same host over https on its default
// port: the usual way a CRL server moves to TLS.
func isHTTPSUpgrade(origin, next *url.URL) bool {
	return strings.EqualFold(origin.Scheme, "http") && strings.EqualFold(next.Scheme, "https") &&
		strings.EqualFold(origin.Hostname(), next.Hostname()) &&
		(origin.Port() == "" || origin.Port() == "80") && (next.Port() == "" || next.Port() == "443")
}

func hostPort(u *url.URL) string {
	p := u.Port()
	if p == "" {
		p = map[string]string{"http": "80", "https": "443"}[strings.ToLower(u.Scheme)]
	}
	return strings.ToLower(u.Hostname()) + ":" + p
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// redactError removes any URL (and so any token in its query) from a transport error.
func redactError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + " " + audit.RedactURL(ue.URL) + ": " + ue.Err.Error()
	}
	return err.Error()
}

// parseRetryAfter reads delta-seconds or an HTTP date; anything else, or a value over a day, is zero.
func parseRetryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(h); err == nil && n > 0 {
		return min(time.Duration(n)*time.Second, maxRetryAfterRaw)
	}
	if t, err := http.ParseTime(h); err == nil && t.After(now) {
		return min(t.Sub(now), maxRetryAfterRaw)
	}
	return 0
}
