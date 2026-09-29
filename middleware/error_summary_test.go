package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bide-ai/bide/agent"
)

// ErrorSummary names what failed (the typed signal, the provider status, the condition or
// category the error wraps) and nothing the error's text carries.
func TestErrorSummary(t *testing.T) {
	const secret = "SECRET-CONTENT"
	wrap := func(err error) error { return fmt.Errorf("while handling %s: %w", secret, err) }
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{wrap(agent.ErrTool), "tool"},
		{wrap(agent.ErrModel), "model"},
		{wrap(agent.ErrStorage), "storage"},
		{wrap(agent.ErrConfig), "config"},
		{wrap(agent.ErrProtocol), "protocol"},
		{wrap(agent.ErrBudget), "budget"},
		{wrap(agent.ErrUnknownTool), "unknown tool: tool"},
		{wrap(agent.ErrToolArgs), "invalid tool arguments: tool"},
		{wrap(agent.ErrMaxTurns), "max turns exceeded: budget"},
		{wrap(agent.ErrToolUseIDReused), "tool-use id missing or reused: protocol (model)"},
		{wrap(context.Canceled), "context canceled"},
		{wrap(context.DeadlineExceeded), "context deadline exceeded"},
		{wrap(&agent.APIError{StatusCode: 401, Body: secret, Message: secret, Code: "invalid_api_key", Err: agent.ErrModel}),
			"api error: status 401 (invalid_api_key): model"},
		{wrap(&agent.APIError{StatusCode: 500, Body: secret, Err: agent.ErrModel}), "api error: status 500: model"},
		{&agent.APIError{StatusCode: 502, Body: secret}, "api error: status 502"},
		{wrap(&agent.RateLimited{RetryAfter: 2 * time.Second, Message: secret, Err: agent.ErrModel}),
			"rate limited (retry after 2s): model"},
		{wrap(&agent.PendingApproval{RunID: secret, ToolName: secret}), "pending approval"},
		{wrap(&agent.ResumeHalt{RunID: secret, ToolName: secret}), "resume halted"},
		{wrap(&agent.SagaAborted{RunID: secret, Cause: errors.New(secret)}), "saga aborted"},
		{&url.Error{Op: "Get", URL: "https://x.test/?key=" + secret, Err: errors.New(secret)}, "unclassified error (*url.Error)"},
		{errors.New(secret), "unclassified error (*errors.errorString)"},
	} {
		got := ErrorSummary(c.err)
		if got != c.want || strings.Contains(got, secret) {
			t.Errorf("ErrorSummary(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	for _, long := range []string{strings.Repeat("x", 200), strings.Repeat("\u20ac", 200)} {
		got := ErrorSummary(&agent.APIError{StatusCode: 400, Type: long, Err: agent.ErrModel})
		if len([]rune(got)) > 120 || !utf8.ValidString(got) {
			t.Errorf("ErrorSummary kept a %d-character provider error type, or cut it mid-character: %q", len([]rune(got)), got)
		}
	}
}
