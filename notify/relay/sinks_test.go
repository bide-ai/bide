package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/notify"
)

func samplePayload(kind string) notify.Payload {
	at := time.Date(2026, 10, 13, 9, 14, 5, 0, time.UTC)
	return notify.Payload{Schema: notify.Schema, ID: agent.NotifyID("k-" + kind), Key: "k-" + kind, Kind: kind, RunID: "run-9", EventAt: &at, Summary: "Run run-9 needs attention.", View: notify.View{Command: "bide view -db <journal.db> -run run-9"}}
}

func TestOutboundRetriesServerErrorsThenSucceeds(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	s, err := NewSlack(SlackConfig{WebhookURL: srv.URL, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), samplePayload("run_aborted")); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 3 {
		t.Fatalf("%d requests, want 3", n.Load())
	}
}

func TestOutboundAttemptsAreBounded(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1); w.WriteHeader(500) }))
	defer srv.Close()
	s, _ := NewSlack(SlackConfig{WebhookURL: srv.URL, Backoff: time.Millisecond, Attempts: 2})
	err := s.Send(context.Background(), samplePayload("run_aborted"))
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 500 || n.Load() != 2 {
		t.Fatalf("err %v after %d requests, want a 500 StatusError after 2", err, n.Load())
	}
}

func TestOutboundDoesNotRetryARefusal(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(404)
		w.Write([]byte("no_service"))
	}))
	defer srv.Close()
	s, _ := NewSlack(SlackConfig{WebhookURL: srv.URL + "/secret/path", Backoff: time.Millisecond})
	err := s.Send(context.Background(), samplePayload("run_aborted"))
	if err == nil || n.Load() != 1 {
		t.Fatalf("err %v after %d requests, want one", err, n.Load())
	}
	if !strings.Contains(err.Error(), "no_service") || strings.Contains(err.Error(), "secret/path") {
		t.Fatalf("error = %q: want Slack's reason and no URL", err)
	}
}

func TestOutboundRetriesA429AndHonorsTheContextDeadline(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	s, _ := NewSlack(SlackConfig{WebhookURL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.Send(ctx, samplePayload("run_aborted"))
	if err == nil || time.Since(start) > 2*time.Second || n.Load() != 1 {
		t.Fatalf("err %v after %v and %d requests: a wait past the deadline must be skipped", err, time.Since(start), n.Load())
	}
}

func TestOutboundTransportErrorNamesHostNotURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/services/T0/B0/xoxb-secret"
	srv.Close() // connection refused
	s, _ := NewSlack(SlackConfig{WebhookURL: url, Attempts: 1})
	err := s.Send(context.Background(), samplePayload("run_aborted"))
	if err == nil || strings.Contains(err.Error(), "xoxb-secret") || strings.Contains(err.Error(), "/services") {
		t.Fatalf("error %v must not include the URL path", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error %v should name the host", err)
	}
}

func TestOutboundTimesOutASlowThirdParty(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	s, _ := NewSlack(SlackConfig{WebhookURL: srv.URL, Attempts: 1, Timeout: 50 * time.Millisecond})
	start := time.Now()
	if err := s.Send(context.Background(), samplePayload("run_aborted")); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestOutboundDoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	s, _ := NewSlack(SlackConfig{WebhookURL: srv.URL, Backoff: time.Millisecond})
	if err := s.Send(context.Background(), samplePayload("run_aborted")); err == nil || elsewhere.Load() != 0 {
		t.Fatalf("err %v, %d requests followed the redirect", err, elsewhere.Load())
	}
}

func TestSinkURLsMustBeHTTPSOrLoopback(t *testing.T) {
	for _, u := range []string{"http://hooks.slack.com/services/x", "ftp://x", "not a url", "", "/relative"} {
		if _, err := NewSlack(SlackConfig{WebhookURL: u}); err == nil {
			t.Errorf("slack accepted %q", u)
		}
		if _, err := NewPagerDuty(PagerDutyConfig{RoutingKey: "k", URL: u}); u != "" && err == nil {
			t.Errorf("pagerduty accepted %q", u)
		}
	}
	if _, err := NewSlack(SlackConfig{WebhookURL: "https://hooks.slack.com/services/x"}); err != nil {
		t.Error(err)
	}
	if _, err := NewPagerDuty(PagerDutyConfig{}); err == nil {
		t.Error("pagerduty accepted no routing key")
	}
	if _, err := NewSlack(SlackConfig{WebhookURL: "https://hooks.slack.com/x", ViewURL: "javascript:alert(1)"}); err == nil {
		t.Error("slack accepted a javascript: view URL")
	}
}

func TestSlackButtonOnlyForHTTPLinks(t *testing.T) {
	p := samplePayload("approval_pending")
	bad := "javascript:alert(1)"
	p.View.URL = &bad
	b, _ := json.Marshal(SlackMessage(p, ""))
	if strings.Contains(string(b), "javascript") || strings.Contains(string(b), `"actions"`) {
		t.Fatalf("a non-http link became a button: %s", b)
	}
	good := "https://bide.example.com/runs/run-9"
	p.View.URL = &good
	b, _ = json.Marshal(SlackMessage(p, ""))
	if !strings.Contains(string(b), good) {
		t.Fatalf("link missing: %s", b)
	}
}

func TestSlackMessageStaysWithinSlackLimits(t *testing.T) {
	p := samplePayload("approval_pending")
	p.Summary = strings.Repeat("é", 5000)
	p.RunID = strings.Repeat("r", 5000)
	m := SlackMessage(p, "")
	if len(m.Text) > slackFallback {
		t.Fatalf("fallback text %d bytes", len(m.Text))
	}
	if n := len(m.Blocks[1].Text.Text); n > slackSectionMax {
		t.Fatalf("section %d bytes", n)
	}
	for _, f := range m.Blocks[1].Fields {
		if len(f.Text) > slackFieldMax {
			t.Fatalf("field %d bytes", len(f.Text))
		}
	}
	b, _ := json.Marshal(m)
	if !json.Valid(b) {
		t.Fatal("invalid JSON after clipping multi-byte text")
	}
	for _, f := range m.Blocks[1].Fields {
		if !utf8.ValidString(f.Text) {
			t.Fatal("clipped inside a rune")
		}
	}
}

func TestSlackTitlesCoverEveryKind(t *testing.T) {
	for _, k := range []string{"approval_pending", "outcome_unknown", "run_aborted", "recovery_failing", "notification_overdue", "escalated"} {
		if KindTitle(k) == strings.ReplaceAll(k, "_", " ") {
			t.Errorf("%s has no title", k)
		}
	}
	if KindTitle("brand_new") != "brand new" || KindTitle("") == "" {
		t.Error("unknown kinds should still read")
	}
}

func TestPagerDutySeverityByKind(t *testing.T) {
	want := map[string]string{"outcome_unknown": "critical", "notification_overdue": "critical", "escalated": "critical", "run_aborted": "error", "recovery_failing": "error", "approval_pending": "warning", "future_kind": "warning"}
	for k, w := range want {
		if got := DefaultSeverity(notify.Payload{Kind: k}); got != w {
			t.Errorf("%s: %s, want %s", k, got, w)
		}
	}
}

func TestPagerDutyDedupKeyIsTheEventIDAndStable(t *testing.T) {
	p := samplePayload("outcome_unknown")
	a, b := PagerDutyTrigger("rk", "bide", "critical", p), PagerDutyTrigger("rk", "bide", "critical", p)
	if a.DedupKey != p.ID || a.DedupKey != b.DedupKey || len(a.DedupKey) != 64 {
		t.Fatalf("dedup key %q for id %q", a.DedupKey, p.ID)
	}
	other := samplePayload("run_aborted")
	if PagerDutyTrigger("rk", "bide", "error", other).DedupKey == a.DedupKey {
		t.Fatal("two events share a dedup key")
	}
}

func TestPagerDutyResolveHook(t *testing.T) {
	up := newUpstream(t)
	original := samplePayload("approval_pending")
	pd, err := NewPagerDuty(PagerDutyConfig{
		RoutingKey: "rk", URL: up.URL,
		ResolveTarget: func(p notify.Payload) (string, bool) {
			if p.Kind == "all_clear" {
				return agent.NotifyID(strings.TrimPrefix(p.Key, "clear:")), true
			}
			return "", false
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	clear := notify.Payload{Schema: notify.Schema, ID: agent.NotifyID("clear:" + original.Key), Key: "clear:" + original.Key, Kind: "all_clear", RunID: "run-9", Summary: "Run run-9 is clear."}
	if err := pd.Send(context.Background(), clear); err != nil {
		t.Fatal(err)
	}
	var ev PagerDutyEvent
	if err := json.Unmarshal(up.last(t), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.EventAction != "resolve" || ev.DedupKey != original.ID || ev.Payload != nil {
		t.Fatalf("resolve event = %+v", ev)
	}
	// The resolve and the trigger it ends share a dedup key.
	if trig := PagerDutyTrigger("rk", "bide", "warning", original); trig.DedupKey != ev.DedupKey {
		t.Fatalf("trigger %q and resolve %q differ", trig.DedupKey, ev.DedupKey)
	}
}

func TestPagerDutyDefaultResolveUsesClearsID(t *testing.T) {
	id := agent.NotifyID("orig")
	got, ok := DefaultResolveTarget(notify.Payload{Kind: ResolveKind, Clears: &notify.Clears{ID: id}})
	if !ok || got != id {
		t.Fatalf("resolve target = %q, %v", got, ok)
	}
	if _, ok := DefaultResolveTarget(samplePayload("approval_pending")); ok {
		t.Fatal("a trigger kind was treated as a clear")
	}
	if _, ok := DefaultResolveTarget(notify.Payload{Kind: ResolveKind}); ok {
		t.Fatal("a resolved with no clears detail named an incident")
	}
}

func TestPagerDutyIgnoresAClearThatNamesNoEvent(t *testing.T) {
	up := newUpstream(t)
	pd, _ := NewPagerDuty(PagerDutyConfig{RoutingKey: "rk", URL: up.URL})
	if err := pd.Send(context.Background(), notify.Payload{Kind: ResolveKind, ID: agent.NotifyID("x"), Summary: "over"}); err != nil || up.count() != 0 {
		t.Fatalf("err %v, %d requests: a nameless clear must neither trigger nor resolve", err, up.count())
	}
}

func TestClipKeepsRunesWhole(t *testing.T) {
	for n := 4; n < 12; n++ {
		got := clip(strings.Repeat("é", 20), n)
		if !utf8.ValidString(got) || len(got) > n {
			t.Fatalf("clip to %d: %q (%d bytes)", n, got, len(got))
		}
	}
}

func TestStatusErrorKeepsOnlyAnAllowListedToken(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"slack plain token":  {"no_service", "no_service"},
		"slack json error":   {`{"ok":false,"error":"channel_not_found"}`, "channel_not_found"},
		"pagerduty status":   {`{"status":"invalid event","message":"Event object is invalid: card 4111111111111111","errors":["x"]}`, "invalid event"},
		"free text":          {"Your card 4111111111111111 was refused", ""},
		"uppercase text":     {"Invalid Payload", ""},
		"plain with spaces":  {"invalid payload for acct 42", ""},
		"json free message":  {`{"message":"secret thing"}`, ""},
		"json error is text": {`{"error":"Something SECRET happened at T000/B000"}`, ""},
		"too long token":     {strings.Repeat("a", 80), ""},
		"empty":              {"", ""},
		"digits only":        {"503", ""},
		"leading underscore": {"_hidden", ""},
	}
	for name, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			w.Write([]byte(c.body))
		}))
		s, _ := NewSlack(SlackConfig{WebhookURL: srv.URL, Attempts: 1})
		err := s.Send(context.Background(), samplePayload("run_aborted"))
		srv.Close()
		var se *StatusError
		if !errors.As(err, &se) || se.Code != c.want {
			t.Errorf("%s: err %v, want code %q", name, err, c.want)
			continue
		}
		for _, leak := range []string{"4111", "SECRET", "secret thing", "T000"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: error %q leaks %q", name, err, leak)
			}
		}
	}
}

// The relay's sinks are called from the handler's goroutines at once: they hold no per-call state.
func TestSinksAreSafeForConcurrentSends(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1) }))
	defer srv.Close()
	slack, err := NewSlack(SlackConfig{WebhookURL: srv.URL, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	pd, err := NewPagerDuty(PagerDutyConfig{RoutingKey: "rk", URL: srv.URL, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := samplePayload("outcome_unknown")
			if err := slack.Send(context.Background(), p); err != nil {
				t.Error(err)
			}
			if err := pd.Send(context.Background(), p); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n.Load() != 32 {
		t.Fatalf("%d requests, want 32", n.Load())
	}
}

// The relays do not render an approval's reasons (approval.why): they build their messages from named fields
// of the payload, so a source or rule name a host declared (and an attacker may have influenced) never reaches a
// Slack message, a mention, or a PagerDuty summary or custom detail. The reasons travel in the webhook's JSON
// only. If a relay ever renders them, this test fails until the names are escaped for that target.
func TestRelaysDoNotRenderTheReasons(t *testing.T) {
	amt := int64(600000)
	p := samplePayload("approval_pending")
	p.Approval = &notify.Approval{ToolUseID: "c3", Tool: "write", Policy: notify.Policy{Need: 2, Of: 2}, Approvers: []string{"a", "b"},
		Why: []agent.GateReason{
			{Kind: "taint", Source: "<!channel> <@U123> <https://evil.example|click> *bold* `x`", Ref: "tool:read/c1", Pos: 1},
			{Kind: "cap", Rule: "<!here> r", Action: "gate", Why: "over", Currency: "USD", Amount: &amt, Limit: 1},
			{Kind: "conflict"},
		}}
	slack, _ := json.Marshal(SlackMessage(p, ""))
	pd, _ := json.Marshal(PagerDutyTrigger("key", "bide", "warning", p))
	for name, out := range map[string]string{"slack": string(slack), "pagerduty": string(pd)} {
		for _, leaked := range []string{"!channel", "@U123", "evil.example", "!here", "*bold*", "conflict", "600000", "untrusted content"} {
			if strings.Contains(out, leaked) {
				t.Errorf("%s renders the reasons (%q is in the message): %s", name, leaked, out)
			}
		}
	}
}
