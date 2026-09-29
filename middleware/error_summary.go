package middleware

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// ErrorSummary describes err by what failed, never by what its text says: the control-flow
// signal it is (pending approval, resume halted, saga aborted), the provider's HTTP status and
// error type or code (from *agent.APIError) or a rate limit, and the condition or category
// sentinel it wraps. An error's text often carries content: a provider error body that echoes
// the prompt, a tool error that embeds the call's arguments, a URL with a credential in its
// query. The summary carries none of it, so it is safe to put in a log line, a span, or a
// metric label when content capture is off. An error that wraps none of these is summarized by
// its Go type, as "unclassified error (*url.Error)". A nil error summarizes to "".
//
//	api error: status 400 (invalid_request_error): model
//	rate limited (retry after 2s): model
//	unknown tool: tool
func ErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	var (
		apiErr   *agent.APIError
		limited  *agent.RateLimited
		pending  *agent.PendingApproval
		halt     *agent.ResumeHalt
		aborted  *agent.SagaAborted
		signal   string
		sentinel = errorClass(err)
	)
	switch {
	case errors.As(err, &pending):
		return "pending approval"
	case errors.As(err, &halt):
		return "resume halted"
	case errors.As(err, &aborted):
		return "saga aborted"
	case errors.As(err, &apiErr):
		signal = fmt.Sprintf("api error: status %d", apiErr.StatusCode)
		if kind := cmp.Or(apiErr.Code, apiErr.Type); kind != "" {
			signal += " (" + clip(kind) + ")"
		}
	case errors.As(err, &limited):
		signal = "rate limited"
		if limited.RetryAfter > 0 {
			signal += fmt.Sprintf(" (retry after %s)", limited.RetryAfter)
		}
	}
	switch {
	case signal != "" && sentinel != "":
		return signal + ": " + sentinel
	case signal != "":
		return signal
	case sentinel != "":
		return sentinel
	}
	return fmt.Sprintf("unclassified error (%T)", err)
}

// conditions are the condition sentinels, most specific first. Each one's text is fixed, names
// the condition, and ends with the category it wraps.
var conditions = []error{
	agent.ErrUnknownTool, agent.ErrToolArgs, agent.ErrToolReinvoked, agent.ErrNoRecordedOutput,
	agent.ErrIncompleteResponse, agent.ErrTruncatedToolArgs, agent.ErrToolUseIDReused,
	agent.ErrQuotaExhausted, agent.ErrResponseTooLarge, agent.ErrBudgetExceeded, agent.ErrMaxTurns,
	agent.ErrInvalidApproval, agent.ErrAlreadyDecided,
}

// categories are the category sentinels, and the context errors, whose texts are fixed.
var categories = []error{
	context.Canceled, context.DeadlineExceeded,
	agent.ErrModel, agent.ErrTool, agent.ErrStorage, agent.ErrConfig, agent.ErrProtocol, agent.ErrBudget,
}

// errorClass returns the text of the first condition, or else category, sentinel err wraps, or
// "" if it wraps none.
func errorClass(err error) string {
	for _, s := range conditions {
		if errors.Is(err, s) {
			return s.Error()
		}
	}
	for _, s := range categories {
		if errors.Is(err, s) {
			return s.Error()
		}
	}
	return ""
}

// clip bounds a provider-supplied error type or code, which is an identifier and has no reason
// to be long.
func clip(s string) string {
	const max = 64
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}
