package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// ModelHandler answers one model call: it returns the response the turn records. Middleware wraps
// it; the innermost handler, which the agent (or CallModel) supplies, sends each request to
// call.Model. On error the response carries at most the usage the failed request reported.
type ModelHandler func(ctx context.Context, call ModelCall) (ModelResponse, error)

// Middleware wraps a ModelHandler: the net/http-style func(Handler) Handler chain, at the
// semantic layer (it sees messages, tool calls and token usage, not bytes). Batteries live in the
// middleware package (Retry, Hedge, RateLimit, Cost). A middleware passes on the call it received,
// or a copy of it with fields changed (c := call; c.Model = backup): the call carries the agent's
// per-turn state in unexported fields, and the agent's model handler refuses a ModelCall that was
// built from scratch rather than derived from the one the middleware received (ErrConfig).
type Middleware func(next ModelHandler) ModelHandler

// ModelCall is one model call as it passes through the middleware chain. It is a value: a
// middleware changes a copy and passes that on, so what one middleware changes is seen only by
// the handlers it calls, never by its caller or by a sibling (a Hedge's other targets).
//
// Request.Messages and Request.Tools arrive at every handler slices.Clip'ped, so a handler that
// appends to them gets a new backing array and never writes into a slice another handler holds.
// The elements are shared: replace an element through a copy of the slice rather than assign into
// it.
type ModelCall struct {
	// Request is what the call asks of the model.
	Request Request
	// Model is where the agent's model handler sends the call's requests. A middleware may
	// retarget a copy of the call (Hedge sends its backups this way).
	Model Model
	// RunID is the run the call belongs to, and Turn the model turn's sequence number within it
	// (0 for the first). Both are zero for a call made through CallModel.
	RunID string
	Turn  int

	turn    *turnState      // the per-turn state the agent's model handler uses and checks
	hooks   []ModelCallHook // added through AddHook, outermost first
	attempt int             // the request's number, set on the call a hook receives
	layer   int             // how many WithRetrieval layers the call has passed through
}

// AddHook returns a copy of c with h appended to its hooks, which the agent's model handler runs
// around every request it sends for the returned call and every call derived from it. Hooks are
// append-only: a middleware can add one, never remove or reorder those added outside it, and the
// agent's own spend accounting is not a hook at all, so no middleware can hide a request from the
// run's budget or Result.Spend.
func (c ModelCall) AddHook(h ModelCallHook) ModelCall {
	c.hooks = append(slices.Clip(c.hooks), h)
	return c
}

// Attempt is the number of the request a hook is observing: 1 for the first request the turn
// sends, 2 for the next, and so on, counted across the whole turn, so the requests of every
// Retry attempt and every Hedge target are numbered apart. It is 0 on a call that has not reached
// the agent's model handler (as a middleware receives it).
func (c ModelCall) Attempt() int { return c.attempt }

// ModelCallHook runs around each request the agent's model handler sends: every retried attempt
// and every hedged target, wherever the middleware that added it (ModelCall.AddHook) sits in the
// chain. Middleware whose effect belongs to requests actually sent, rather than to calls through
// it, uses one: RateLimit waits for a token in Before, Cost counts spend in After.
//
// The hooks of a call run in the order they were added, so a middleware nearer the outside of the
// chain runs its Before first. Each Before runs once per request; a hook whose Before returned nil
// gets exactly one After for that request.
type ModelCallHook struct {
	// Before runs before the request is sent, with the request's context and the call as it
	// reached the model handler (Attempt set). An error fails the request without sending it:
	// the hooks after it do not run, and the hooks before it get their After with that error.
	Before func(ctx context.Context, call ModelCall) error
	// After runs once the request has ended, successfully or not.
	After func(ctx context.Context, call ModelCall, a ModelAttempt)
}

// ModelAttempt is how one request ended, as an After hook sees it.
type ModelAttempt struct {
	// Response is what the request returned. When Err is set, only its Usage is meaningful: the
	// usage the request reported before it failed, which the provider may still bill.
	Response ModelResponse
	// Discarded is usage the request's model reported as billed for responses it threw away (see
	// Finish.Discarded): zero from a live adapter, the recorded discarded spend from Replay.
	Discarded Usage
	// Err is the request's error, or the agent's rejection of its response (a reused tool-use
	// id), or the error of a Before hook that kept it from being sent.
	Err error
}

// ModelResponse is the answer to a model call: the assistant message, the usage of the request
// that produced it, and why the turn ended. Finish is never empty in a response the agent
// records: an empty reason, which a Model may send when it does not know one, counts as
// FinishStop (see Finish). The loop decides whether to run tools from the message's tool calls,
// never from Finish (an OpenAI forced tool_choice reports stop alongside tool calls).
type ModelResponse struct {
	Message   Message
	Usage     Usage
	Finish    FinishReason
	RawFinish string // the provider's own finish reason, as it sent it; empty if it sent none

	origin responseOrigin // the request that produced the response; zero for one a middleware built
}

// responseOrigin identifies the request that produced a response: its number within the turn
// (never 0), the messages and tools it sent and the model it went to, for the journal.
type responseOrigin struct {
	attempt   int
	msgs      []Message
	tools     []Tool
	info      ModelInfo
	described bool
}

// CallModel sends one model call outside an agent, through mw (first = outermost) and the same
// model handler an agent uses: hooks run around every request, Messages and Tools arrive clipped,
// and the response is checked as an agent checks it (usage, finish reason, tool-use ids). There is
// no journal, no stream and no run, so RunID and Turn are zero. It returns ErrConfig when m is nil.
func CallModel(ctx context.Context, m Model, req Request, mw ...Middleware) (ModelResponse, error) {
	if m == nil {
		return ModelResponse{}, fmt.Errorf("agent: CallModel with a nil Model: %w", ErrConfig)
	}
	return newModelChain(mw).call(ctx, ModelCall{Request: req, Model: m}, &turnState{meter: &spendMeter{}})
}

// journal returns what a model record journals about resp beside its message and usage: the model
// that answered, when it describes itself, and digests of the system prompt and the tool set that
// turn was sent (see Record.PromptDigest). For a response no request produced (a middleware built
// it), there is no model, and the digests are of sent, the request the agent passed to the chain.
func (resp ModelResponse) journal(sent Request) (model *ModelInfo, prompt, tools string) {
	msgs, set := sent.Messages, sent.Tools
	if o := resp.origin; o.attempt != 0 {
		msgs, set = o.msgs, o.tools
		if o.described {
			info := o.info
			model = &info
		}
	}
	return model, PromptDigest(msgs), ToolsDigest(set)
}

// PromptDigest is the hex SHA-256 digest a model record journals of the system prompt a turn was
// sent: the text of every system message in msgs, in order, each length-prefixed under a domain
// tag, so no two different prompts share a digest. It is "" when msgs holds no system message.
func PromptDigest(msgs []Message) string {
	var b []byte
	for _, m := range msgs {
		if m.Role != RoleSystem {
			continue
		}
		if b == nil {
			b = append(make([]byte, 0, 512), "bide.prompt.v1\n"...)
		}
		b = appendField(b, m.Text())
	}
	if b == nil {
		return ""
	}
	return hexDigest(b)
}

// ToolsDigest is the hex SHA-256 digest a model record journals of the tool set a turn was sent:
// each tool's name, description and argument schema, length-prefixed under a domain tag, in the
// order of their names, so the digest does not depend on the order the tools were listed in. It
// is "" for no tools.
func ToolsDigest(tools []Tool) string {
	if len(tools) == 0 {
		return ""
	}
	byName := func(a, b Tool) int { return strings.Compare(a.Name(), b.Name()) }
	if !slices.IsSortedFunc(tools, byName) {
		tools = slices.Clone(tools)
		slices.SortStableFunc(tools, byName)
	}
	b := append(make([]byte, 0, 512), "bide.tools.v1\n"...)
	for _, t := range tools {
		b = appendField(b, t.Name())
		b = appendField(b, t.Description())
		b = appendField(b, t.ArgsSchema())
	}
	return hexDigest(b)
}

// appendField appends s to b as its decimal length, a colon, and s itself.
func appendField[S ~string | ~[]byte](b []byte, s S) []byte {
	b = strconv.AppendInt(b, int64(len(s)), 10)
	b = append(b, ':')
	return append(b, s...)
}

// hexDigest is the hex SHA-256 digest of b.
func hexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	var h [2 * sha256.Size]byte
	hex.Encode(h[:], sum[:])
	return string(h[:])
}

// usageTotals is what a run's model calls used: answer is the usage of the responses the run
// recorded, spend every request's, including requests whose responses were discarded (failed
// attempts, losing hedge targets) and model calls that failed.
type usageTotals struct {
	answer, spend Usage
}

// add counts the usage journaled in r: a model turn's response and discarded spend, a failed
// model call's spend, or a tool call's record carrying the usage of the runs it started.
func (t *usageTotals) add(r Record) {
	if r.Usage != nil {
		addUsage(&t.answer, *r.Usage)
		addUsage(&t.spend, *r.Usage)
	}
	if r.DiscardedUsage != nil {
		addUsage(&t.spend, *r.DiscardedUsage)
	}
}

// spendMeter collects the usage of the model requests a run sends until the run takes it to
// record. A request that ends after its turn was recorded (a hedge loser that outlived the race)
// is taken with the next turn.
type spendMeter struct {
	mu      sync.Mutex
	pending Usage
}

func (m *spendMeter) add(u Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	addUsage(&m.pending, u)
}

func (m *spendMeter) take() Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.pending
	m.pending = Usage{}
	return u
}

// discardedSpend is the part of spent beyond answer, the usage of the response a turn recorded,
// per field and never below zero: a response a middleware supplied without sending a request
// (a cache) may report usage no request spent.
func discardedSpend(spent, answer Usage) Usage {
	sub := func(a, b int) int { return max(a-b, 0) }
	return Usage{
		InputTokens:      sub(spent.InputTokens, answer.InputTokens),
		OutputTokens:     sub(spent.OutputTokens, answer.OutputTokens),
		CacheReadTokens:  sub(spent.CacheReadTokens, answer.CacheReadTokens),
		CacheWriteTokens: sub(spent.CacheWriteTokens, answer.CacheWriteTokens),
	}
}

// spendStepPrefix names the records that journal a failed model call's spend: "@spend/0",
// "@spend/1", and so on, numbered in the order the run wrote them.
const spendStepPrefix = "@spend/"

// recordSpend journals spent, the usage of a model call that failed, as the run's n-th spend
// record: a StepValue carrying it as DiscardedUsage. It is written even when ctx is cancelled,
// the common way a call fails, since the requests were billed either way. It returns the record
// the journal holds.
func (a *Agent) recordSpend(ctx context.Context, runID string, n int, spent Usage) (Record, error) {
	rec, err := a.store.Do(context.WithoutCancel(ctx), runID, spendStep(n), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, DiscardedUsage: &spent}, nil
	})
	if err != nil {
		return Record{}, fmt.Errorf("record spend (run %s): %w (%w)", runID, err, ErrStorage)
	}
	return rec, nil
}
