package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/notify"
)

const secret = "relay-test-secret-do-not-leak"

// upstream is a fake Slack or PagerDuty: it records bodies and answers per a script.
type upstream struct {
	*httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	status func(i int) int // status for request i (from 0); nil means 200
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		i := len(u.bodies)
		u.bodies = append(u.bodies, b)
		st := 200
		if u.status != nil {
			st = u.status(i)
		}
		u.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) count() int { u.mu.Lock(); defer u.mu.Unlock(); return len(u.bodies) }
func (u *upstream) last(t *testing.T) []byte {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		t.Fatal("upstream saw no request")
	}
	return u.bodies[len(u.bodies)-1]
}

type logs struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logs) logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.WriteString(strings.TrimSpace(sprintf(format, a...)) + "\n")
}
func (l *logs) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type rig struct {
	slack, pd *upstream
	h         *Handler
	srv       *httptest.Server
	log       *logs
}

// newRig wires a Handler with a Slack and a PagerDuty sink at fake upstreams, served over HTTP on
// loopback (which bide's own webhook accepts).
func newRig(t *testing.T, mod ...func(*Config)) *rig {
	t.Helper()
	r := &rig{slack: newUpstream(t), pd: newUpstream(t), log: &logs{}}
	sl, err := NewSlack(SlackConfig{WebhookURL: r.slack.URL + "/services/T000/B000/" + secret, Backoff: time.Millisecond, ViewURL: "https://viewer.example/runs"})
	if err != nil {
		t.Fatal(err)
	}
	pd, err := NewPagerDuty(PagerDutyConfig{RoutingKey: "routing-key-do-not-leak", URL: r.pd.URL, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Secrets: []string{secret}, Sinks: []Sink{sl, pd}, Logf: r.log.logf}
	for _, m := range mod {
		m(&cfg)
	}
	r.h, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.srv = httptest.NewServer(r.h)
	t.Cleanup(r.srv.Close)
	return r
}

func approval(runID string) agent.Notification {
	key := agent.ApprovalKey(runID, "call_01", 0)
	return agent.Notification{
		ID: agent.NotifyID(key), Key: key, Kind: agent.NotificationApprovalPending, RunID: runID,
		EventAt:  time.Date(2026, 10, 13, 9, 14, 5, 0, time.UTC),
		Deadline: time.Date(2026, 10, 13, 9, 19, 7, 0, time.UTC),
		Attempt:  1,
		Approval: &agent.NotifyApproval{ToolUseID: "call_01", ToolName: "refund", Need: 2, Of: 3, Approvers: []string{"alice", "bob", "carol"}, Approved: 1},
	}
}

// send delivers n through a real notify.Webhook, so the wire format is bide's own.
func (r *rig) send(t *testing.T, n agent.Notification, opts ...notify.Option) error {
	t.Helper()
	opts = append([]notify.Option{notify.WithSecrets(secret, ""), notify.WithTimeout(5 * time.Second)}, opts...)
	w, err := notify.NewWebhook(r.srv.URL, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return w.Notify(context.Background(), n)
}

// raw posts body with headers signed as bide signs them, then altered by mod.
func (r *rig) raw(t *testing.T, body []byte, mod func(h http.Header)) int {
	t.Helper()
	var p struct{ ID string }
	_ = json.Unmarshal(body, &p)
	ts := time.Now().Unix()
	h := http.Header{}
	h.Set(notify.HeaderDelivery, p.ID)
	h.Set(notify.HeaderTimestamp, strconv.FormatInt(ts, 10))
	h.Set(notify.HeaderSignature, notify.Sign([]string{secret}, ts, body))
	if mod != nil {
		mod(h)
	}
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL, bytes.NewReader(body))
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func payloadBody(t *testing.T, mod func(*notify.Payload)) []byte {
	t.Helper()
	p := notify.Payload{Schema: notify.Schema, ID: agent.NotifyID("k1"), Key: "k1", Kind: "approval_pending", RunID: "run-1", Summary: "Run run-1 is waiting.", View: notify.View{Command: "bide view -db <journal.db> -run run-1"}}
	if mod != nil {
		mod(&p)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEndToEndForwardsToSlackAndPagerDuty(t *testing.T) {
	r := newRig(t)
	n := approval("orders-1042")
	if err := r.send(t, n, notify.WithViewURL("https://bide.example.com/runs")); err != nil {
		t.Fatal(err)
	}
	if r.slack.count() != 1 || r.pd.count() != 1 {
		t.Fatalf("slack %d, pagerduty %d requests, want 1 each", r.slack.count(), r.pd.count())
	}
	var msg map[string]any
	if err := json.Unmarshal(r.slack.last(t), &msg); err != nil {
		t.Fatal(err)
	}
	flat := string(r.slack.last(t))
	for _, want := range []string{
		`"type":"header"`, "Approval pending", "orders-1042", "approval_pending", "refund",
		"2 of 3 needed, 1 given", "alice, bob, carol", "!date^1791883147^", "2026-10-13T09:19:07Z",
		"https://bide.example.com/runs/orders-1042", "Open in viewer", "bide view -db",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("slack message lacks %q:\n%s", want, flat)
		}
	}
	var ev PagerDutyEvent
	if err := json.Unmarshal(r.pd.last(t), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.EventAction != "trigger" || ev.DedupKey != n.ID || ev.RoutingKey == "" || ev.Payload.Severity != "warning" || ev.Payload.Source != "bide" || ev.Payload.Class != "approval_pending" {
		t.Fatalf("pagerduty event = %+v / %+v", ev, ev.Payload)
	}
	if len(ev.Links) != 1 || ev.Links[0].Href != "https://bide.example.com/runs/orders-1042" {
		t.Fatalf("links = %+v", ev.Links)
	}
}

func TestViewerLinkFromRelayConfigWhenPayloadHasNone(t *testing.T) {
	r := newRig(t)
	if err := r.send(t, approval("orders 7")); err != nil { // no WithViewURL: the relay's ViewURL applies
		t.Fatal(err)
	}
	if got := string(r.slack.last(t)); !strings.Contains(got, "https://viewer.example/runs/orders%207") {
		t.Fatalf("slack message lacks the relay-configured viewer link:\n%s", got)
	}
}

func TestBadSignatureIsRefusedAndNothingIsForwarded(t *testing.T) {
	r := newRig(t)
	body := payloadBody(t, nil)
	ts := time.Now().Unix()
	for name, mod := range map[string]func(http.Header){
		"wrong secret": func(h http.Header) {
			h.Set(notify.HeaderSignature, notify.Sign([]string{"another-secret"}, ts, body))
			h.Set(notify.HeaderTimestamp, strconv.FormatInt(ts, 10))
		},
		"no signature": func(h http.Header) { h.Del(notify.HeaderSignature) },
		"body altered": nil,
		"stale timestamp": func(h http.Header) {
			old := time.Now().Add(-time.Hour).Unix()
			h.Set(notify.HeaderTimestamp, strconv.FormatInt(old, 10))
			h.Set(notify.HeaderSignature, notify.Sign([]string{secret}, old, body))
		},
		"future timestamp": func(h http.Header) {
			f := time.Now().Add(time.Hour).Unix()
			h.Set(notify.HeaderTimestamp, strconv.FormatInt(f, 10))
			h.Set(notify.HeaderSignature, notify.Sign([]string{secret}, f, body))
		},
		"delivery header swapped": func(h http.Header) { h.Set(notify.HeaderDelivery, agent.NotifyID("other")) },
		"delivery header missing": func(h http.Header) { h.Del(notify.HeaderDelivery) },
	} {
		b := body
		if name == "body altered" {
			b = bytes.Replace(body, []byte("run-1"), []byte("run-2"), 1)
			// signed over the original body, sent with the altered one
			ts2 := time.Now().Unix()
			mod = func(h http.Header) {
				h.Set(notify.HeaderTimestamp, strconv.FormatInt(ts2, 10))
				h.Set(notify.HeaderSignature, notify.Sign([]string{secret}, ts2, body))
			}
		}
		if got := r.raw(t, b, mod); got != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, got)
		}
	}
	if r.slack.count()+r.pd.count() != 0 {
		t.Fatal("an unverified delivery was forwarded")
	}
	if strings.Contains(r.log.String(), secret) {
		t.Fatal("the secret is in the log")
	}
}

func TestPreviousSecretVerifiesDuringRotation(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Secrets = []string{"new-secret", secret} })
	if err := r.send(t, approval("r1")); err != nil { // signs with the old secret only
		t.Fatal(err)
	}
	if r.slack.count() != 1 {
		t.Fatal("a delivery signed with the previous secret was not forwarded")
	}
}

func TestWrongMethodAndBadDocument(t *testing.T) {
	r := newRig(t)
	resp, err := http.Get(r.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
		t.Fatalf("GET: %d Allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}
	if got := r.raw(t, payloadBody(t, func(p *notify.Payload) { p.Schema = "bide.notification.v2" }), nil); got != 400 {
		t.Fatalf("unknown schema: %d, want 400", got)
	}
	if got := r.raw(t, []byte(`{"id":"x"`), func(h http.Header) { h.Set(notify.HeaderDelivery, "x") }); got != 401 && got != 400 {
		t.Fatalf("malformed JSON: %d", got)
	}
	if r.slack.count()+r.pd.count() != 0 {
		t.Fatal("a bad document was forwarded")
	}
}

func TestRedeliveryIsAcknowledgedButForwardedOnce(t *testing.T) {
	r := newRig(t)
	n := approval("orders-1")
	for i := 0; i < 3; i++ {
		if err := r.send(t, n); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if r.slack.count() != 1 || r.pd.count() != 1 {
		t.Fatalf("slack %d, pagerduty %d requests for three deliveries of one event, want 1 each", r.slack.count(), r.pd.count())
	}
	if !strings.Contains(r.log.String(), "repeat") {
		t.Fatalf("the repeat was not logged:\n%s", r.log)
	}
	// A different event is forwarded.
	if err := r.send(t, approval("orders-2")); err != nil {
		t.Fatal(err)
	}
	if r.slack.count() != 2 {
		t.Fatal("a new event was treated as a repeat")
	}
}

func TestForwardingFailureAnswers5xxAndTheRetryDelivers(t *testing.T) {
	r := newRig(t)
	r.slack.status = func(int) int { return 500 }
	n := approval("orders-1")
	err := r.send(t, n)
	var perr *notify.PermanentError
	if err == nil || errors.As(err, &perr) {
		t.Fatalf("send = %v, want a retryable (5xx) error, not a permanent one", err)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("send = %v, want HTTP 502", err)
	}
	// Slack recovers; bide's retry of the same event goes through, to Slack only: PagerDuty took it.
	r.slack.mu.Lock()
	r.slack.status = nil
	r.slack.mu.Unlock()
	slackBefore, pdBefore := r.slack.count(), r.pd.count()
	if err := r.send(t, n); err != nil {
		t.Fatal(err)
	}
	if r.slack.count() != slackBefore+1 || r.pd.count() != pdBefore {
		t.Fatalf("retry: slack %d->%d, pagerduty %d->%d; want only slack to be sent again", slackBefore, r.slack.count(), pdBefore, r.pd.count())
	}
	// And now it is a repeat for both.
	if err := r.send(t, n); err != nil {
		t.Fatal(err)
	}
	if r.slack.count() != slackBefore+1 || r.pd.count() != pdBefore {
		t.Fatal("a fully forwarded event was forwarded again")
	}
}

func TestFailureNeverLeaksCredentials(t *testing.T) {
	r := newRig(t)
	r.slack.status = func(int) int { return 500 }
	r.pd.status = func(int) int { return 400 }
	_ = r.send(t, approval("orders-1"))
	got := r.log.String()
	if !strings.Contains(got, "slack failed") || !strings.Contains(got, "pagerduty failed") {
		t.Fatalf("failures not logged:\n%s", got)
	}
	for _, leak := range []string{secret, "routing-key-do-not-leak", "/services/T000", r.slack.URL + "/"} {
		if strings.Contains(got, leak) {
			t.Fatalf("log leaks %q:\n%s", leak, got)
		}
	}
}

func TestConcurrentDuplicateWaitsRatherThanAcknowledging(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 4)
	slow := &funcSink{name: "slow", fn: func(ctx context.Context, p notify.Payload) error {
		entered <- struct{}{}
		<-gate
		return nil
	}}
	h, err := New(Config{Secrets: []string{secret}, Sinks: []Sink{slow}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	rg := &rig{srv: srv}
	body := payloadBody(t, nil)
	first := make(chan int, 1)
	go func() { first <- rg.raw(t, body, nil) }()
	<-entered
	if got := rg.raw(t, body, nil); got != http.StatusServiceUnavailable {
		t.Fatalf("a duplicate while the first is forwarding: %d, want 503 (not an ack for work not yet done)", got)
	}
	close(gate)
	if got := <-first; got != 204 {
		t.Fatalf("first: %d", got)
	}
	if got := rg.raw(t, body, nil); got != 204 {
		t.Fatalf("after completion: %d, want 204", got)
	}
	if len(entered) != 0 {
		t.Fatal("the sink ran more than once")
	}
}

func TestForwardingFinishesAfterTheSenderGivesUp(t *testing.T) {
	var done atomic.Int32
	slow := &funcSink{name: "slow", fn: func(ctx context.Context, p notify.Payload) error {
		select {
		case <-time.After(150 * time.Millisecond):
			done.Add(1)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	h, _ := New(Config{Secrets: []string{secret}, Sinks: []Sink{slow}})
	srv := httptest.NewServer(h)
	defer srv.Close()
	body := payloadBody(t, nil)
	ts := time.Now().Unix()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(body))
	req.Header.Set(notify.HeaderDelivery, agent.NotifyID("k1"))
	req.Header.Set(notify.HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(notify.HeaderSignature, notify.Sign([]string{secret}, ts, body))
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("the sender did not time out")
	}
	deadline := time.Now().Add(3 * time.Second)
	for done.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if done.Load() != 1 {
		t.Fatal("forwarding was abandoned when the sender gave up")
	}
	rg := &rig{srv: srv}
	// The sender's retry finds it done: a repeat, acknowledged without a second forward.
	deadline = time.Now().Add(3 * time.Second)
	got := 0
	for time.Now().Before(deadline) {
		if got = rg.raw(t, body, nil); got == 204 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got != 204 || done.Load() != 1 {
		t.Fatalf("retry after the sender gave up: status %d, forwards %d", got, done.Load())
	}
}

func TestDedupeCacheIsBounded(t *testing.T) {
	var n atomic.Int32
	count := &funcSink{name: "c", fn: func(context.Context, notify.Payload) error { n.Add(1); return nil }}
	h, _ := New(Config{Secrets: []string{secret}, Sinks: []Sink{count}, DedupeSize: 3})
	srv := httptest.NewServer(h)
	defer srv.Close()
	rg := &rig{srv: srv}
	send := func(i int) {
		id := agent.NotifyID("k" + strconv.Itoa(i))
		rg.raw(t, payloadBody(t, func(p *notify.Payload) { p.ID, p.Key = id, "k"+strconv.Itoa(i) }), nil)
	}
	for i := 0; i < 50; i++ {
		send(i)
	}
	if h.seen.ll.Len() > 3 || len(h.seen.m) > 3 {
		t.Fatalf("cache holds %d/%d entries, bound is 3", h.seen.ll.Len(), len(h.seen.m))
	}
	before := n.Load()
	send(49) // recent: remembered
	if n.Load() != before {
		t.Fatal("a remembered ID was forwarded again")
	}
	send(0) // long evicted: forwarded again (a rare repeat, never a lost notification)
	if n.Load() != before+1 {
		t.Fatal("an evicted ID was dropped instead of forwarded")
	}
}

func TestRelayNeverForwardsErrorTextOrArguments(t *testing.T) {
	r := newRig(t)
	n := approval("orders-1")
	n.Err = &agent.ApprovalPending{Args: json.RawMessage(`{"card":"4111111111111111"}`)}
	err := r.send(t, n, notify.WithFields(notify.ErrorText, notify.Args("refund")))
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []*upstream{r.slack, r.pd} {
		if got := string(u.last(t)); strings.Contains(got, "4111111111111111") || strings.Contains(got, `"error"`) {
			t.Fatalf("sensitive field forwarded: %s", got)
		}
	}
}

func TestSlackEscapesRunValues(t *testing.T) {
	r := newRig(t)
	p := notify.Payload{Schema: notify.Schema, ID: agent.NotifyID("evil"), Kind: "approval_pending", RunID: "<!channel> <@U123> & `x`", Summary: "Run <!channel> is waiting & <https://evil.example|click>.",
		Approval: &notify.Approval{Tool: "<!here>", Approvers: []string{"<@U999>"}, Policy: notify.Policy{Need: 1, Of: 1}}, View: notify.View{Command: "bide view -run '<!channel>'"}}
	b, _ := json.Marshal(p)
	if got := r.raw(t, b, nil); got != 204 {
		t.Fatalf("status %d", got)
	}
	got := string(r.slack.last(t))
	for _, bad := range []string{"<!channel>", "<@U123>", "<!here>", "<@U999>", "<https://evil.example|click>"} {
		if strings.Contains(got, bad) {
			t.Fatalf("slack message holds a live %q:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "\\u0026lt;!channel\\u0026gt;") && !strings.Contains(got, "&lt;!channel&gt;") {
		t.Fatalf("expected the escaped form of the mention:\n%s", got)
	}
}

func TestNewRejectsUnusableConfig(t *testing.T) {
	ok := &funcSink{name: "a"}
	cases := map[string]Config{
		"no secret":      {Sinks: []Sink{ok}},
		"empty secret":   {Secrets: []string{""}, Sinks: []Sink{ok}},
		"no sinks":       {Secrets: []string{"s"}},
		"duplicate name": {Secrets: []string{"s"}, Sinks: []Sink{ok, &funcSink{name: "a"}}},
		"unnamed sink":   {Secrets: []string{"s"}, Sinks: []Sink{&funcSink{}}},
	}
	for name, c := range cases {
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPanickingSinkDoesNotWedgeTheDelivery(t *testing.T) {
	var calls atomic.Int32
	bad := &funcSink{name: "bad", fn: func(context.Context, notify.Payload) error {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		return nil
	}}
	h, _ := New(Config{Secrets: []string{secret}, Sinks: []Sink{bad}})
	srv := httptest.NewServer(h)
	defer srv.Close()
	rg := &rig{srv: srv}
	body := payloadBody(t, nil)
	if got := rg.raw(t, body, nil); got != 502 {
		t.Fatalf("panic: %d, want 502", got)
	}
	if got := rg.raw(t, body, nil); got != 204 {
		t.Fatalf("retry: %d, want 204 (not stuck in flight)", got)
	}
}

type funcSink struct {
	name string
	fn   func(context.Context, notify.Payload) error
}

func (f *funcSink) Name() string { return f.name }
func (f *funcSink) Send(ctx context.Context, p notify.Payload) error {
	if f.fn == nil {
		return nil
	}
	return f.fn(ctx, p)
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

func TestEvictionNeverDropsAnInFlightDelivery(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 2)
	var sends atomic.Int32
	sink := &funcSink{name: "s", fn: func(_ context.Context, p notify.Payload) error {
		sends.Add(1)
		if p.Key == "slow" {
			entered <- struct{}{}
			<-gate
		}
		return nil
	}}
	h, _ := New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, DedupeSize: 1})
	srv := httptest.NewServer(h)
	defer srv.Close()
	rg := &rig{srv: srv}
	slow := payloadBody(t, func(p *notify.Payload) { p.ID, p.Key = agent.NotifyID("slow"), "slow" })
	done := make(chan int, 1)
	go func() { done <- rg.raw(t, slow, nil) }()
	<-entered
	// Another event arrives while the first is in flight and the cache is full.
	if got := rg.raw(t, payloadBody(t, func(p *notify.Payload) { p.ID, p.Key = agent.NotifyID("fast"), "fast" }), nil); got != 204 {
		t.Fatalf("second event: %d", got)
	}
	close(gate)
	<-done
	before := sends.Load()
	if got := rg.raw(t, slow, nil); got != 204 || sends.Load() != before {
		t.Fatalf("repeat of the in-flight event: status %d, %d extra sends (its claim was evicted)", got, sends.Load()-before)
	}
}

func TestLateTriggerAfterItsClearIsAcknowledgedButDropped(t *testing.T) {
	var sends atomic.Int32
	sink := &funcSink{name: "s", fn: func(context.Context, notify.Payload) error { sends.Add(1); return nil }}
	trigID := agent.NotifyID("k1")
	h, _ := New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, Logf: func(string, ...any) {},
		ClearsID: func(p notify.Payload) string {
			if p.Kind == "resolved" {
				return trigID
			}
			return ""
		}})
	srv := httptest.NewServer(h)
	defer srv.Close()
	rg := &rig{srv: srv}
	clear := payloadBody(t, func(p *notify.Payload) {
		p.Kind, p.ID, p.Key = "resolved", agent.NotifyID("resolved:k1"), "resolved:k1"
	})
	if got := rg.raw(t, clear, nil); got != 204 || sends.Load() != 1 {
		t.Fatalf("clear: %d, %d sends", got, sends.Load())
	}
	if got := rg.raw(t, payloadBody(t, nil), nil); got != 204 || sends.Load() != 1 {
		t.Fatalf("late trigger: status %d, %d sends; want 204 and no forward", got, sends.Load())
	}
	// An unrelated event is still forwarded.
	other := payloadBody(t, func(p *notify.Payload) { p.ID, p.Key = agent.NotifyID("k2"), "k2" })
	if got := rg.raw(t, other, nil); got != 204 || sends.Load() != 2 {
		t.Fatalf("unrelated: %d, %d sends", got, sends.Load())
	}
}

func TestClearedSetIsBounded(t *testing.T) {
	s := newIDSet(3)
	for i := 0; i < 20; i++ {
		s.add(strconv.Itoa(i))
	}
	if len(s.m) != 3 || s.ll.Len() != 3 || !s.has("19") || s.has("0") {
		t.Fatalf("set holds %d entries", len(s.m))
	}
}

func TestClearThatFailsToForwardStillGuardsTheEvent(t *testing.T) {
	fail := atomic.Bool{}
	fail.Store(true)
	sink := &funcSink{name: "s", fn: func(_ context.Context, p notify.Payload) error {
		if fail.Load() {
			return errors.New("down")
		}
		return nil
	}}
	h, _ := New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, Logf: func(string, ...any) {},
		ClearsID: func(p notify.Payload) string {
			if p.Kind == "resolved" {
				return agent.NotifyID("k1")
			}
			return ""
		}})
	srv := httptest.NewServer(h)
	defer srv.Close()
	rg := &rig{srv: srv}
	clear := payloadBody(t, func(p *notify.Payload) {
		p.Kind, p.ID, p.Key = "resolved", agent.NotifyID("resolved:k1"), "resolved:k1"
	})
	if got := rg.raw(t, clear, nil); got != 502 {
		t.Fatalf("clear: %d", got)
	}
	fail.Store(false)
	if got := rg.raw(t, payloadBody(t, nil), nil); got != 204 {
		t.Fatalf("late trigger: %d", got)
	}
}

func resolved(runID string) agent.Notification {
	orig := approval(runID)
	key := agent.ResolvedKey(orig.Key)
	return agent.Notification{
		ID: agent.NotifyID(key), Key: key, Kind: agent.NotificationResolved, RunID: runID, Attempt: 1, Reason: "decided",
		EventAt: orig.EventAt.Add(time.Minute),
		Clears:  &agent.NotifyClears{Key: orig.Key, ID: orig.ID, Kind: orig.Kind},
	}
}

func TestTriggerThenResolveWithPagerDuty(t *testing.T) {
	r := newRig(t)
	trig := approval("orders-1042")
	if err := r.send(t, trig); err != nil {
		t.Fatal(err)
	}
	if err := r.send(t, resolved("orders-1042")); err != nil {
		t.Fatal(err)
	}
	if r.pd.count() != 2 {
		t.Fatalf("pagerduty saw %d requests, want trigger and resolve", r.pd.count())
	}
	var first, second PagerDutyEvent
	json.Unmarshal(r.pd.bodies[0], &first)
	json.Unmarshal(r.pd.bodies[1], &second)
	if first.EventAction != "trigger" || second.EventAction != "resolve" || first.DedupKey != trig.ID || second.DedupKey != trig.ID || second.Payload != nil {
		t.Fatalf("trigger %+v, resolve %+v", first, second)
	}
	// Slack hears the all clear too.
	if r.slack.count() != 2 || !strings.Contains(string(r.slack.last(t)), "All clear") {
		t.Fatalf("slack: %d messages; last:\n%s", r.slack.count(), r.slack.last(t))
	}
	// Redelivering the clear does not resolve twice; a trigger arriving late is dropped.
	if err := r.send(t, resolved("orders-1042")); err != nil {
		t.Fatal(err)
	}
	if err := r.send(t, trig); err != nil {
		t.Fatal(err)
	}
	if r.pd.count() != 2 || r.slack.count() != 2 {
		t.Fatalf("after a repeat and a late trigger: pagerduty %d, slack %d, want 2 each", r.pd.count(), r.slack.count())
	}
}

func TestClearBeforeItsEventNeverOpensAnIncident(t *testing.T) {
	r := newRig(t)
	if err := r.send(t, resolved("orders-7")); err != nil {
		t.Fatal(err)
	}
	if err := r.send(t, approval("orders-7")); err != nil { // the event's delayed delivery
		t.Fatal(err)
	}
	for i, b := range r.pd.bodies {
		var ev PagerDutyEvent
		json.Unmarshal(b, &ev)
		if ev.EventAction == "trigger" {
			t.Fatalf("pagerduty request %d is a trigger after the clear", i)
		}
	}
}
