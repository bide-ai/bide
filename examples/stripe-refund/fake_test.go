package main

// A fake of the slice of Stripe's API the example uses, so the tests need no key and no network.
// It checks the bearer token, creates refunds, lists them, and honors Idempotency-Key the way
// Stripe does: the same key with the same parameters returns the first response without creating
// a second refund, and the same key with different parameters is an error.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type fakePI struct {
	ID       string
	Status   string
	Received int64
	Currency string
}

type fakeReq struct {
	Method, Path, Idem string
	Form               map[string]string
}

type idemEntry struct {
	params string
	status int
	body   []byte
}

type fakeStripe struct {
	t        *testing.T
	srv      *httptest.Server
	key      string
	pageSize int

	mu       sync.Mutex
	pis      map[string]fakePI
	refunds  []refund
	idem     map[string]idemEntry
	requests []fakeReq
	badAuth  int
}

func newFakeStripe(t *testing.T, key string, pis ...fakePI) *fakeStripe {
	f := &fakeStripe{t: t, key: key, pageSize: 100, pis: map[string]fakePI{}, idem: map[string]idemEntry{}}
	for _, p := range pis {
		f.pis[p.ID] = p
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// seed adds a refund Stripe already holds, and page sets the most refunds a list returns at once.
func (f *fakeStripe) seed(r refund) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refunds = append(f.refunds, r)
}

func (f *fakeStripe) page(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pageSize = n
}

func (f *fakeStripe) URL() string { return f.srv.URL }

func (f *fakeStripe) refundCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.refunds)
}

// posts returns the refund-creating requests the fake received.
func (f *fakeStripe) posts() []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeReq
	for _, r := range f.requests {
		if r.Method == http.MethodPost && r.Path == "/v1/refunds" {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeStripe) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeStripe) refundedOn(pi string) int64 {
	var n int64
	for _, r := range f.refunds {
		if r.PaymentIntent == pi {
			n += r.Amount
		}
	}
	return n
}

func reply(w http.ResponseWriter, status int, v any) []byte {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
	return b
}

func apiErr(w http.ResponseWriter, status int, typ, msg string) {
	reply(w, status, map[string]any{"error": map[string]string{"type": typ, "message": msg}})
}

func (f *fakeStripe) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	form := map[string]string{}
	for k, v := range r.Form {
		form[k] = v[0]
	}
	f.requests = append(f.requests, fakeReq{Method: r.Method, Path: r.URL.Path, Idem: r.Header.Get("Idempotency-Key"), Form: form})
	// Stripe takes the key as a bearer token or as the user of HTTP basic auth (curl -u key:).
	if user, _, basic := r.BasicAuth(); r.Header.Get("Authorization") != "Bearer "+f.key && !(basic && user == f.key) {
		f.badAuth++
		apiErr(w, 401, "invalid_request_error", "Invalid API Key provided")
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/payment_intents/"):
		p, ok := f.pis[strings.TrimPrefix(r.URL.Path, "/v1/payment_intents/")]
		if !ok {
			apiErr(w, 404, "invalid_request_error", "No such payment_intent")
			return
		}
		reply(w, 200, map[string]any{"id": p.ID, "amount": p.Received, "amount_received": p.Received, "currency": p.Currency,
			"status": p.Status, "latest_charge": "ch_" + p.ID})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/charges/ch_"):
		pi := strings.TrimPrefix(r.URL.Path, "/v1/charges/ch_")
		reply(w, 200, map[string]any{"id": "ch_" + pi, "amount_refunded": f.refundedOn(pi)})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/refunds":
		f.createRefund(w, r, form)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/refunds":
		f.listRefunds(w, form)
	default:
		apiErr(w, 404, "invalid_request_error", "Unrecognized request URL")
	}
}

func (f *fakeStripe) createRefund(w http.ResponseWriter, r *http.Request, form map[string]string) {
	params := fmt.Sprint(sortedForm(form))
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		if e, ok := f.idem[key]; ok {
			if e.params != params {
				apiErr(w, 400, "idempotency_error", "Keys for idempotent requests can only be used with the same parameters they were first used with.")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(e.status)
			w.Write(e.body)
			return
		}
	}
	p, ok := f.pis[form["payment_intent"]]
	amount, err := strconv.ParseInt(form["amount"], 10, 64)
	if !ok || err != nil || amount <= 0 || amount > p.Received-f.refundedOn(p.ID) {
		apiErr(w, 400, "invalid_request_error", "Refund amount is invalid or exceeds the charge")
		return
	}
	meta := map[string]string{}
	for k, v := range form {
		if strings.HasPrefix(k, "metadata[") {
			meta[strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")] = v
		}
	}
	rf := refund{ID: fmt.Sprintf("re_fake_%d", len(f.refunds)+1), Amount: amount, Currency: p.Currency, Status: "succeeded", PaymentIntent: p.ID, Metadata: meta}
	f.refunds = append(f.refunds, rf)
	b := reply(w, 200, rf)
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		f.idem[key] = idemEntry{params: params, status: 200, body: b}
	}
}

func (f *fakeStripe) listRefunds(w http.ResponseWriter, form map[string]string) {
	limit := f.pageSize
	if n, err := strconv.Atoi(form["limit"]); err == nil && n > 0 && n < limit {
		limit = n
	}
	var match []refund
	for _, r := range f.refunds {
		if r.PaymentIntent == form["payment_intent"] {
			match = append(match, r)
		}
	}
	start := 0
	if after := form["starting_after"]; after != "" {
		for i, r := range match {
			if r.ID == after {
				start = i + 1
			}
		}
	}
	page := match[start:]
	more := len(page) > limit
	if more {
		page = page[:limit]
	}
	reply(w, 200, map[string]any{"object": "list", "data": page, "has_more": more})
}

func sortedForm(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
