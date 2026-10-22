package middleware_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/middleware"
	"github.com/bide-ai/bide/model/provider"
)

func TestRetryable_Classifies(t *testing.T) {
	sc := provider.NewSSEScanner(strings.NewReader("data: " + strings.Repeat("A", provider.MaxSSELine) + "\n"))
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
		"stream cut":          {provider.SSEReadError("x", io.ErrUnexpectedEOF), true},
		"reused tool-use id":  {fmt.Errorf("x: %w", agent.ErrToolUseIDReused), true},
		"invalid tool-use id": {fmt.Errorf("x: %w", agent.ErrToolUseIDInvalid), true},
		"config":              {fmt.Errorf("openai: strict schema: %w", agent.ErrConfig), false},
		"config under model":  {fmt.Errorf("generate: %w (%w)", fmt.Errorf("x: %w", agent.ErrConfig), agent.ErrModel), false},
		"quota exhausted 429": {&agent.APIError{StatusCode: 429, Err: fmt.Errorf("openai: %w", agent.ErrQuotaExhausted)}, false},
		"quota exhausted 5xx": {&agent.APIError{StatusCode: 503, Err: fmt.Errorf("x: %w", agent.ErrQuotaExhausted)}, false},
		"line too large":      {provider.SSEReadError("x", sc.Err()), false},
		"truncated tool args": {fmt.Errorf("anthropic: %w", agent.ErrTruncatedToolArgs), false},
		"stream protocol":     {fmt.Errorf("x: %w", agent.ErrStreamProtocol), true},
	} {
		if got := middleware.Retryable(tc.err); got != tc.want {
			t.Errorf("%s: Retryable(%v) = %v, want %v", name, tc.err, got, tc.want)
		}
	}
}

// badFirstIDModel answers its first call with a tool call whose ID holds '#', and every later one
// from ok.
type badFirstIDModel struct {
	calls atomic.Int32
	ok    *agenttest.ScriptedModel
}

func (m *badFirstIDModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	if m.calls.Add(1) == 1 {
		return agenttest.NewScriptedModel(agenttest.ToolTurn("c1#taint", "lookup", `{}`)).Stream(ctx, req)
	}
	return m.ok.Stream(ctx, req)
}

// A model that sends a tool-use ID with a '#' fails the turn with agent.ErrToolUseIDInvalid, below
// the middleware, so Retry (with Retryable, and by default) asks the model again and the run
// completes on the retry's valid ID. The rejected turn is not journaled.
func TestRetry_RetriesAnInvalidToolUseID(t *testing.T) {
	tool := agent.MustFunc("lookup", "look up", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
	for name, opts := range map[string][]middleware.RetryOption{
		"Retryable": {middleware.WithRetryIf(middleware.Retryable)},
		"default":   nil,
	} {
		journal := agenttest.MemJournal()
		// The scripted model picks its turn by the conversation, so it would repeat the refused
		// turn on a retry: the first call alone is the refused one.
		model := &badFirstIDModel{ok: agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "lookup", `{}`), agenttest.TextTurn("done"))}
		a, err := agent.New(model, journal, agent.WithTools(tool), agent.WithMiddleware(middleware.Retry(1, opts...)))
		if err != nil {
			t.Fatal(err)
		}
		res, err := a.Run(context.Background(), "r", agent.UserText("go"))
		if err != nil || res.Message.Text() != "done" {
			t.Fatalf("%s: Run = %v, %v; want done", name, res.Message.Text(), err)
		}
		recs, _ := journal.History(context.Background(), "r")
		for _, r := range recs {
			if r.Message == nil {
				continue
			}
			for _, p := range r.Message.Parts {
				if tu, ok := p.(agent.ToolUse); ok && strings.Contains(tu.ID, "#") {
					t.Fatalf("%s: the refused turn was journaled: %+v", name, tu)
				}
			}
		}
		// Without a retry the same turn fails the run with the sentinel.
		a, _ = agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1#taint", "lookup", `{}`), agenttest.TextTurn("done")), agenttest.MemJournal(), agent.WithTools(tool))
		if _, err := a.Run(context.Background(), "r", agent.UserText("go")); !errors.Is(err, agent.ErrToolUseIDInvalid) {
			t.Fatalf("%s: without Retry: %v, want ErrToolUseIDInvalid", name, err)
		}
	}
}
