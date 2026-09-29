package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/bide-ai/bide/agent"
)

// spanTexts returns every string a span carries: its status description and each event
// attribute (RecordError writes the error text as the exception.message event attribute).
func spanTexts(s sdktrace.ReadOnlySpan) []string {
	texts := []string{s.Status().Description}
	for _, kv := range s.Attributes() {
		texts = append(texts, kv.Value.Emit())
	}
	for _, e := range s.Events() {
		for _, kv := range e.Attributes {
			texts = append(texts, kv.Value.Emit())
		}
	}
	return texts
}

// failingSpans runs a failing model call, a failing tool call, and a failing top-level run
// through the instrumentation, each error carrying content, and returns the ended spans.
func failingSpans(t *testing.T) []sdktrace.ReadOnlySpan {
	t.Helper()
	sr, tp := recorder()
	tracer := tp.Tracer("t")
	mh := Model(tracer)(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
		return agent.Message{}, agent.Usage{}, &agent.APIError{StatusCode: 400, Type: "invalid_request_error",
			Body: `{"error":{"message":"Invalid 'messages[0].content': 'PATIENT-SSN-123-45-6789'"}}`,
			Err:  fmt.Errorf("openai (%w)", agent.ErrModel)}
	})
	_, _, _ = mh(context.Background(), agent.Request{Messages: []agent.Message{agent.UserText("PATIENT-SSN-123-45-6789")}})
	th := Tool(tracer)(func(_ context.Context, tu agent.ToolUse) (json.RawMessage, error) {
		return nil, fmt.Errorf("charge failed for args %s: %w", tu.Args, agent.ErrTool)
	})
	_, _ = th(context.Background(), agent.ToolUse{ID: "t1", Name: "charge", Args: json.RawMessage(`{"card":"4111111111111111"}`)})
	_, end := Invoke(context.Background(), tracer, "a")
	end(errors.New("run failed on input CUSTOMER-NOTE-SECRET"))
	return sr.Ended()
}

var errorContent = []string{"PATIENT-SSN-123-45-6789", "4111111111111111", "CUSTOMER-NOTE-SECRET"}

// With content capture off, an error must not copy content into a span: a provider error body
// that echoes the prompt, a tool error that embeds its arguments, and a run error are recorded
// by category and status only.
func TestErrorContentNotCapturedWhenCaptureOff(t *testing.T) {
	t.Setenv(captureEnv, "")
	want := map[string]string{
		"chat":                "api error: status 400 (invalid_request_error): model",
		"execute_tool charge": "tool",
		"invoke_agent a":      "unclassified error (*errors.errorString)",
	}
	for _, s := range failingSpans(t) {
		for _, x := range spanTexts(s) {
			for _, c := range errorContent {
				if strings.Contains(x, c) {
					t.Errorf("span %q carries error content with capture off: %s", s.Name(), x)
				}
			}
		}
		if got := s.Status().Description; got != want[s.Name()] {
			t.Errorf("span %q status = %q, want %q", s.Name(), got, want[s.Name()])
		}
		if len(s.Events()) != 0 {
			t.Errorf("span %q records %d events (an exception event carries the error text), want none", s.Name(), len(s.Events()))
		}
	}
}

// With capture on, the operator asked for content: the error text is recorded as before.
func TestErrorContentCapturedWhenCaptureOn(t *testing.T) {
	t.Setenv(captureEnv, "true")
	found := map[string]bool{}
	for _, s := range failingSpans(t) {
		if len(s.Events()) != 1 || s.Events()[0].Name != "exception" {
			t.Errorf("span %q events = %v, want one exception event", s.Name(), s.Events())
		}
		for _, x := range spanTexts(s) {
			for _, c := range errorContent {
				if strings.Contains(x, c) {
					found[c] = true
				}
			}
		}
	}
	for _, c := range errorContent {
		if !found[c] {
			t.Errorf("error content %q not recorded with capture on", c)
		}
	}
}

// A model or tool call that panics must still end its span marked failed. The agent recovers a
// panic inside a sub-agent and reports it as the sub-agent's tool error, so the run goes on and
// the panicked call's span is exported: left unmarked, it reads as a call that succeeded. The
// panic value can carry content, so the status names the panic and nothing more.
func TestPanicMarksSpanFailed(t *testing.T) {
	for _, capture := range []string{"", "true"} {
		t.Setenv(captureEnv, capture)
		sr, tp := recorder()
		tracer := tp.Tracer("t")
		mustPanic := func(call func()) {
			t.Helper()
			defer func() {
				if r := recover(); r != "PATIENT-SSN-123-45-6789" {
					t.Errorf("recovered %v, want the handler's own panic value re-raised", r)
				}
			}()
			call()
		}
		mustPanic(func() {
			h := Model(tracer)(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
				panic("PATIENT-SSN-123-45-6789")
			})
			_, _, _ = h(context.Background(), agent.Request{})
		})
		mustPanic(func() {
			h := Tool(tracer)(func(context.Context, agent.ToolUse) (json.RawMessage, error) {
				panic("PATIENT-SSN-123-45-6789")
			})
			_, _ = h(context.Background(), agent.ToolUse{ID: "t1", Name: "charge"})
		})
		spans := sr.Ended()
		if len(spans) != 2 {
			t.Fatalf("capture=%q: %d spans ended, want 2", capture, len(spans))
		}
		for _, s := range spans {
			if st := s.Status(); st.Code != codes.Error || st.Description != "panic" {
				t.Errorf("capture=%q: span %q status = %v %q, want Error \"panic\"", capture, s.Name(), st.Code, st.Description)
			}
			for _, x := range spanTexts(s) {
				if strings.Contains(x, "PATIENT-SSN-123-45-6789") {
					t.Errorf("capture=%q: span %q carries the panic value: %s", capture, s.Name(), x)
				}
			}
		}
	}
}
