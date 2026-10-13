package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBase = "https://api.stripe.com"
	testKeyPref = "sk_test_"
)

// checkTestKey refuses anything but a test-mode secret key. This example never touches live
// mode. The error names the rule, never the key.
func checkTestKey(key string) error {
	if key == "" {
		return errors.New("STRIPE_SECRET_KEY is not set (use a test-mode secret key, sk_test_...)")
	}
	if !strings.HasPrefix(key, testKeyPref) {
		return errors.New("STRIPE_SECRET_KEY is not a test-mode secret key (it must start with sk_test_): this example never uses live mode")
	}
	return nil
}

// checkBase accepts Stripe's API, or a loopback address (STRIPE_API_BASE, for the fake server the
// tests run against). Any other host is refused, so a test key and a typo cannot send requests
// anywhere unexpected.
func checkBase(base string) error {
	if base == defaultBase {
		return nil
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" {
		return fmt.Errorf("STRIPE_API_BASE %q is not %s or an http loopback address", base, defaultBase)
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return nil
	}
	return fmt.Errorf("STRIPE_API_BASE %q is not %s or an http loopback address", base, defaultBase)
}

// stripe is the slice of Stripe's REST API this example uses, over net/http with no SDK.
type stripe struct {
	base string
	key  string // never printed or logged
	hc   *http.Client
}

func newStripe(base, key string) (*stripe, error) {
	if err := checkTestKey(key); err != nil {
		return nil, err
	}
	if base == "" {
		base = defaultBase
	}
	if err := checkBase(base); err != nil {
		return nil, err
	}
	return &stripe{base: base, key: key, hc: &http.Client{Timeout: 30 * time.Second}}, nil
}

// apiError is Stripe's error object, decoded from a non-2xx response.
type apiError struct {
	Status  int
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("stripe: %d %s: %s", e.Status, e.Type, e.Message)
}

// do sends one request and decodes a 2xx JSON body into out. A non-2xx response is an *apiError.
// A transport error (the response may never have arrived) is returned as is.
func (s *stripe) do(ctx context.Context, method, path string, form url.Values, idemKey string, out any) error {
	var body io.Reader
	target := s.base + path
	if method == http.MethodGet && len(form) > 0 {
		target += "?" + form.Encode()
	} else if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return redactKey(err, s.key)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var env struct {
			Error apiError `json:"error"`
		}
		_ = json.Unmarshal(b, &env)
		env.Error.Status = resp.StatusCode
		return &env.Error
	}
	return json.Unmarshal(b, out)
}

// redactKey keeps the key out of a transport error's text.
func redactKey(err error, key string) error {
	if key != "" && strings.Contains(err.Error(), key) {
		return errors.New(strings.ReplaceAll(err.Error(), key, "[redacted]"))
	}
	return err
}

// paymentIntent is the fields of a PaymentIntent the example reads.
type paymentIntent struct {
	ID             string `json:"id"`
	Amount         int64  `json:"amount"`
	AmountReceived int64  `json:"amount_received"`
	Currency       string `json:"currency"`
	Status         string `json:"status"`
	LatestCharge   string `json:"latest_charge"`
}

// charge is the fields of a Charge the example reads.
type charge struct {
	ID             string `json:"id"`
	AmountRefunded int64  `json:"amount_refunded"`
}

// refund is the fields of a Refund the example reads.
type refund struct {
	ID            string            `json:"id"`
	Amount        int64             `json:"amount"`
	Currency      string            `json:"currency"`
	Status        string            `json:"status"`
	PaymentIntent string            `json:"payment_intent"`
	Metadata      map[string]string `json:"metadata"`
}

func (s *stripe) paymentIntent(ctx context.Context, id string) (paymentIntent, error) {
	var pi paymentIntent
	err := s.do(ctx, http.MethodGet, "/v1/payment_intents/"+url.PathEscape(id), nil, "", &pi)
	return pi, err
}

func (s *stripe) charge(ctx context.Context, id string) (charge, error) {
	var c charge
	err := s.do(ctx, http.MethodGet, "/v1/charges/"+url.PathEscape(id), nil, "", &c)
	return c, err
}

// createRefund posts a refund. The Idempotency-Key makes a repeat of this exact request return
// the first response instead of refunding again (Stripe keeps keys for 24 hours). The metadata
// is what a reconciler later finds the refund by: Stripe cannot look a refund up by its key.
func (s *stripe) createRefund(ctx context.Context, pi string, amount int64, idemKey string, meta map[string]string) (refund, error) {
	form := url.Values{"payment_intent": {pi}, "amount": {strconv.FormatInt(amount, 10)}}
	for k, v := range meta {
		form.Set("metadata["+k+"]", v)
	}
	var r refund
	err := s.do(ctx, http.MethodPost, "/v1/refunds", form, idemKey, &r)
	return r, err
}

// listRefunds returns every refund of a payment intent, following pagination.
func (s *stripe) listRefunds(ctx context.Context, pi string) ([]refund, error) {
	var all []refund
	after := ""
	for {
		form := url.Values{"payment_intent": {pi}, "limit": {"100"}}
		if after != "" {
			form.Set("starting_after", after)
		}
		var page struct {
			Data    []refund `json:"data"`
			HasMore bool     `json:"has_more"`
		}
		if err := s.do(ctx, http.MethodGet, "/v1/refunds", form, "", &page); err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		if !page.HasMore || len(page.Data) == 0 {
			return all, nil
		}
		after = page.Data[len(page.Data)-1].ID
	}
}
