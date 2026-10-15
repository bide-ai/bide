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
	"os"
	"os/exec"
	"path/filepath"
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

// restartRig is a Handler with a state file, restarted by building another over the same file.
func restartHandler(t *testing.T, path string, sends *atomic.Int32, size int) (*httptest.Server, *rig) {
	t.Helper()
	sink := &funcSink{name: "s", fn: func(context.Context, notify.Payload) error { sends.Add(1); return nil }}
	h, err := New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, StatePath: path, StateFlushDelay: -1, DedupeSize: size, Logf: func(string, ...any) {},
		ClearsID: func(p notify.Payload) string {
			if p.Kind == "resolved" {
				return agent.NotifyID("k1")
			}
			return ""
		}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	handlers.Store(srv, h)
	t.Cleanup(func() {
		closeSrv(srv)
		if path != "" {
			requireStateFree(t, path, "after the test's relay closed")
		}
	})
	return srv, &rig{srv: srv}
}

var handlers sync.Map // *httptest.Server -> *Handler

// closeSrv stops a restartHandler server and releases its state lock, as a relay exiting does.
func closeSrv(srv *httptest.Server) {
	srv.Close()
	if h, ok := handlers.LoadAndDelete(srv); ok {
		h.(*Handler).Close()
	}
}

func TestStateFileKeepsClearedIDsAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	var sends atomic.Int32
	srv, rg := restartHandler(t, path, &sends, 0)
	clear := payloadBody(t, func(p *notify.Payload) {
		p.Kind, p.ID, p.Key = "resolved", agent.NotifyID("resolved:k1"), "resolved:k1"
	})
	if got := rg.raw(t, clear, nil); got != 204 {
		t.Fatalf("clear: %d", got)
	}
	closeSrv(srv) // the relay restarts
	sends.Store(0)
	_, rg = restartHandler(t, path, &sends, 0)
	if got := rg.raw(t, payloadBody(t, nil), nil); got != 204 || sends.Load() != 0 {
		t.Fatalf("late trigger after a restart: status %d, %d sends; want 204 and none", got, sends.Load())
	}
	// The clear itself is a repeat after the restart too.
	if got := rg.raw(t, clear, nil); got != 204 || sends.Load() != 0 {
		t.Fatalf("redelivered clear after a restart: %d, %d sends", got, sends.Load())
	}
}

func TestWithoutAStateFileARestartForgets(t *testing.T) {
	var sends atomic.Int32
	srv, rg := restartHandler(t, "", &sends, 0)
	clear := payloadBody(t, func(p *notify.Payload) {
		p.Kind, p.ID, p.Key = "resolved", agent.NotifyID("resolved:k1"), "resolved:k1"
	})
	rg.raw(t, clear, nil)
	closeSrv(srv)
	sends.Store(0)
	_, rg = restartHandler(t, "", &sends, 0)
	if got := rg.raw(t, payloadBody(t, nil), nil); got != 204 || sends.Load() != 1 {
		t.Fatalf("without -state a trigger after a restart is forwarded again: %d, %d sends", got, sends.Load())
	}
}

func TestStateFileKeepsWhichSinksTookAnEventAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	var sends atomic.Int32
	srv, rg := restartHandler(t, path, &sends, 0)
	body := payloadBody(t, func(p *notify.Payload) { p.ID, p.Key = agent.NotifyID("k9"), "k9" })
	if got := rg.raw(t, body, nil); got != 204 || sends.Load() != 1 {
		t.Fatal(got)
	}
	closeSrv(srv)
	_, rg = restartHandler(t, path, &sends, 0)
	if got := rg.raw(t, body, nil); got != 204 || sends.Load() != 1 {
		t.Fatalf("redelivery after a restart: %d, %d sends, want 1 in all", got, sends.Load())
	}
}

func TestStateFileIsBoundedAndKeepsTheNewest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	var sends atomic.Int32
	srv, rg := restartHandler(t, path, &sends, 3)
	for i := 0; i < 10; i++ {
		k := "e" + strconv.Itoa(i)
		rg.raw(t, payloadBody(t, func(p *notify.Payload) { p.ID, p.Key = agent.NotifyID(k), k }), nil)
	}
	closeSrv(srv)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Seen []struct{ ID string } `json:"seen"`
	}
	if err := json.Unmarshal(b, &st); err != nil || len(st.Seen) != 3 {
		t.Fatalf("state holds %d seen entries (%v), want 3", len(st.Seen), err)
	}
	sends.Store(0)
	_, rg = restartHandler(t, path, &sends, 3)
	rg.raw(t, payloadBody(t, func(p *notify.Payload) { p.ID, p.Key = agent.NotifyID("e9"), "e9" }), nil)
	if sends.Load() != 0 {
		t.Fatal("the newest event was forgotten")
	}
	rg.raw(t, payloadBody(t, func(p *notify.Payload) { p.ID, p.Key = agent.NotifyID("e0"), "e0" }), nil)
	if sends.Load() != 1 {
		t.Fatal("the oldest event was kept past the bound")
	}
}

// requireStateFree fails unless nothing in this process holds the state file's lock or an open
// handle on its lock file: the lock can be taken again, and the lock file can be deleted (Windows
// refuses to delete a file with an open handle, and TempDir cleanup is where that shows up).
func requireStateFree(t *testing.T, path, what string) {
	t.Helper()
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	held, err := lockFile(f)
	f.Close()
	if err != nil || held {
		t.Errorf("%s: the state lock is still held (held=%v, err=%v)", what, held, err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Errorf("%s: the lock file cannot be removed, so a handle on it is still open: %v", what, err)
	}
}

func TestStateFileProblemsAreRefusedAtStart(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.state")
	os.WriteFile(bad, []byte("{not json"), 0o600)
	sink := &funcSink{name: "s"}
	start := func(path string) (*Handler, error) {
		return New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, StatePath: path})
	}
	if _, err := start(bad); err == nil {
		t.Error("a corrupt state file was accepted: the relay would run with its memory silently empty")
	}
	requireStateFree(t, bad, "after a corrupt file")
	missing := filepath.Join(dir, "missing-dir", "x.state")
	if _, err := start(missing); err == nil {
		t.Error("an unwritable state path was accepted")
	}
	// A refused start releases the lock: once the file is fixed the next start works.
	os.WriteFile(bad, []byte(`{"version":1,"cleared":[],"seen":[]}`), 0o600)
	if h, err := start(bad); err != nil {
		t.Errorf("after fixing the file: %v", err)
	} else {
		h.Close()
		requireStateFree(t, bad, "after Close")
	}
	os.WriteFile(bad, []byte("{not json"), 0o600)
	if _, err := start(bad); err == nil || strings.Contains(err.Error(), "in use") {
		t.Errorf("a refused start left the lock held: %v", err)
	}
	requireStateFree(t, bad, "after a second corrupt-file refusal")
	wrong := filepath.Join(dir, "v2.state")
	os.WriteFile(wrong, []byte(`{"version":2,"cleared":[],"seen":[]}`), 0o600)
	if _, err := start(wrong); err == nil {
		t.Error("a state file of another version was accepted")
	}
	requireStateFree(t, wrong, "after a wrong version")
	// A missing file is a first start.
	fresh := filepath.Join(dir, "new.state")
	h, err := start(fresh)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if h2, err := start(fresh); err == nil || !strings.Contains(err.Error(), "in use") {
		if h2 != nil {
			h2.Close()
		}
		t.Errorf("a second start on a held file: %v", err)
	}
	h.Close() // a started relay holds the lock file open until Close, and Windows cannot delete it before
	requireStateFree(t, fresh, "after Close")
}

func TestStateFileHoldsNoSecretOrPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	var sends atomic.Int32
	srv, rg := restartHandler(t, path, &sends, 0)
	rg.raw(t, payloadBody(t, nil), nil)
	closeSrv(srv)
	b, _ := os.ReadFile(path)
	for _, leak := range []string{secret, "run-1", "waiting"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("state file holds %q: %s", leak, b)
		}
	}
}

func TestStateFileKeepsRecencyOrderAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	var sends atomic.Int32
	srv, rg := restartHandler(t, path, &sends, 3)
	send := func(rg *rig, k string) {
		rg.raw(t, payloadBody(t, func(p *notify.Payload) { p.ID, p.Key = agent.NotifyID(k), k }), nil)
	}
	for _, k := range []string{"a", "b", "c"} {
		send(rg, k)
	}
	closeSrv(srv)
	sends.Store(0)
	_, rg = restartHandler(t, path, &sends, 3)
	send(rg, "d") // evicts the oldest, a
	if sends.Load() != 1 {
		t.Fatal("d was not forwarded")
	}
	send(rg, "c") // still remembered
	send(rg, "b")
	if sends.Load() != 1 {
		t.Fatalf("a recent event was forgotten after the restart (%d sends)", sends.Load())
	}
	send(rg, "a") // the oldest went first
	if sends.Load() != 2 {
		t.Fatal("the oldest event outlived a newer one across the restart")
	}
}

// ---- the state lock and torn writes ----

func TestASecondRelayOnTheSameStateFileIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	sink := &funcSink{name: "s"}
	first, err := New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, StatePath: path}); err == nil || !strings.Contains(err.Error(), "in use by another relay") {
		t.Fatalf("second relay: %v, want a refusal naming the file", err)
	}
	// The refused relay must not have disturbed the first one's file.
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, StatePath: path})
	if err != nil {
		t.Fatalf("after the first closed: %v", err)
	}
	second.Close()
}

// TestMain lets the test binary act as a relay process that takes the state lock and waits.
func TestMain(m *testing.M) {
	if path := os.Getenv("RELAY_TEST_HOLD_STATE"); path != "" {
		if _, err := New(Config{Secrets: []string{secret}, Sinks: []Sink{&funcSink{name: "s"}}, StatePath: path}); err != nil {
			fmt.Println("refused:", err)
			os.Exit(3)
		}
		fmt.Println("locked")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func startHolder(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "RELAY_TEST_HOLD_STATE="+path)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	line := make(chan string, 1)
	go func() { b := make([]byte, 64); n, _ := out.Read(b); line <- string(b[:n]) }()
	select {
	case l := <-line:
		if !strings.HasPrefix(l, "locked") {
			t.Fatalf("holder: %q", l)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("holder did not take the lock")
	}
	return cmd
}

func TestStateLockIsHeldAcrossProcessesAndDiesWithTheHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	sink := &funcSink{name: "s"}
	holder := startHolder(t, path)
	if _, err := New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, StatePath: path}); err == nil || !strings.Contains(err.Error(), "in use by another relay") {
		t.Fatalf("while another process holds it: %v", err)
	}
	holder.Process.Kill() // a crash: no Close, no cleanup
	holder.Wait()
	var h *Handler
	var err error
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if h, err = New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, StatePath: path}); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("after the holder crashed the lock is stale: %v", err)
	}
	h.Close()
}

func TestKilledSaveLeavesTheOldStateLoadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	var sends atomic.Int32
	srv, rg := restartHandler(t, path, &sends, 0)
	rg.raw(t, payloadBody(t, func(p *notify.Payload) { p.ID, p.Key = agent.NotifyID("old"), "old" }), nil)
	before, _ := os.ReadFile(path)
	// The next save is killed between writing the temporary file and the rename.
	h, _ := handlers.Load(srv)
	h.(*Handler).state.beforeRename = func() error { return errors.New("killed") }
	rg.raw(t, payloadBody(t, func(p *notify.Payload) { p.ID, p.Key = agent.NotifyID("new"), "new" }), nil)
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("a save that did not finish changed the state file:\n%s\n%s", before, after)
	}
	if tmp, _ := filepath.Glob(path + ".tmp-*"); len(tmp) != 0 {
		t.Fatalf("a failed save left %v", tmp)
	}
	closeSrv(srv)
	// A real kill leaves the temporary file behind: plant one, with garbage in it.
	os.WriteFile(path+".tmp-123", []byte("{torn"), 0o600)
	sends.Store(0)
	_, rg = restartHandler(t, path, &sends, 0)
	rg.raw(t, payloadBody(t, func(p *notify.Payload) { p.ID, p.Key = agent.NotifyID("old"), "old" }), nil)
	if sends.Load() != 0 {
		t.Fatal("the old state did not load after a killed save")
	}
	if tmp, _ := filepath.Glob(path + ".tmp-*"); len(tmp) != 0 {
		t.Fatalf("a stale temporary file was kept: %v", tmp)
	}
}

// ---- coalesced writes, directory sync, duplicate restore ----

func coalescingHandler(t *testing.T, path string, delay time.Duration) (*Handler, *rig) {
	t.Helper()
	sink := &funcSink{name: "s"}
	h, err := New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, StatePath: path, StateFlushDelay: delay, Logf: func(string, ...any) {},
		ClearsID: func(p notify.Payload) string {
			if p.Kind == "resolved" {
				return agent.NotifyID("k1")
			}
			return ""
		}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		h.Close() // a relay that was not closed holds its lock file open, which Windows cannot delete
		requireStateFree(t, path, "after the test's relay closed")
	})
	return h, &rig{srv: srv}
}

func sendEvents(t *testing.T, rg *rig, n int, prefix string) {
	t.Helper()
	for i := 0; i < n; i++ {
		k := prefix + strconv.Itoa(i)
		rg.raw(t, payloadBody(t, func(p *notify.Payload) { p.ID, p.Key = agent.NotifyID(k), k }), nil)
	}
}

func seenInFile(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st stateFile
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return len(st.Seen)
}

func TestStateWritesAreCoalescedAndFlushedWithinTheDelay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	h, rg := coalescingHandler(t, path, 100*time.Millisecond)
	base := h.state.saveCount()
	sendEvents(t, rg, 40, "e")
	if n := h.state.saveCount() - base; n > 10 {
		t.Fatalf("%d writes for 40 events inside one window; writes are not coalesced", n)
	}
	waitFor2(t, "the flush", func() bool { return seenInFile(t, path) == 40 })
	h.Close()
}

func TestCloseFlushesPendingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	h, rg := coalescingHandler(t, path, time.Hour)
	sendEvents(t, rg, 5, "e")
	if n := seenInFile(t, path); n != 0 {
		t.Fatalf("file holds %d entries before the window ended", n)
	}
	h.Close()
	if n := seenInFile(t, path); n != 5 {
		t.Fatalf("Close left %d of 5 entries unwritten", n)
	}
}

func TestACrashLosesOnlyTheLastWindowOfDedupeEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	h, rg := coalescingHandler(t, path, time.Hour)
	sendEvents(t, rg, 3, "old")
	h.state.save(h.seen, h.cleared) // written by an earlier window
	sendEvents(t, rg, 3, "new")
	h.crash() // no Close: the pending window is lost
	h2, _ := coalescingHandler(t, path, time.Hour)
	if n := len(h2.seen.snapshot()); n != 3 {
		t.Fatalf("after a crash %d entries survive, want the 3 written before the window", n)
	}
	h2.Close()
}

func TestAClearIsWrittenAtOnceNotWithTheWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	h, rg := coalescingHandler(t, path, time.Hour)
	clear := payloadBody(t, func(p *notify.Payload) {
		p.Kind, p.ID, p.Key = "resolved", agent.NotifyID("resolved:k1"), "resolved:k1"
	})
	rg.raw(t, clear, nil)
	h.crash()
	h2, rg2 := coalescingHandler(t, path, time.Hour)
	var sends atomic.Int32
	h2.sinks = []Sink{&funcSink{name: "s", fn: func(context.Context, notify.Payload) error { sends.Add(1); return nil }}}
	if got := rg2.raw(t, payloadBody(t, nil), nil); got != 204 || sends.Load() != 0 {
		t.Fatalf("a trigger after a crash that followed a clear: %d, %d sends; the clear must be durable at once", got, sends.Load())
	}
	h2.Close()
}

func TestWriteThroughWhenTheDelayIsNegative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	h, rg := coalescingHandler(t, path, -1)
	sendEvents(t, rg, 3, "e")
	if n := seenInFile(t, path); n != 3 {
		t.Fatalf("write-through left %d of 3 entries unwritten", n)
	}
	h.Close()
}

func TestSyncDirIsCalledAfterTheRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	h, _ := coalescingHandler(t, path, -1)
	var order []string
	h.state.afterRename = func() { order = append(order, "after-rename") }
	h.state.syncDir = func(dir string) error { order = append(order, "syncdir:"+filepath.Base(dir)); return nil }
	if err := h.state.save(h.seen, h.cleared); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "after-rename" || !strings.HasPrefix(order[1], "syncdir:") {
		t.Fatalf("order %v: the directory must be synced after the rename", order)
	}
	h.state.syncDir = func(string) error { return errors.New("disk") }
	if err := h.state.save(h.seen, h.cleared); err == nil {
		t.Fatal("a failed directory sync was not reported")
	}
	h.state.syncDir = syncDir
	h.Close()
}

func TestRestoreDropsDuplicateIDsFromTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	os.WriteFile(path, []byte(`{"version":1,"cleared":["c","c"],"seen":[{"id":"x","done":["a"]},{"id":"y","done":["a"]},{"id":"x","done":["b"]}]}`), 0o600)
	h, _ := coalescingHandler(t, path, -1)
	defer h.Close()
	if h.seen.ll.Len() != 2 || len(h.seen.m) != 2 || h.cleared.ll.Len() != 1 {
		t.Fatalf("seen %d/%d, cleared %d; want 2/2 and 1", h.seen.ll.Len(), len(h.seen.m), h.cleared.ll.Len())
	}
	snap := h.seen.snapshot()
	var x seenState
	for _, e := range snap {
		if e.ID == "x" {
			x = e
		}
	}
	if len(x.Done) != 2 {
		t.Fatalf("a duplicate's sinks were not merged: %+v", snap)
	}
	if snap[len(snap)-1].ID != "x" {
		t.Fatalf("the later duplicate should be the most recent: %+v", snap)
	}
}

func waitFor2(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func BenchmarkStateSave(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			d, c := newDedupe(n), newIDSet(n)
			for i := 0; i < n; i++ {
				d.restore(agent.NotifyID("e"+strconv.Itoa(i)), []string{"slack", "pagerduty"})
				c.add(agent.NotifyID("c" + strconv.Itoa(i)))
			}
			s := &stateStore{path: filepath.Join(b.TempDir(), "relay.state"), syncDir: syncDir}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.save(d, c); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestAFlushAfterCloseWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	h, rg := coalescingHandler(t, path, time.Hour)
	sendEvents(t, rg, 2, "e")
	h.Close()
	n := h.state.saveCount()
	h.flush() // a timer that fired as the relay closed must not write without the lock
	if h.state.saveCount() != n {
		t.Fatal("a flush after Close wrote the state file without holding the lock")
	}
}

func TestZeroFlushDelayMeansTheDefault(t *testing.T) {
	h, _ := coalescingHandler(t, filepath.Join(t.TempDir(), "relay.state"), 0)
	defer h.Close()
	if h.delay != DefaultStateFlushDelay {
		t.Fatalf("delay %v, want %v", h.delay, DefaultStateFlushDelay)
	}
}
