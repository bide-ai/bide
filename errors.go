package agent

import (
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
	ErrUnknownTool       = fmt.Errorf("unknown tool: %w", ErrTool)
	ErrToolArgs          = fmt.Errorf("invalid tool arguments: %w", ErrTool)
	ErrNoRecordedOutput  = fmt.Errorf("no recorded model output: %w", ErrModel)
	ErrTruncatedToolArgs = fmt.Errorf("truncated tool-call arguments: %w", ErrProtocol)
	ErrBudgetExceeded    = fmt.Errorf("budget exceeded: %w", ErrBudget)
	ErrMaxTurns          = fmt.Errorf("max turns exceeded: %w", ErrBudget)
)

// RateLimited is returned by a provider adapter when it receives HTTP 429. It
// carries an optional RetryAfter hint from the Retry-After response header (0
// means no hint was provided). It wraps the caller-supplied Err (typically
// wrapping ErrModel) so errors.Is(err, ErrModel) holds.
type RateLimited struct {
	RetryAfter time.Duration
	Err        error
}

func (e *RateLimited) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("rate limited (retry after %s): %v", e.RetryAfter, e.Err)
	}
	return fmt.Sprintf("rate limited: %v", e.Err)
}

func (e *RateLimited) Unwrap() error { return e.Err }
