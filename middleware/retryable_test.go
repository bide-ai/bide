package middleware_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

func TestRetryable_Classifies(t *testing.T) {
	sc := agent.NewSSEScanner(strings.NewReader("data: " + strings.Repeat("A", agent.MaxSSELine) + "\n"))
	for sc.Scan() {
	}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"rate limited":        {&agent.RateLimited{Err: agent.ErrModel}, true},
		"5xx":                 {&agent.APIError{StatusCode: 503, Err: agent.ErrModel}, true},
		"408":                 {&agent.APIError{StatusCode: 408, Err: agent.ErrModel}, true},
		"4xx":                 {&agent.APIError{StatusCode: 400, Err: agent.ErrModel}, false},
		"timeout":             {context.DeadlineExceeded, true},
		"cancelled":           {context.Canceled, false},
		"network":             {io.ErrUnexpectedEOF, true},
		"stream cut":          {agent.SSEReadError("x", io.ErrUnexpectedEOF), true},
		"reused tool-use id":  {fmt.Errorf("x: %w", agent.ErrToolUseIDReused), true},
		"config":              {fmt.Errorf("openai: strict schema: %w", agent.ErrConfig), false},
		"config under model":  {fmt.Errorf("generate: %w (%w)", fmt.Errorf("x: %w", agent.ErrConfig), agent.ErrModel), false},
		"quota exhausted 429": {&agent.APIError{StatusCode: 429, Err: fmt.Errorf("openai: %w", agent.ErrQuotaExhausted)}, false},
		"quota exhausted 5xx": {&agent.APIError{StatusCode: 503, Err: fmt.Errorf("x: %w", agent.ErrQuotaExhausted)}, false},
		"line too large":      {agent.SSEReadError("x", sc.Err()), false},
		"truncated tool args": {fmt.Errorf("anthropic: %w", agent.ErrTruncatedToolArgs), false},
		"stream protocol":     {fmt.Errorf("x: %w", agent.ErrStreamProtocol), true},
	} {
		if got := middleware.Retryable(tc.err); got != tc.want {
			t.Errorf("%s: Retryable(%v) = %v, want %v", name, tc.err, got, tc.want)
		}
	}
}
