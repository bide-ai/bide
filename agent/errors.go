package agent

import (
	"cmp"
	"errors"
	"fmt"
	"time"
)

// The toolkit classifies failures with sentinel errors matched by errors.Is — the
// standard-library idiom (io.EOF, sql.ErrNoRows). There are two tiers:
//
//   - Category sentinels (ErrModel, ErrTool, ErrStorage, ErrConfig, ErrProtocol,
//     ErrBudget) — the coarse class of failure, for a broad branch or a metric label.
//   - Condition sentinels (ErrUnknownTool, ErrToolArgs, …) — a specific cause. Each
//     wraps its category, so errors.Is(err, ErrUnknownTool) and errors.Is(err, ErrTool)
//     both hold.
//
// Every error the toolkit returns wraps exactly one category (and usually the
// underlying cause too, via a second %w). Match the class you care about:
//
//	if errors.Is(err, agent.ErrModel)   { retryLater() }   // any model-provider fault
//	if errors.Is(err, agent.ErrUnknownTool) { … }          // one specific condition
//
// The control-flow signals are richer than a category, so they stay concrete types
// matched with errors.As, not sentinels: *PendingApproval (human approval needed),
// *ResumeHalt (unsafe to resume), *SagaAborted (transaction rolled back). context
// cancellation surfaces as the usual context.Canceled / context.DeadlineExceeded.
var (
	// ErrConfig is a programmer-facing misuse or misconfiguration, not a runtime fault.
	ErrConfig = errors.New("config")
	// ErrModel is a model-provider call that failed: HTTP status, decode, stream error.
	ErrModel = errors.New("model")
	// ErrTool is a tool that failed or was misused (unknown tool, bad arguments).
	ErrTool = errors.New("tool")
	// ErrStorage is a durable-store I/O failure: marshal, insert, query, history.
	ErrStorage = errors.New("storage")
	// ErrProtocol is malformed wire or journal data (bad log entry, unknown content part).
	ErrProtocol = errors.New("protocol")
	// ErrBudget is a middleware limit tripping (e.g. a token budget).
	ErrBudget = errors.New("budget")
)

// Condition sentinels — specific causes, each wrapping its category.
var (
	ErrUnknownTool = fmt.Errorf("unknown tool: %w", ErrTool)
	ErrToolArgs    = fmt.Errorf("invalid tool arguments: %w", ErrTool)
	// ErrToolReinvoked is a tool middleware calling a tool that is not retry-safe a second
	// time for the same tool call. The agent refuses and does not run the tool again.
	ErrToolReinvoked    = fmt.Errorf("tool invoked again for one call: %w", ErrConfig)
	ErrNoRecordedOutput = fmt.Errorf("no recorded model output: %w", ErrModel)
	// ErrIncompleteResponse is a model stream that ended without a Finish event: the
	// response stopped partway through a turn, so what arrived is not the model's answer.
	ErrIncompleteResponse = fmt.Errorf("model response ended before the turn finished: %w", ErrModel)
	ErrTruncatedToolArgs  = fmt.Errorf("truncated tool-call arguments: %w", ErrProtocol)
	// ErrToolUseIDReused is a model turn whose tool call has no ID, repeats an ID from an
	// earlier turn of the conversation, or repeats one within the turn. The loop keys each
	// call's result and journal step by its ID, so a reused ID would pass the call off as
	// already done. It wraps ErrModel as well as ErrProtocol: the fault is in the model's
	// output, the turn is not journaled, and a fresh attempt can issue a valid turn, so a
	// retry middleware treats it as retryable.
	ErrToolUseIDReused = fmt.Errorf("tool-use id missing or reused: %w (%w)", ErrProtocol, ErrModel)
	// ErrQuotaExhausted is a provider refusing a call because the account's quota or credit
	// is used up (OpenAI insufficient_quota, Anthropic billing_error, HTTP 402, a Gemini daily
	// quota), as opposed to a momentary rate limit. Waiting a few seconds does not lift it, so
	// middleware.Retryable does not retry it. It arrives as an *APIError.
	ErrQuotaExhausted = fmt.Errorf("provider quota or credit exhausted: %w", ErrModel)
	// ErrResponseTooLarge is a streamed response with a line longer than MaxSSELine. The same
	// request would produce it again, so middleware.Retryable does not retry it.
	ErrResponseTooLarge = fmt.Errorf("model response line too large: %w", ErrModel)
	ErrBudgetExceeded   = fmt.Errorf("budget exceeded: %w", ErrBudget)
	ErrMaxTurns         = fmt.Errorf("max turns exceeded: %w", ErrBudget)
	// ErrInvalidApproval is an approver decision rejected at submission by ApproveAs's
	// WithDecisionCheck: no such tool call, an unknown approver, or a signature that does not
	// verify for this exact call.
	ErrInvalidApproval = fmt.Errorf("invalid approval: %w", ErrConfig)
	// ErrAlreadyDecided is a decision submitted (with WithDecisionCheck) by an approver whose
	// earlier valid decision on the same call already counts; the new one would be ignored.
	ErrAlreadyDecided = fmt.Errorf("approver already decided: %w", ErrConfig)
)

// RateLimited is returned by a provider adapter when it is rate limited (HTTP 429 that is not an
// exhausted quota; see ClassifyHTTPError). It carries an optional RetryAfter hint from the
// Retry-After response header or the provider's own retry delay (0 means no hint was provided),
// and the provider's error message. It wraps the caller-supplied Err (typically wrapping
// ErrModel) so errors.Is(err, ErrModel) holds.
type RateLimited struct {
	RetryAfter time.Duration
	Message    string // the provider's error message, if it sent one
	Err        error
}

func (e *RateLimited) Error() string {
	s := "rate limited"
	if e.RetryAfter > 0 {
		s += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	if e.Message != "" {
		s += ": " + truncate(e.Message)
	}
	return fmt.Sprintf("%s: %v", s, e.Err)
}

func (e *RateLimited) Unwrap() error { return e.Err }

// APIError is returned by a provider adapter for a non-2xx HTTP response that is not a rate
// limit (which uses RateLimited), and for an exhausted quota whatever its status (wrapping
// ErrQuotaExhausted). It carries the StatusCode so a retry classifier can tell a transient
// failure (5xx, 408) from a terminal one (most 4xx: auth, validation), the provider's error
// fields when the body held its error object, and the Body (cut to 8KB) for diagnostics. It
// wraps Err (typically ErrModel) so errors.Is(err, ErrModel) holds. StatusCode is 0 for an
// error the provider sent partway through a stream without one.
type APIError struct {
	StatusCode int
	Body       string
	Message    string // the provider's error message
	Type       string // the provider's error type (OpenAI, Anthropic) or status (Gemini)
	Code       string // the provider's error code (OpenAI)
	Err        error
}

func (e *APIError) Error() string {
	detail := truncate(e.Body)
	if e.Message != "" {
		detail = truncate(e.Message)
		if kind := cmp.Or(e.Code, e.Type); kind != "" {
			detail += " (" + kind + ")"
		}
	}
	return fmt.Sprintf("api error: status %d: %s: %v", e.StatusCode, detail, e.Err)
}

func (e *APIError) Unwrap() error { return e.Err }
