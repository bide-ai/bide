//go:build !bide_audit_only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/notify"
)

// shellFields splits a command the way the payload's quoting expects: spaces separate words, and
// single quotes group one.
func shellFields(s string) []string {
	var out []string
	var cur strings.Builder
	in, has := false, false
	for _, r := range s {
		switch {
		case r == '\'':
			in, has = !in, true
		case r == ' ' && !in:
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if has {
		out = append(out, cur.String())
	}
	return out
}

// TestNotificationViewCommandParses: the view command a webhook puts in a payload is accepted by
// the real bide view flag set. It names a journal that does not exist, so a command that parses
// fails opening it (exit 1), where a command missing its store is a usage error (exit 2).
func TestNotificationViewCommandParses(t *testing.T) {
	for _, db := range []string{"", "/nonexistent/run.db", "/nonexistent/my dir/run.db"} {
		var opts []notify.Option
		if db != "" {
			opts = append(opts, notify.WithViewDB(db))
		}
		cmd := deliveredViewCommand(t, opts...)
		words := shellFields(cmd)
		if len(words) < 2 || words[0] != "bide" || words[1] != "view" {
			t.Fatalf("command %q does not start with bide view", cmd)
		}
		var stdout, stderr bytes.Buffer
		code := runview.Main(words[2:], &stdout, &stderr, stores)
		if code != 1 || strings.Contains(stderr.String(), "Usage") || strings.Contains(stderr.String(), "give exactly one") {
			t.Errorf("%q: exit %d, want 1 (opened and failed on the missing file)\n%s", cmd, code, stderr.String())
		}
	}
}

// deliveredViewCommand sends one notification through a webhook and returns the view command in
// the body the receiver got.
func deliveredViewCommand(t *testing.T, opts ...notify.Option) string {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
	}))
	t.Cleanup(srv.Close)
	hook, err := notify.NewWebhook(srv.URL, append([]notify.Option{notify.WithSecrets("test-secret", "")}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	n := agent.Notification{Kind: agent.NotificationRunAborted, RunID: "order-42", Reason: "x", Attempt: 1}
	if err := hook.Notify(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	var p notify.Payload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	return p.View.Command
}
