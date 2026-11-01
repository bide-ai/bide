//go:build ignore

// fakestripe.go: a minimal local stand-in for the slice of Stripe's API that examples/stripe-refund
// uses, for rehearsing assets/demo/stripe-promo-record.sh without a key. It is not used for the
// final recording. Run: go run fakestripe.go -addr 127.0.0.1:18599 -key sk_test_rehearsal
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18599", "listen address")
	key := flag.String("key", "sk_test_rehearsal", "accepted key")
	flag.Parse()
	var mu sync.Mutex
	var refunds []map[string]any
	idem := map[string][]byte{}
	out := func(w http.ResponseWriter, code int, v any) []byte {
		b, _ := json.MarshalIndent(v, "", "  ")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		w.Write(b)
		return b
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = r.ParseForm()
		u, _, basic := r.BasicAuth()
		if r.Header.Get("Authorization") != "Bearer "+*key && !(basic && u == *key) {
			out(w, 401, map[string]any{"error": map[string]string{"type": "invalid_request_error", "message": "Invalid API Key provided"}})
			return
		}
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/v1/payment_intents/"):
			id := strings.TrimPrefix(p, "/v1/payment_intents/")
			out(w, 200, map[string]any{"id": id, "amount": 5000, "amount_received": 5000, "currency": "usd", "status": "succeeded", "latest_charge": "ch_" + id})
		case strings.HasPrefix(p, "/v1/charges/"):
			var n int64
			for _, x := range refunds {
				n += x["amount"].(int64)
			}
			out(w, 200, map[string]any{"id": strings.TrimPrefix(p, "/v1/charges/"), "amount_refunded": n})
		case p == "/v1/refunds" && r.Method == "POST":
			if b, ok := idem[r.Header.Get("Idempotency-Key")]; ok {
				w.Header().Set("Content-Type", "application/json")
				w.Write(b)
				return
			}
			amt, _ := strconv.ParseInt(r.Form.Get("amount"), 10, 64)
			meta := map[string]string{}
			for k, v := range r.Form {
				if strings.HasPrefix(k, "metadata[") {
					meta[strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")] = v[0]
				}
			}
			rf := map[string]any{"id": fmt.Sprintf("re_rehearsal_%d", len(refunds)+1), "object": "refund", "amount": amt, "currency": "usd",
				"status": "succeeded", "payment_intent": r.Form.Get("payment_intent"), "metadata": meta}
			refunds = append(refunds, rf)
			b := out(w, 200, rf)
			idem[r.Header.Get("Idempotency-Key")] = b
		case p == "/v1/refunds":
			var data []map[string]any
			for _, x := range refunds {
				if x["payment_intent"] == r.Form.Get("payment_intent") {
					data = append(data, x)
				}
			}
			out(w, 200, map[string]any{"object": "list", "data": data, "has_more": false})
		default:
			out(w, 404, map[string]any{"error": map[string]string{"type": "invalid_request_error", "message": "Unrecognized request URL"}})
		}
	})
	fmt.Println("fakestripe on http://" + *addr)
	http.ListenAndServe(*addr, nil)
}
