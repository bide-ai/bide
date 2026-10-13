package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

func TestNewWebhookHTTPSOnly(t *testing.T) {
	ok := []string{
		"https://example.com/hook",
		"http://localhost/hook",
		"http://localhost:8080/hook",
		"http://127.0.0.1:9/hook",
		"http://[::1]:9/hook",
	}
	for _, u := range ok {
		if _, err := NewWebhook(u, WithSecrets("s", "")); err != nil {
			t.Errorf("%s refused: %v", u, err)
		}
	}
	bad := []string{
		"http://example.com/hook",
		"http://localhost.evil.com/hook",
		"http://127.0.0.1.evil.com/hook",
		"http://evil.com/?h=localhost",
		"http://10.0.0.1/hook",
		"http://0.0.0.0/hook",
		"ftp://localhost/hook",
		"//localhost/hook",
		"localhost/hook",
		"",
	}
	for _, u := range bad {
		_, err := NewWebhook(u, WithSecrets("s", ""))
		if !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%q: got %v, want ErrConfig", u, err)
		}
	}
}

func TestNewWebhookConfigErrors(t *testing.T) {
	if _, err := NewWebhook("https://example.com/"); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("no secret: %v", err)
	}
	if _, err := NewWebhook("https://example.com/", WithSecrets("", "old")); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("empty current: %v", err)
	}
	if _, err := NewWebhook("https://example.com/", WithSecrets("s", ""), WithTimeout(0)); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("zero timeout: %v", err)
	}
	if _, err := NewWebhook("https://example.com/", WithSecrets("s", ""), WithServerCA("/nonexistent/ca.pem")); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("missing CA file: %v", err)
	}
	if _, err := NewWebhook("https://example.com/", WithSecrets("s", ""), WithServerCAPEM([]byte("not pem"))); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("bad CA pem: %v", err)
	}
	if _, err := NewWebhook("https://example.com/", WithSecrets("s", ""), WithClientCert("/nonexistent/c", "/nonexistent/k")); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("missing client cert: %v", err)
	}
	for _, pins := range [][]string{nil, {"zz"}, {strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}} {
		if _, err := NewWebhook("https://example.com/", WithSecrets("s", ""), WithServerPin(pins...)); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("pins %v: %v", pins, err)
		}
	}
	if _, err := NewWebhook("http://localhost/", WithSecrets("s", ""), WithServerPin(strings.Repeat("a", 64))); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("pin on http: %v", err)
	}
	if _, err := NewWebhook("https://example.com/", WithSecrets("s", ""), WithViewURL("javascript:alert(1)")); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("view url: %v", err)
	}
}

// newPlainServer is an http://127.0.0.1 server, allowed for development.
func newPlainServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func TestRequestShapeAndSignature(t *testing.T) {
	var gotHdr http.Header
	var gotBody []byte
	srv := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotHdr = r.Header.Clone()
		b := make([]byte, 1<<16)
		n, _ := r.Body.Read(b)
		gotBody = b[:n]
		if r.Method != "POST" {
			t.Errorf("method %s", r.Method)
		}
	})
	w, _ := fastWebhook(t, srv.URL+"/hook")
	n := sampleApproval()
	before := time.Now().Unix()
	if err := w.Notify(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if ct := gotHdr.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q", ct)
	}
	if gotHdr.Get(HeaderDelivery) != n.ID {
		t.Errorf("delivery %q want %q", gotHdr.Get(HeaderDelivery), n.ID)
	}
	ts, err := strconv.ParseInt(gotHdr.Get(HeaderTimestamp), 10, 64)
	if err != nil || ts < before-1 || ts > time.Now().Unix()+1 {
		t.Errorf("timestamp %q", gotHdr.Get(HeaderTimestamp))
	}
	want := "v1=" + hexMAC(testSecret, gotHdr.Get(HeaderTimestamp)+"."+string(gotBody))
	if gotHdr.Get(HeaderSignature) != want {
		t.Errorf("signature %q, want %q (HMAC over timestamp.body)", gotHdr.Get(HeaderSignature), want)
	}
	// And the receiver helper agrees, over a rebuilt request.
	r := httptest.NewRequest("POST", "/hook", strings.NewReader(string(gotBody)))
	r.Header = gotHdr
	if err := Verify(r, []string{testSecret}, 0); err != nil {
		t.Fatal(err)
	}
	// The secret is nowhere in the request.
	if strings.Contains(string(gotBody), testSecret) {
		t.Fatal("secret in body")
	}
	for k, v := range gotHdr {
		if strings.Contains(strings.Join(v, ","), testSecret) {
			t.Fatalf("secret in header %s", k)
		}
	}
}

func TestSignsWithBothSecretsDuringRotation(t *testing.T) {
	var hdr http.Header
	var body []byte
	srv := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		b := make([]byte, 1<<16)
		n, _ := r.Body.Read(b)
		body = b[:n]
	})
	w, err := NewWebhook(srv.URL, WithSecrets("new-secret", "old-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Notify(context.Background(), sampleApproval()); err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(hdr.Get(HeaderSignature))); n != 2 {
		t.Fatalf("want two signatures, header %q", hdr.Get(HeaderSignature))
	}
	for _, rx := range [][]string{{"new-secret"}, {"old-secret"}} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(string(body)))
		r.Header = hdr
		if err := Verify(r, rx, 0); err != nil {
			t.Errorf("receiver with %v: %v", rx, err)
		}
	}
}

func TestRetriesAndBackoff(t *testing.T) {
	var mu sync.Mutex
	var stamps []string
	var codes []int
	srv := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		stamps = append(stamps, r.Header.Get(HeaderTimestamp))
		c := codes[len(stamps)-1]
		w.WriteHeader(c)
	})
	t.Run("recovers on the third request", func(t *testing.T) {
		stamps, codes = nil, []int{500, 503, 200}
		w, sleeps := fastWebhook(t, srv.URL, WithRequestsPerCall(3))
		var tick int64
		w.now = func() time.Time { tick += 10; return time.Unix(1_800_000_000+tick, 0) }
		if err := w.Notify(context.Background(), sampleApproval()); err != nil {
			t.Fatal(err)
		}
		if len(stamps) != 3 {
			t.Fatalf("requests: %d", len(stamps))
		}
		if len(*sleeps) != 2 || (*sleeps)[0] != time.Second || (*sleeps)[1] != 2*time.Second {
			t.Fatalf("backoff %v, want 1s then 2s", *sleeps)
		}
		if stamps[0] == stamps[1] || stamps[1] == stamps[2] {
			t.Fatalf("each request carries its own send time, got %v", stamps)
		}
	})
	t.Run("gives up after three", func(t *testing.T) {
		stamps, codes = nil, []int{500, 500, 500, 200}
		w, _ := fastWebhook(t, srv.URL, WithRequestsPerCall(3))
		err := w.Notify(context.Background(), sampleApproval())
		if err == nil || len(stamps) != 3 {
			t.Fatalf("err %v after %d requests, want an error after exactly 3", err, len(stamps))
		}
		if errors.Is(err, agent.ErrNotifyPermanent) {
			t.Fatalf("a 5xx is retried in a later round, not permanent: %v", err)
		}
	})
	t.Run("4xx is permanent for the round", func(t *testing.T) {
		for _, code := range []int{400, 401, 403, 404, 410} {
			stamps, codes = nil, []int{code, 200}
			w, sleeps := fastWebhook(t, srv.URL, WithRequestsPerCall(3))
			err := w.Notify(context.Background(), sampleApproval())
			var pe *PermanentError
			if !errors.As(err, &pe) || pe.Status != code || !errors.Is(err, agent.ErrNotifyPermanent) {
				t.Fatalf("%d: %v", code, err)
			}
			if len(stamps) != 1 || len(*sleeps) != 0 {
				t.Fatalf("%d: %d requests, sleeps %v", code, len(stamps), *sleeps)
			}
		}
	})
	t.Run("429 honors Retry-After", func(t *testing.T) {
		stamps = nil
		var n int
		srv2 := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) {
			n++
			if n == 1 {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(429)
			}
		})
		w, sleeps := fastWebhook(t, srv2.URL, WithRequestsPerCall(3))
		if err := w.Notify(context.Background(), sampleApproval()); err != nil {
			t.Fatal(err)
		}
		if n != 2 || len(*sleeps) != 1 || (*sleeps)[0] != 7*time.Second {
			t.Fatalf("requests %d sleeps %v, want one 7s wait", n, *sleeps)
		}
	})
	t.Run("Retry-After is capped and 429 without it uses the backoff", func(t *testing.T) {
		var n int
		srv2 := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) {
			n++
			switch n {
			case 1:
				w.Header().Set("Retry-After", "86400")
				w.WriteHeader(429)
			case 2:
				w.WriteHeader(429)
			}
		})
		w, sleeps := fastWebhook(t, srv2.URL, WithRequestsPerCall(3))
		if err := w.Notify(context.Background(), sampleApproval()); err != nil {
			t.Fatal(err)
		}
		if len(*sleeps) != 2 || (*sleeps)[0] != maxRetryAfter || (*sleeps)[1] != 2*time.Second {
			t.Fatalf("sleeps %v", *sleeps)
		}
	})
	t.Run("default is one request per call, Retry-After still waited out", func(t *testing.T) {
		var n int
		srv2 := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) {
			n++
			w.Header().Set("Retry-After", "9")
			w.WriteHeader(429)
		})
		w, sleeps := fastWebhook(t, srv2.URL)
		err := w.Notify(context.Background(), sampleApproval())
		if err == nil || n != 1 || len(*sleeps) != 1 || (*sleeps)[0] != 9*time.Second {
			t.Fatalf("err %v, requests %d, sleeps %v", err, n, *sleeps)
		}
		// Without Retry-After a failed single request returns at once.
		n2 := 0
		srv3 := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) { n2++; w.WriteHeader(500) })
		w3, sleeps3 := fastWebhook(t, srv3.URL)
		if err := w3.Notify(context.Background(), sampleApproval()); err == nil || n2 != 1 || len(*sleeps3) != 0 {
			t.Fatalf("err %v requests %d sleeps %v", err, n2, *sleeps3)
		}
	})
	t.Run("a Retry-After past the context deadline is not waited for", func(t *testing.T) {
		srv2 := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
		})
		w, sleeps := fastWebhook(t, srv2.URL, WithRequestsPerCall(3))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.Notify(ctx, sampleApproval()); err == nil || len(*sleeps) != 0 {
			t.Fatalf("err %v sleeps %v", err, *sleeps)
		}
	})
	t.Run("an HTTP-date Retry-After", func(t *testing.T) {
		now := time.Unix(1_800_000_000, 0)
		if d := parseRetryAfter(now.Add(20*time.Second).UTC().Format(http.TimeFormat), now); d != 20*time.Second {
			t.Fatalf("got %v", d)
		}
		if d := parseRetryAfter("-5", now); d != 0 {
			t.Fatalf("got %v", d)
		}
		if d := parseRetryAfter("soon", now); d != 0 {
			t.Fatalf("got %v", d)
		}
	})
}

func TestTimeoutIsRetried(t *testing.T) {
	var mu sync.Mutex
	var n int
	srv := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}
	})
	w, _ := fastWebhook(t, srv.URL, WithTimeout(100*time.Millisecond), WithRequestsPerCall(2))
	if err := w.Notify(context.Background(), sampleApproval()); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("requests %d", n)
	}
}

func TestCancelledDuringBackoff(t *testing.T) {
	srv := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	w, _ := fastWebhook(t, srv.URL, WithRequestsPerCall(3))
	ctx, cancel := context.WithCancel(context.Background())
	w.sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	err := w.Notify(ctx, sampleApproval())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestNoRedirects(t *testing.T) {
	var targetHits int
	target := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) { targetHits++ })
	for _, code := range []int{301, 302, 303, 307, 308} {
		var hits int
		src := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) {
			hits++
			http.Redirect(w, r, target.URL+"/elsewhere", code)
		})
		w, _ := fastWebhook(t, src.URL)
		err := w.Notify(context.Background(), sampleApproval())
		var pe *PermanentError
		if !errors.As(err, &pe) || pe.Status != code {
			t.Fatalf("%d: got %v, want a permanent error", code, err)
		}
		if hits != 1 {
			t.Fatalf("%d: %d requests to the named URL", code, hits)
		}
	}
	if targetHits != 0 {
		t.Fatalf("the signed body was sent to a redirect target %d times", targetHits)
	}
}

func TestErrorsLeakNeitherURLNorSecret(t *testing.T) {
	const token = "T0KEN-in-the-webhook-path"
	// A refused connection.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL + "/services/" + token
	dead.Close()
	w, _ := fastWebhook(t, url)
	err := w.Notify(context.Background(), sampleApproval())
	if err == nil {
		t.Fatal("want an error")
	}
	// And a 500.
	srv := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	w2, _ := fastWebhook(t, srv.URL+"/services/"+token)
	err2 := w2.Notify(context.Background(), sampleApproval())
	for _, e := range []error{err, err2} {
		if s := e.Error(); strings.Contains(s, token) || strings.Contains(s, testSecret) || strings.Contains(s, "/services/") {
			t.Fatalf("error leaks the URL path or the secret: %q", s)
		}
	}
}

func TestConcurrentNotify(t *testing.T) {
	var mu sync.Mutex
	var n int
	srv := newPlainServer(t, func(w http.ResponseWriter, r *http.Request) { mu.Lock(); n++; mu.Unlock() })
	w, _ := fastWebhook(t, srv.URL)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.Notify(context.Background(), sampleApproval()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n != 16 {
		t.Fatalf("%d requests", n)
	}
}

// ---- payload ----

func notifyJSON(t *testing.T, w *Webhook, n agent.Notification) (map[string]any, string) {
	t.Helper()
	b, err := json.Marshal(w.payload(n))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m, string(b)
}

func plainWebhook(t *testing.T, opts ...Option) *Webhook {
	t.Helper()
	w, err := NewWebhook("https://example.com/hook", append([]Option{WithSecrets(testSecret, "")}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

const (
	cardNumber = "4111111111111111"
	argsJSON   = `{"card":"` + cardNumber + `","amount":12500}`
)

func approvalWithSecrets() agent.Notification {
	n := sampleApproval()
	n.Err = &agent.ApprovalPending{ToolUseID: "call_01", ToolName: "refund", Args: json.RawMessage(argsJSON)}
	return n
}

func TestPayloadShape(t *testing.T) {
	w := plainWebhook(t)
	m, _ := notifyJSON(t, w, approvalWithSecrets())
	if m["schema"] != "bide.notification.v1" || m["kind"] != "approval_pending" || m["run_id"] != "orders-1042" ||
		m["id"] != agent.NotifyID("approval:orders-1042:call_01:0") || m["key"] != "approval:orders-1042:call_01:0" ||
		m["event_at"] != "2026-10-13T09:14:05Z" || m["observed_at"] != "2026-10-13T09:14:07Z" || m["deadline_at"] != "2026-10-13T09:19:07Z" ||
		m["attempt"] != float64(1) {
		t.Fatalf("envelope: %v", m)
	}
	if m["summary"] != "Run orders-1042 is waiting for approval of tool refund (call call_01)." {
		t.Fatalf("summary %q", m["summary"])
	}
	ap := m["approval"].(map[string]any)
	pol := ap["policy"].(map[string]any)
	if pol["need"] != float64(2) || pol["of"] != float64(3) || len(ap["approvers"].([]any)) != 3 || ap["tool"] != "refund" {
		t.Fatalf("approval: %v", ap)
	}
	if pr := m["principal"].(map[string]any); pr["on_behalf_of"] != "desk-7" {
		t.Fatalf("principal: %v", pr)
	}
	if v := m["view"].(map[string]any); v["command"] != "bide view -run orders-1042" || v["url"] != nil {
		t.Fatalf("view: %v", v)
	}
	if m["halt"] != nil || m["abort"] != nil || m["failing"] != nil {
		t.Fatalf("other details must be null: %v", m)
	}
	// No principal: null, not an empty object.
	n := approvalWithSecrets()
	n.Principal = nil
	if m, _ := notifyJSON(t, w, n); m["principal"] != nil {
		t.Fatalf("principal %v", m["principal"])
	}
}

func TestPayloadHaltAndOthers(t *testing.T) {
	w := plainWebhook(t)
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	m, _ := notifyJSON(t, w, agent.Notification{Kind: agent.NotificationOutcomeUnknown, RunID: "r1", Halt: &agent.NotifyHalt{
		Op: agent.OpRef{Kind: agent.OpTool, ID: "call_9", ToolName: "charge"}, Cause: agent.HaltCrashed, AttemptedAt: at}})
	h := m["halt"].(map[string]any)
	st := m["step"].(map[string]any)
	if h["cause"] != "crashed" || h["attempted_at"] != "2026-10-13T09:00:00Z" || st["op_kind"] != "tool" || st["op_id"] != "call_9" || st["tool"] != "charge" {
		t.Fatalf("halt: %v %v", h, st)
	}
	m, _ = notifyJSON(t, w, agent.Notification{Kind: agent.NotificationRunAborted, RunID: "r1", Reason: "budget"})
	if m["abort"].(map[string]any)["reason"] != "budget" {
		t.Fatalf("abort: %v", m)
	}
	m, _ = notifyJSON(t, w, agent.Notification{Kind: agent.NotificationRecoveryFailing, RunID: "r1", Reason: "store_error"})
	if m["failing"].(map[string]any)["cause"] != "store_error" {
		t.Fatalf("failing: %v", m)
	}
	m, _ = notifyJSON(t, w, agent.Notification{Kind: agent.NotificationOverdue, RunID: "r1", Reason: "deadline"})
	if m["reason"] != "deadline" {
		t.Fatalf("overdue: %v", m)
	}
}

func TestPayloadIsMinimalByDefault(t *testing.T) {
	w := plainWebhook(t)
	n := approvalWithSecrets()
	n.Err = &withText{n.Err, "stripe said: card " + cardNumber + " declined"}
	m, body := notifyJSON(t, w, n)
	if strings.Contains(body, cardNumber) || strings.Contains(body, "declined") || strings.Contains(body, "amount") {
		t.Fatalf("payload carries error text or arguments: %s", body)
	}
	if _, ok := m["error"]; ok {
		t.Fatal("error key present without ErrorText")
	}
	if _, ok := m["approval"].(map[string]any)["args"]; ok {
		t.Fatal("args present without Args")
	}
	// Exactly the documented top-level fields: a new one must be a deliberate, reviewed change.
	want := map[string]bool{"schema": true, "id": true, "key": true, "kind": true, "run_id": true, "event_at": true, "observed_at": true,
		"deadline_at": true, "attempt": true, "summary": true, "step": true, "halt": true, "principal": true, "approval": true,
		"abort": true, "failing": true, "view": true}
	for k := range m {
		if !want[k] {
			t.Errorf("undocumented top-level field %q", k)
		}
	}
	for k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing top-level field %q", k)
		}
	}
}

type withText struct {
	error
	text string
}

func (w *withText) Error() string { return w.text }
func (w *withText) Unwrap() error { return w.error }

func TestPayloadAllowlist(t *testing.T) {
	n := approvalWithSecrets()
	n.Err = &withText{n.Err, "stripe said: card " + cardNumber + " declined"}

	t.Run("ErrorText", func(t *testing.T) {
		m, _ := notifyJSON(t, plainWebhook(t, WithFields(ErrorText)), n)
		if m["error"] != "stripe said: card "+cardNumber+" declined" {
			t.Fatalf("error %v", m["error"])
		}
		if _, ok := m["approval"].(map[string]any)["args"]; ok {
			t.Fatal("ErrorText must not allow args")
		}
	})
	t.Run("Args for the named tool only", func(t *testing.T) {
		m, _ := notifyJSON(t, plainWebhook(t, WithFields(Args("refund"))), n)
		var want map[string]any
		_ = json.Unmarshal([]byte(argsJSON), &want)
		if got := m["approval"].(map[string]any)["args"]; !reflect.DeepEqual(got, any(want)) {
			t.Fatalf("args %v", got)
		}
		if _, ok := m["error"]; ok {
			t.Fatal("Args must not allow error text")
		}
		// Another tool's arguments are not sent.
		other := n
		other.Approval = &agent.NotifyApproval{ToolUseID: "call_02", ToolName: "transfer", Need: 1, Of: 1}
		other.Err = &agent.ApprovalPending{ToolUseID: "call_02", ToolName: "transfer", Args: json.RawMessage(argsJSON)}
		_, body := notifyJSON(t, plainWebhook(t, WithFields(Args("refund"))), other)
		if strings.Contains(body, cardNumber) {
			t.Fatalf("args of a tool that is not allowlisted were sent: %s", body)
		}
	})
	t.Run("invalid JSON args are not forwarded", func(t *testing.T) {
		bad := n
		bad.Err = &agent.ApprovalPending{ToolName: "refund", Args: json.RawMessage(`{"a":`)}
		if _, err := json.Marshal(plainWebhook(t, WithFields(Args("refund"))).payload(bad)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("error text is bounded", func(t *testing.T) {
		big := n
		big.Err = errors.New(strings.Repeat("é", maxErrorText))
		m, _ := notifyJSON(t, plainWebhook(t, WithFields(ErrorText)), big)
		if s := m["error"].(string); len(s) > maxErrorText {
			t.Fatalf("len %d", len(s))
		}
	})
}

func TestPayloadHasViewPointerNeverAnActionLink(t *testing.T) {
	w := plainWebhook(t, WithViewURL("https://viewer.example.com/runs/"))
	n := approvalWithSecrets()
	n.RunID = "orders 1042/../x"
	m, body := notifyJSON(t, w, n)
	v := m["view"].(map[string]any)
	if v["url"] != "https://viewer.example.com/runs/orders%201042%2F..%2Fx" {
		t.Fatalf("url %v", v["url"])
	}
	if v["command"] != `bide view -run 'orders 1042/../x'` {
		t.Fatalf("command %v", v["command"])
	}
	// The only URL in the payload is the view pointer; nothing decides on a click.
	if c := strings.Count(body, "http"); c != 1 {
		t.Fatalf("%d URLs in %s", c, body)
	}
	for _, bad := range []string{"approve", "resolve", "decide", "reject", "token"} {
		for k := range m {
			if strings.Contains(strings.ToLower(k), bad) {
				t.Errorf("key %q suggests an action link", k)
			}
		}
		if strings.Contains(strings.ToLower(v["url"].(string)), bad) {
			t.Errorf("view url contains %q", bad)
		}
	}
	if shellWord("-rf") != "'-rf'" || shellWord("a'b") != `'a'\''b'` || shellWord("ok-1.2:x") != "ok-1.2:x" || shellWord("") != "''" {
		t.Fatal("shellWord")
	}
}
