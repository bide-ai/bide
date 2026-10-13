// Package notify ships bide's webhook notifier: a signed HTTPS POST of a structured notification,
// and the receiver-side Verify that checks one.
//
// The package is stdlib only. A request carries a Bide-Delivery header (the notification ID, the
// dedupe key, identical on every attempt of an event), a Bide-Timestamp (Unix seconds at send time)
// and a Bide-Signature: "v1=" and the hex HMAC-SHA256 of timestamp + "." + body, once per secret.
// The signature is always on. Mutual TLS, a private CA and a key pin are optional and add to it;
// they never replace it.
//
// The payload is structured fields only. It never carries error text, tool arguments, results or
// model output unless the operator names them in WithFields, and it carries a view pointer, never
// a link that approves or resolves anything.
package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Defaults of a Webhook.
const (
	DefaultTimeout = 10 * time.Second // per request
	MaxRequests    = 3                // the most requests one Notify call may make (WithRequestsPerCall)
	maxRetryAfter  = 60 * time.Second // the longest Retry-After a round will wait out
)

// A Webhook delivers notifications as signed HTTPS POSTs. Build one with NewWebhook. It is safe
// for concurrent use.
type Webhook struct {
	url      *url.URL
	host     string // for error text: the URL's host only, since a webhook URL can itself be a secret
	secrets  []string
	timeout  time.Duration
	requests int // requests per Notify call; the notifier core retries across calls
	fields   fieldSet
	viewURL  string
	onError  func(error)
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	jitter   func(time.Duration) time.Duration
	client   *http.Client
	tlsCfg   *tls.Config
	cert     *clientCert
	pins     [][sha256.Size]byte
	customCA bool
}

// An Option configures NewWebhook.
type Option func(*Webhook) error

func configErr(format string, a ...any) error {
	return fmt.Errorf("notify: %w: %s", agent.ErrConfig, fmt.Sprintf(format, a...))
}

// WithSecrets sets the signing secrets. current always signs. previous, if not empty, signs too, so
// a receiver that has not yet been given the new secret (or has not yet dropped the old one)
// verifies either way: rotate by deploying previous=old, current=new, updating the receiver, then
// dropping previous. Secrets are never logged, journaled, put in a payload or put in an error.
func WithSecrets(current, previous string) Option {
	return func(w *Webhook) error {
		if current == "" {
			return configErr("WithSecrets needs a non-empty current secret")
		}
		w.secrets = []string{current}
		if previous != "" {
			w.secrets = append(w.secrets, previous)
		}
		return nil
	}
}

// WithTimeout sets the timeout of each request (default 10 seconds).
func WithTimeout(d time.Duration) Option {
	return func(w *Webhook) error {
		if d <= 0 {
			return configErr("WithTimeout needs a positive duration")
		}
		w.timeout = d
		return nil
	}
}

// WithRequestsPerCall sets how many requests one Notify call may make, with backoff of 1 s then
// 2 s between them (default 1, at most MaxRequests). The notifier core already runs several calls
// per delivery round, so the default leaves the retrying to it; raise this for a caller that uses
// the Webhook without the core. A Retry-After on a 429 or 5xx is honored either way: Notify waits
// it out (up to 60 s, and never past the context's deadline) before returning, so the core's next
// call does not arrive early.
func WithRequestsPerCall(n int) Option {
	return func(w *Webhook) error {
		if n < 1 || n > MaxRequests {
			return configErr("WithRequestsPerCall needs 1 to %d", MaxRequests)
		}
		w.requests = n
		return nil
	}
}

// WithClientCert presents a client certificate for mutual TLS, read from PEM files. The files are
// re-read when they change (their modification time is checked at most once a minute, and again
// after a TLS handshake failure), so a rotated or short-lived certificate is picked up without a
// restart. A read that fails keeps the last good pair and reports the error to the handler set by
// WithOnError.
func WithClientCert(certFile, keyFile string) Option {
	return func(w *Webhook) error {
		c, err := newClientCertFiles(certFile, keyFile, func() time.Time { return w.now() }, func(e error) { w.report(e) })
		if err != nil {
			return configErr("%v", err)
		}
		w.cert = c
		return nil
	}
}

// WithClientCertPEM is WithClientCert for a pair held in memory, such as one read from the
// environment. It is not reloaded.
func WithClientCertPEM(certPEM, keyPEM []byte) Option {
	return func(w *Webhook) error {
		c, err := newClientCertPEM(certPEM, keyPEM, func() time.Time { return w.now() }, func(e error) { w.report(e) })
		if err != nil {
			return configErr("%v", err)
		}
		w.cert = c
		return nil
	}
}

// WithServerCA verifies the receiver against the CA certificates in a PEM file instead of the
// system roots. It replaces them rather than adding to them, so a private receiver cannot be
// impersonated by a certificate from a public CA.
func WithServerCA(file string) Option {
	return func(w *Webhook) error {
		pem, err := os.ReadFile(file)
		if err != nil {
			return configErr("server CA: %v", unwrapPath(err))
		}
		return WithServerCAPEM(pem)(w)
	}
}

// WithServerCAPEM is WithServerCA for a bundle held in memory.
func WithServerCAPEM(pem []byte) Option {
	return func(w *Webhook) error {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return configErr("server CA: no certificate found in the PEM")
		}
		w.tlsCfg.RootCAs = pool
		w.customCA = true
		return nil
	}
}

// WithServerPin pins the receiver's key: after normal certificate verification, the handshake
// fails unless a certificate in the verified chain has a SubjectPublicKeyInfo whose SHA-256 is one
// of the pins (64 hex characters or base64; see SPKIHash). Give two while the receiver rotates its
// key. Pinning is in addition to normal verification, never instead of it.
func WithServerPin(pins ...string) Option {
	return func(w *Webhook) error {
		if len(pins) == 0 || len(pins) > 2 {
			return configErr("WithServerPin takes one or two pins")
		}
		w.pins = w.pins[:0]
		for _, p := range pins {
			h, err := parsePin(p)
			if err != nil {
				return configErr("%v", err)
			}
			w.pins = append(w.pins, h)
		}
		return nil
	}
}

// WithFields allows payload fields that are left out by default. See Field, ErrorText and Args.
func WithFields(fs ...Field) Option {
	return func(w *Webhook) error {
		for _, f := range fs {
			f.apply(&w.fields)
		}
		return nil
	}
}

// WithViewURL sets the base URL of the deployment's run viewer; the payload's view.url is then
// base + "/" + run ID, escaped. It is a view pointer: no approve or resolve link is ever built.
func WithViewURL(base string) Option {
	return func(w *Webhook) error {
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) || u.Host == "" {
			return configErr("WithViewURL needs an https URL")
		}
		w.viewURL = base
		return nil
	}
}

// WithOnError sets where non-fatal problems are reported, such as a client certificate that failed
// to reload (the last good pair stays in use). Pass the function given to agent.WithRecoverErrors.
func WithOnError(f func(error)) Option {
	return func(w *Webhook) error { w.onError = f; return nil }
}

func (w *Webhook) report(err error) {
	if w.onError != nil {
		w.onError(err)
	}
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// NewWebhook returns a Webhook that posts to rawURL. The URL must be https, except for localhost,
// 127.0.0.1 and [::1], which exist for development and tests; anything else is agent.ErrConfig.
// WithSecrets is required: an unsigned webhook is not offered.
func NewWebhook(rawURL string, opts ...Option) (*Webhook, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, configErr("the webhook URL is not a valid absolute URL")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return nil, configErr("the webhook URL must be https (http is allowed only for localhost, 127.0.0.1 and [::1])")
	}
	w := &Webhook{
		url:      u,
		host:     u.Host,
		timeout:  DefaultTimeout,
		requests: 1,
		now:      time.Now,
		sleep:    sleepCtx,
		jitter:   func(d time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(d)/4 + 1)) },
		tlsCfg:   &tls.Config{MinVersion: tls.VersionTLS12},
	}
	for _, o := range opts {
		if err := o(w); err != nil {
			return nil, err
		}
	}
	if len(w.secrets) == 0 {
		return nil, configErr("WithSecrets is required")
	}
	if u.Scheme == "http" && (w.cert != nil || w.customCA || len(w.pins) > 0) {
		return nil, configErr("a client certificate, server CA or pin needs an https URL")
	}
	if w.cert != nil {
		w.tlsCfg.GetClientCertificate = w.cert.get
	}
	if len(w.pins) > 0 {
		w.tlsCfg.VerifyConnection = pinVerifier(w.pins)
	}
	w.client = &http.Client{
		// A signed body must not go somewhere the operator did not name.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: w.tlsCfg,
			// One handshake per delivery: deliveries are rare, a rotated client certificate is
			// then used by the next one, and the audit record's "client certificate presented"
			// is true of this request rather than of some earlier connection.
			DisableKeepAlives:     true,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   w.timeout,
			ResponseHeaderTimeout: w.timeout,
		},
	}
	return w, nil
}

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

// Notify delivers n as one signed POST, retrying inside the round (see MaxRequests). It returns
// nil only once the receiver answered 2xx, and reports how the delivery was authenticated through
// agent.ReportNotifyAck, for the audit record of the acknowledgement. A refusal that retrying in
// the round cannot fix (a 4xx other than 429, a redirect) is a *PermanentError, which wraps
// agent.ErrNotifyPermanent.
func (w *Webhook) Notify(ctx context.Context, n agent.Notification) error {
	body, err := json.Marshal(w.payload(n))
	if err != nil {
		return fmt.Errorf("notify: encoding the payload: %w", err)
	}
	res, err := w.deliver(ctx, n.ID, body)
	if err != nil {
		return err
	}
	agent.ReportNotifyAck(ctx, agent.NotifyAck{Status: res.Status, Auth: res.Auth})
	return nil
}

var _ agent.Notifier = (*Webhook)(nil)

type deliveryResult struct {
	Status      int
	FirstSentAt time.Time
	Requests    int
	Auth        *agent.NotifyAuth
}

// PermanentError is returned for a response that retrying inside the round cannot fix: any 4xx
// other than 429, or a redirect. The caller's outer backoff still tries again later, since a
// receiver's configuration can change.
type PermanentError struct{ Status int }

func (e *PermanentError) Error() string {
	return "notify: receiver refused the delivery: HTTP " + strconv.Itoa(e.Status)
}

// Unwrap makes errors.Is(err, agent.ErrNotifyPermanent) true.
func (e *PermanentError) Unwrap() error { return agent.ErrNotifyPermanent }

// deliver sends body, retrying within the round.
func (w *Webhook) deliver(ctx context.Context, id string, body []byte) (deliveryResult, error) {
	var res deliveryResult
	var lastErr error
	delay := time.Second
	for attempt := 1; attempt <= w.requests; attempt++ {
		sent := w.now()
		if res.FirstSentAt.IsZero() {
			res.FirstSentAt = sent
		}
		res.Requests = attempt
		status, retryAfter, auth, err := w.once(ctx, id, body, sent)
		if err == nil && status/100 == 2 {
			res.Status, res.Auth = status, auth
			return res, nil
		}
		res.Status = status
		switch {
		case err != nil:
			lastErr = err
			if ctx.Err() != nil {
				return res, lastErr
			}
		case status == http.StatusTooManyRequests || status >= 500:
			lastErr = fmt.Errorf("notify: receiver answered HTTP %d", status)
		default:
			return res, &PermanentError{Status: status}
		}
		last := attempt == w.requests
		wait := delay + w.jitter(delay)
		if retryAfter > 0 {
			wait = min(retryAfter, maxRetryAfter)
		} else if last {
			break
		}
		if dl, ok := ctx.Deadline(); ok && w.now().Add(wait).After(dl) {
			break // the wait would outlast the caller's deadline: report now
		}
		if serr := w.sleep(ctx, wait); serr != nil {
			return res, serr
		}
		if last {
			break
		}
		if err != nil && w.cert != nil {
			// The backoff is when an operator's rotation lands: look again before retrying.
			w.cert.reloadNow()
		}
		delay *= 2
	}
	return res, lastErr
}

func (w *Webhook) once(ctx context.Context, id string, body []byte, sent time.Time) (status int, retryAfter time.Duration, auth *agent.NotifyAuth, err error) {
	rctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	rctx, pres := withPresented(rctx)
	ts := sent.Unix()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, w.url.String(), bytes.NewReader(body))
	if err != nil {
		return 0, 0, nil, configErr("building the request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderDelivery, id)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, Sign(w.secrets, ts, body))
	resp, err := w.client.Do(req)
	if err != nil {
		if w.cert != nil && ctx.Err() == nil {
			// A handshake failure may be a rotated certificate the receiver now requires.
			w.cert.reloadNow()
		}
		return 0, 0, nil, w.sanitize(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), w.now())
	}
	if resp.StatusCode/100 == 2 {
		auth = w.authFor(resp, pres)
	}
	return resp.StatusCode, retryAfter, auth, nil
}

func (w *Webhook) authFor(resp *http.Response, pres *presented) *agent.NotifyAuth {
	a := &agent.NotifyAuth{Signature: "hmac-sha256", SecretSlot: "current"}
	if len(w.secrets) > 1 {
		a.SecretSlot = "current+previous"
	}
	if ok, spki := pres.get(); ok {
		a.MTLS, a.ClientSPKI = true, spki
	}
	a.ServerPinned = len(w.pins) > 0 && resp.TLS != nil
	a.CustomCA = w.customCA && resp.TLS != nil
	if resp.TLS != nil {
		a.TLSVersion = tlsVersionName(resp.TLS.Version)
	}
	return a
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "1.3"
	case tls.VersionTLS12:
		return "1.2"
	case tls.VersionTLS11:
		return "1.1"
	case tls.VersionTLS10:
		return "1.0"
	}
	return "0x" + strconv.FormatUint(uint64(v), 16)
}

// sanitize drops the URL from a transport error: a webhook URL can itself be a credential (Slack's
// is), and an error string ends up in logs and the trail's failure path.
func (w *Webhook) sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("notify: POST to %s failed: %w", w.host, err)
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
